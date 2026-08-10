package backend

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

// lazyStateStore 延迟打开状态文件，保证 backend Mutex 先于任何状态事实读取。
type lazyStateStore struct {
	mu     sync.Mutex
	layout *config.Layout
	clock  func() time.Time
	store  *state.Store
	closed bool
}

func (s *lazyStateStore) ensure(ctx context.Context) (*state.Store, error) {
	if ctx == nil {
		return nil, errors.New("state store context is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, state.ErrClosed
	}
	if s.store != nil {
		return s.store, nil
	}
	store, err := state.NewStore(ctx, s.layout, state.WithClock(s.clock))
	if err != nil {
		return nil, err
	}
	s.store = store
	return store, nil
}

func (s *lazyStateStore) ReadEnvironment(ctx context.Context) (state.EnvironmentState, error) {
	store, err := s.ensure(ctx)
	if err != nil {
		return state.EnvironmentState{}, err
	}
	return store.ReadEnvironment(ctx)
}

func (s *lazyStateStore) ReadBackendTransaction(ctx context.Context) (Transaction, error) {
	store, err := s.ensure(ctx)
	if err != nil {
		return Transaction{}, err
	}
	snapshot, err := store.ReadTransaction(ctx, state.TransactionBackend)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return Transaction{}, ErrTransactionNotFound
		}
		return Transaction{}, err
	}
	value := snapshot.State()
	return Transaction{
		PID:     value.PID,
		Version: value.TargetVersion,
		Stage:   value.Stage,
		Handle:  &productionTransaction{snapshot: snapshot, snapshotValid: true, value: value},
	}, nil
}

func (s *lazyStateStore) BeginBackendTransaction(ctx context.Context, input TransactionInput) (TransactionHandle, error) {
	store, err := s.ensure(ctx)
	if err != nil {
		return nil, err
	}
	value, err := store.NewTransaction(state.TransactionBackend, state.TransactionInput{
		OperationID:   input.OperationID,
		Command:       "backend supervise",
		PID:           input.PID,
		TargetVersion: input.Version,
		Stage:         input.Stage,
	})
	if err != nil {
		return nil, err
	}
	transaction := &productionTransaction{value: value}
	if err := store.WriteTransaction(ctx, state.TransactionBackend, value); err != nil {
		return transaction, err
	}
	snapshot, err := store.ReadTransaction(ctx, state.TransactionBackend)
	if err != nil {
		return transaction, err
	}
	transaction.snapshot = snapshot
	transaction.snapshotValid = true
	return transaction, nil
}

func (s *lazyStateStore) UpdateBackendTransaction(ctx context.Context, handle TransactionHandle, stage protocol.Stage) error {
	transaction, ok := handle.(*productionTransaction)
	if !ok || transaction == nil {
		return errors.New("backend transaction handle is invalid")
	}
	store, err := s.ensure(ctx)
	if err != nil {
		return err
	}
	value := transaction.value
	value.Stage = stage
	transaction.value = value
	transaction.snapshotValid = false
	if err := store.WriteTransaction(ctx, state.TransactionBackend, value); err != nil {
		return err
	}
	snapshot, err := store.ReadTransaction(ctx, state.TransactionBackend)
	if err != nil {
		return err
	}
	transaction.snapshot = snapshot
	transaction.snapshotValid = true
	return nil
}

func (s *lazyStateStore) RemoveBackendTransaction(ctx context.Context, handle TransactionHandle) error {
	transaction, ok := handle.(*productionTransaction)
	if !ok || transaction == nil {
		return errors.New("backend transaction handle is invalid")
	}
	store, err := s.ensure(ctx)
	if err != nil {
		return err
	}
	if !transaction.snapshotValid {
		snapshot, readErr := store.ReadTransaction(ctx, state.TransactionBackend)
		if readErr != nil {
			if errors.Is(readErr, state.ErrNotFound) {
				return nil
			}
			return readErr
		}
		transaction.snapshot = snapshot
		transaction.snapshotValid = true
	}
	return store.RemoveTransaction(ctx, transaction.snapshot)
}

func (s *lazyStateStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.store == nil {
		return nil
	}
	return s.store.Close()
}

type productionTransaction struct {
	snapshot      state.TransactionSnapshot
	snapshotValid bool
	value         state.TransactionState
}
