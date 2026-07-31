package filesystem

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

const (
	stateIntentVersion           = 1
	maxStateIntentBytes    int64 = 64 * 1024
	stateGateRetryDelay          = 10 * time.Millisecond
	stateIntentNonceLength       = 32

	ntFileSupersede        = uint32(0x00000000)
	ntFileSynchronousAlert = uint32(0x00000010)
	ntFileDeleteOnClose    = uint32(0x00001000)
)

type stateFileDependencies struct {
	api       pathAPI
	waitGate  WaitFunc
	fillNonce func([]byte) error
}

type stateGuardMode uint8

const (
	stateGuardShared stateGuardMode = iota + 1
	stateGuardExclusive
)

type stateIdentityProof struct {
	VolumeSerial uint64   `json:"volumeSerial"`
	FileID       [16]byte `json:"fileId"`
}

type stateObjectProof struct {
	stateIdentityProof
	Size   int64    `json:"size"`
	Digest [32]byte `json:"digest"`
}

type stateIntent struct {
	Version         int                `json:"version"`
	Kind            StateFileKind      `json:"kind"`
	DestinationLeaf string             `json:"destinationLeaf"`
	IntentLeaf      string             `json:"intentLeaf"`
	TempLeaf        string             `json:"tempLeaf"`
	BackupLeaf      string             `json:"backupLeaf"`
	Nonce           string             `json:"nonce"`
	Root            stateIdentityProof `json:"root"`
	IntentObject    stateIdentityProof `json:"intentObject"`
	Old             stateObjectProof   `json:"old"`
	New             stateObjectProof   `json:"new"`
}

type stateIntentEnvelope struct {
	Intent stateIntent `json:"intent"`
	Seal   [32]byte    `json:"seal"`
}

type stateCandidate struct {
	leaf    string
	object  pinnedObject
	payload []byte
	digest  [32]byte
	present bool
}

type stateUnlinkMutationError struct {
	dispositionErr error
	closeErr       error
}

func (e *stateUnlinkMutationError) Error() string {
	return "state unlink mutation failed"
}

func (e *stateUnlinkMutationError) Unwrap() []error {
	causes := make([]error, 0, 2)
	if e.dispositionErr != nil {
		causes = append(causes, e.dispositionErr)
	}
	if e.closeErr != nil {
		causes = append(causes, e.closeErr)
	}
	return causes
}

// NewStateFiles 创建固定并验证 Runtime state 目录的能力。
func NewStateFiles(ctx context.Context, layout *config.Layout) (*StateFiles, error) {
	return newStateFilesWithDependencies(
		ctx,
		layout,
		stateFileDependencies{
			api:       newProductionPathAPI(),
			waitGate:  defaultStateGateWait,
			fillNonce: fillCryptoNonce,
		},
	)
}

func fillCryptoNonce(buffer []byte) error {
	n, err := rand.Read(buffer)
	if err != nil {
		return err
	}
	if n != len(buffer) {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func newStateFilesWithDependencies(
	ctx context.Context,
	layout *config.Layout,
	dependencies stateFileDependencies,
) (*StateFiles, error) {
	if ctx == nil || layout == nil || !dependencies.api.valid() ||
		dependencies.waitGate == nil || dependencies.fillNonce == nil {
		return nil, fmt.Errorf("%w: invalid state-file dependencies", ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	api := dependencies.api
	appPath, err := canonicalizeContextWith(ctx, layout.AppRoot(), api)
	if err != nil {
		return nil, err
	}
	parentPath, err := canonicalizeContextWith(ctx, filepath.Dir(layout.AppRoot()), api)
	if err != nil {
		return nil, err
	}
	appChain, err := openPinnedChainWith(ctx, parentPath, appPath, directoryPinSpec(), api)
	if err != nil {
		return nil, err
	}
	app, err := detachLeaf(appChain)
	if err != nil {
		return nil, err
	}
	originals := []pinnedObject{app}
	failOriginals := func(operationErr error) (*StateFiles, error) {
		return nil, errors.Join(operationErr, closePinnedObjects(api, originals))
	}

	if err := ctx.Err(); err != nil {
		return failOriginals(err)
	}
	statePath, err := canonicalizeContextWith(ctx, layout.StateDir(), api)
	if err != nil {
		return failOriginals(err)
	}
	stateRoot, err := openOrCreatePinnedDirectory(ctx, app, statePath, api)
	if err != nil {
		return failOriginals(err)
	}
	originals = append(originals, stateRoot)

	duplicates := make([]pinnedObject, 0, len(originals))
	failDuplicates := func(operationErr error) (*StateFiles, error) {
		return nil, errors.Join(
			operationErr,
			closePinnedObjects(api, duplicates),
			closePinnedObjects(api, originals),
		)
	}
	for i := range originals {
		if err := ctx.Err(); err != nil {
			return failDuplicates(err)
		}
		duplicate, err := duplicatePinnedDirectory(ctx, originals[i], api)
		if err != nil {
			return failDuplicates(err)
		}
		duplicates = append(duplicates, duplicate)
	}
	if err := validateParentIdentityWith(ctx, duplicates[0], duplicates[1], api); err != nil {
		return failDuplicates(err)
	}
	if err := closePinnedObjects(api, originals); err != nil {
		return nil, errors.Join(err, closePinnedObjects(api, duplicates))
	}
	return &StateFiles{
		layout:      layout,
		api:         api,
		waitGate:    dependencies.waitGate,
		fillNonce:   dependencies.fillNonce,
		owner:       &stateFileOwner{marker: 1},
		pins:        [2]pinnedObject{duplicates[0], duplicates[1]},
		probePassed: make(map[StateFileKind]bool, 4),
	}, nil
}

func duplicatePinnedDirectory(
	ctx context.Context,
	source pinnedObject,
	api pathAPI,
) (pinnedObject, error) {
	duplicate, err := api.duplicateHandle(source.handle)
	if err != nil {
		return pinnedObject{}, &FileError{
			Operation: "duplicate",
			Path:      source.path.String(),
			Err:       err,
		}
	}
	closeOnFailure := func(operationErr error) (pinnedObject, error) {
		return pinnedObject{}, errors.Join(
			operationErr,
			wrapFileError("close", source.path.String(), api.closeHandle(duplicate)),
		)
	}
	identity, err := api.identity(duplicate)
	if err != nil {
		return closeOnFailure(&FileError{
			Operation: "identify-duplicate",
			Path:      source.path.String(),
			Err:       err,
		})
	}
	if !sameObjectIdentity(identity, source.identity) {
		return closeOnFailure(&FileError{
			Operation: "verify-duplicate",
			Path:      source.path.String(),
			Err:       ErrIdentityChanged,
		})
	}
	finalPath, err := api.finalPath(duplicate)
	if err != nil {
		return closeOnFailure(&FileError{
			Operation: "final-path",
			Path:      source.path.String(),
			Err:       err,
		})
	}
	canonical, err := canonicalizeContextWith(ctx, finalPath, api)
	if err != nil {
		return closeOnFailure(err)
	}
	if !canonical.Equal(source.path) {
		return closeOnFailure(&FileError{
			Operation: "verify-duplicate",
			Path:      source.path.String(),
			Err:       ErrIdentityChanged,
		})
	}
	result := pinnedObject{path: canonical, handle: duplicate, identity: identity}
	if err := validatePinnedDirectory(ctx, result, api); err != nil {
		return closeOnFailure(err)
	}
	return result, nil
}

func sameObjectIdentity(left, right objectIdentity) bool {
	return left.volumeSerial == right.volumeSerial &&
		left.fileID == right.fileID &&
		left.attributes == right.attributes &&
		left.numberOfLinks == right.numberOfLinks &&
		left.size == right.size
}

func defaultStateGateWait(ctx context.Context, delay time.Duration) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is nil", ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Read 在持久 shared guard 内选择权威对象，并在释放 guard 后有界读取。
func (f *StateFiles) Read(
	ctx context.Context,
	kind StateFileKind,
	maxBytes int64,
) (StateFileSnapshot, error) {
	if ctx == nil {
		return StateFileSnapshot{}, fmt.Errorf("%w: context is nil", ErrInvalidArgument)
	}
	if !kind.Valid() || maxBytes <= 0 || maxBytes > MaxStateFileBytes {
		return StateFileSnapshot{}, fmt.Errorf("%w: invalid state read", ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return StateFileSnapshot{}, err
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.closed {
		return StateFileSnapshot{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return StateFileSnapshot{}, err
	}
	return f.readLocked(ctx, kind, maxBytes)
}

// WriteAtomic 在持久 exclusive guard 内以 sealed intent 和 no-replace rename 发布状态。
func (f *StateFiles) WriteAtomic(
	ctx context.Context,
	kind StateFileKind,
	payload []byte,
) (WriteAtomicResult, error) {
	if ctx == nil {
		return WriteAtomicResult{}, fmt.Errorf("%w: context is nil", ErrInvalidArgument)
	}
	if !kind.Valid() || len(payload) == 0 || int64(len(payload)) > MaxStateFileBytes {
		return WriteAtomicResult{}, fmt.Errorf("%w: invalid state write", ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return WriteAtomicResult{}, err
	}
	payload = append([]byte(nil), payload...)

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return WriteAtomicResult{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return WriteAtomicResult{}, err
	}

	guard, err := f.acquireStateGuard(ctx, kind, stateGuardExclusive)
	if err != nil {
		recoveryRequired := !errors.Is(err, context.Canceled) &&
			!errors.Is(err, context.DeadlineExceeded)
		return stateWriteFailure(
			StateWritePhaseRecover,
			false,
			recoveryRequired,
			err,
			nil,
		)
	}
	if err := f.ensurePOSIXUnlinkCapability(ctx, kind); err != nil {
		closeErr := f.closeStateObject(&guard)
		return stateWriteFailure(
			StateWritePhaseRecover,
			false,
			true,
			err,
			closeErr,
		)
	}
	if err := f.recoverStateNamespace(ctx, kind); err != nil {
		closeErr := f.closeStateObject(&guard)
		return stateWriteFailure(
			StateWritePhaseRecover,
			false,
			true,
			err,
			closeErr,
		)
	}
	return f.writeStateLocked(ctx, kind, payload, guard)
}

// RemoveTransactionIfUnchanged 仅在 snapshot 仍绑定当前正式对象时删除它。
func (f *StateFiles) RemoveTransactionIfUnchanged(
	ctx context.Context,
	snapshot StateFileSnapshot,
) (StateRemoveResult, error) {
	if ctx == nil {
		return StateRemoveResult{}, fmt.Errorf("%w: context is nil", ErrInvalidArgument)
	}
	if err := f.validateStateRemoveSnapshot(snapshot); err != nil {
		return StateRemoveResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return StateRemoveResult{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return StateRemoveResult{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return StateRemoveResult{}, err
	}

	guard, err := f.acquireStateGuard(ctx, snapshot.kind, stateGuardExclusive)
	if err != nil {
		recoveryRequired := !errors.Is(err, context.Canceled) &&
			!errors.Is(err, context.DeadlineExceeded)
		return stateRemoveFailure(recoveryRequired, err, nil)
	}
	return f.removeStateLocked(ctx, snapshot, guard)
}

func (f *StateFiles) validateStateRemoveSnapshot(snapshot StateFileSnapshot) error {
	if snapshot.owner == nil ||
		snapshot.owner != f.owner ||
		!snapshot.kind.Valid() ||
		snapshot.kind == StateEnvironment ||
		snapshot.size <= 0 ||
		snapshot.size != int64(len(snapshot.bytes)) ||
		snapshot.digest != sha256.Sum256(snapshot.bytes) {
		return ErrInvalidToken
	}
	return nil
}

func (f *StateFiles) removeStateLocked(
	ctx context.Context,
	snapshot StateFileSnapshot,
	guard pinnedObject,
) (StateRemoveResult, error) {
	intentLeaf := stateIntentLeaf(snapshot.kind)
	intent, intentMissing, err := f.openStateMutationLeaf(ctx, intentLeaf)
	if err != nil {
		closeErr := f.closeStateObject(&guard)
		return stateRemoveFailure(true, err, closeErr)
	}
	if !intentMissing {
		if closeErr := f.closeStateObject(&intent); closeErr != nil {
			return stateRemoveFailure(
				true,
				f.stateRecoveryError(intent.path.String(), closeErr),
				f.closeStateObject(&guard),
			)
		}
		if err := f.ensurePOSIXUnlinkCapability(ctx, snapshot.kind); err != nil {
			return stateRemoveFailure(true, err, f.closeStateObject(&guard))
		}
		if err := f.recoverStateNamespace(ctx, snapshot.kind); err != nil {
			return stateRemoveFailure(true, err, f.closeStateObject(&guard))
		}
	}

	destinationPath, err := f.statePath(snapshot.kind)
	if err != nil {
		return stateRemoveFailure(true, err, f.closeStateObject(&guard))
	}
	destinationLeaf := filepath.Base(destinationPath)
	destination, err := f.inspectStateMutationCandidate(ctx, destinationLeaf)
	if err != nil {
		return stateRemoveFailure(true, err, f.closeStateObject(&guard))
	}
	if !destination.present {
		closeErr := f.closeStateObject(&guard)
		if closeErr != nil {
			return stateRemoveFailure(true, nil, closeErr)
		}
		return StateRemoveResult{}, nil
	}
	if !snapshotMatchesStateCandidate(snapshot, destination) {
		closeErr := errors.Join(
			f.closeCandidate(&destination),
			f.closeStateObject(&guard),
		)
		if closeErr != nil {
			return stateRemoveFailure(true, ErrIdentityChanged, closeErr)
		}
		return StateRemoveResult{}, ErrIdentityChanged
	}
	if err := ctx.Err(); err != nil {
		closeErr := errors.Join(
			f.closeCandidate(&destination),
			f.closeStateObject(&guard),
		)
		if closeErr != nil {
			return stateRemoveFailure(true, err, closeErr)
		}
		return StateRemoveResult{}, err
	}
	if err := f.ensurePOSIXUnlinkCapability(ctx, snapshot.kind); err != nil {
		closeErr := errors.Join(
			f.closeCandidate(&destination),
			f.closeStateObject(&guard),
		)
		if closeErr != nil {
			return stateRemoveFailure(true, err, closeErr)
		}
		return StateRemoveResult{}, err
	}
	if err := ctx.Err(); err != nil {
		closeErr := errors.Join(
			f.closeCandidate(&destination),
			f.closeStateObject(&guard),
		)
		if closeErr != nil {
			return stateRemoveFailure(true, err, closeErr)
		}
		return StateRemoveResult{}, err
	}

	proof := stateObjectProof{
		stateIdentityProof: proofIdentityValue(destination.object.identity),
		Size:               destination.object.identity.size,
		Digest:             destination.digest,
	}
	unlinkErr := f.unlinkStateObject(
		context.WithoutCancel(ctx),
		destinationLeaf,
		&destination.object,
		&proof,
	)
	if unlinkErr != nil {
		knownNotApplied := false
		var classificationErr error
		var mutationErr *stateUnlinkMutationError
		if errors.As(unlinkErr, &mutationErr) &&
			mutationErr.dispositionErr != nil &&
			mutationErr.closeErr == nil {
			knownNotApplied, classificationErr = f.stateUnlinkKnownNotApplied(
				context.WithoutCancel(ctx),
				destinationLeaf,
				snapshot,
				&destination.object,
				&proof,
			)
		}
		closeErr := errors.Join(
			classificationErr,
			f.closeCandidate(&destination),
			f.closeStateObject(&guard),
		)
		recoveryRequired := !knownNotApplied || closeErr != nil
		return stateRemoveFailure(recoveryRequired, unlinkErr, closeErr)
	}
	closeErr := errors.Join(
		f.closeCandidate(&destination),
		f.closeStateObject(&guard),
	)
	if closeErr != nil {
		return StateRemoveResult{
				MutationApplied:  true,
				RecoveryRequired: true,
			},
			&StateRemoveError{CleanupError: closeErr}
	}
	return StateRemoveResult{MutationApplied: true}, nil
}

func (f *StateFiles) stateUnlinkKnownNotApplied(
	ctx context.Context,
	leaf string,
	snapshot StateFileSnapshot,
	anchor *pinnedObject,
	proof *stateObjectProof,
) (bool, error) {
	if err := f.verifyStateAnchorReadable(ctx, anchor, proof); err != nil {
		return false, err
	}
	current, err := f.inspectStateMutationCandidate(ctx, leaf)
	if err != nil || !current.present {
		return false, err
	}
	matches := current.object.identity.volumeSerial == anchor.identity.volumeSerial &&
		current.object.identity.fileID == anchor.identity.fileID &&
		snapshotMatchesStateCandidate(snapshot, current)
	closeErr := f.closeCandidate(&current)
	if !matches {
		return false, errors.Join(ErrIdentityChanged, closeErr)
	}
	if closeErr != nil {
		return false, closeErr
	}
	return true, nil
}

func snapshotMatchesStateCandidate(
	snapshot StateFileSnapshot,
	candidate stateCandidate,
) bool {
	return candidate.present &&
		snapshot.volumeSerial == candidate.object.identity.volumeSerial &&
		snapshot.fileID == candidate.object.identity.fileID &&
		snapshot.size == candidate.object.identity.size &&
		snapshot.digest == candidate.digest &&
		bytes.Equal(snapshot.bytes, candidate.payload)
}

func stateRemoveFailure(
	recoveryRequired bool,
	cause error,
	cleanupErr error,
) (StateRemoveResult, error) {
	result := StateRemoveResult{RecoveryRequired: recoveryRequired}
	if cleanupErr == nil {
		return result, cause
	}
	return result, &StateRemoveError{
		Cause:        cause,
		CleanupError: cleanupErr,
	}
}

func (f *StateFiles) readLocked(
	ctx context.Context,
	kind StateFileKind,
	maxBytes int64,
) (StateFileSnapshot, error) {
	guard, err := f.acquireStateGuard(ctx, kind, stateGuardShared)
	if err != nil {
		return StateFileSnapshot{}, err
	}
	destinationPath, err := f.statePath(kind)
	if err != nil {
		return StateFileSnapshot{}, errors.Join(err, f.closeStateObject(&guard))
	}
	destinationLeaf := filepath.Base(destinationPath)
	intentLeaf := stateIntentLeaf(kind)
	intentObject, intentMissing, err := f.openStateLeaf(ctx, intentLeaf, statePayloadReadSpec())
	if err != nil {
		return StateFileSnapshot{}, errors.Join(err, f.closeStateObject(&guard))
	}
	if intentMissing {
		return f.readWithoutIntent(ctx, kind, destinationPath, destinationLeaf, maxBytes, guard)
	}
	return f.readWithIntent(
		ctx,
		kind,
		destinationPath,
		destinationLeaf,
		intentLeaf,
		maxBytes,
		guard,
		intentObject,
	)
}

func (f *StateFiles) readWithoutIntent(
	ctx context.Context,
	kind StateFileKind,
	destinationPath string,
	destinationLeaf string,
	maxBytes int64,
	guard pinnedObject,
) (StateFileSnapshot, error) {
	payload, missing, err := f.openStateLeaf(ctx, destinationLeaf, statePayloadReadSpec())
	if err != nil {
		return StateFileSnapshot{}, errors.Join(err, f.closeStateObject(&guard))
	}
	if missing {
		if closeErr := f.closeStateObject(&guard); closeErr != nil {
			return StateFileSnapshot{}, closeErr
		}
		return StateFileSnapshot{}, &stateFileNotFoundError{
			kind: kind,
			path: destinationPath,
		}
	}
	if err := f.validateStatePayloadMetadata(payload, maxBytes, nil); err != nil {
		return StateFileSnapshot{}, errors.Join(
			err,
			f.closeStateObject(&payload),
			f.closeStateObject(&guard),
		)
	}
	if err := f.closeStateObject(&guard); err != nil {
		return StateFileSnapshot{}, errors.Join(err, f.closeStateObject(&payload))
	}
	return f.snapshotFromPayload(ctx, kind, payload, maxBytes, nil)
}

func (f *StateFiles) readWithIntent(
	ctx context.Context,
	kind StateFileKind,
	destinationPath string,
	destinationLeaf string,
	intentLeaf string,
	maxBytes int64,
	guard pinnedObject,
	intentObject pinnedObject,
) (StateFileSnapshot, error) {
	intentBytes, _, err := f.readPinnedBytes(ctx, intentObject, maxStateIntentBytes)
	if err != nil {
		return StateFileSnapshot{}, errors.Join(
			f.stateRecoveryError(intentObject.path.String(), err),
			f.closeStateObject(&intentObject),
			f.closeStateObject(&guard),
		)
	}
	intent, err := decodeStateIntent(intentBytes)
	if err != nil {
		return StateFileSnapshot{}, errors.Join(
			f.stateRecoveryError(intentObject.path.String(), err),
			f.closeStateObject(&intentObject),
			f.closeStateObject(&guard),
		)
	}
	if err := f.validateIntentBinding(
		kind,
		destinationLeaf,
		intentLeaf,
		intentObject,
		intent,
	); err != nil {
		return StateFileSnapshot{}, errors.Join(
			err,
			f.closeStateObject(&intentObject),
			f.closeStateObject(&guard),
		)
	}

	destination, err := f.inspectStateCandidate(ctx, destinationLeaf)
	if err != nil {
		return StateFileSnapshot{}, errors.Join(
			err,
			f.closeStateObject(&intentObject),
			f.closeStateObject(&guard),
		)
	}
	backup, err := f.inspectStateCandidate(ctx, intent.BackupLeaf)
	if err != nil {
		return StateFileSnapshot{}, errors.Join(
			err,
			f.closeCandidate(&destination),
			f.closeStateObject(&intentObject),
			f.closeStateObject(&guard),
		)
	}
	temp, err := f.inspectStateCandidate(ctx, intent.TempLeaf)
	if err != nil {
		return StateFileSnapshot{}, errors.Join(
			err,
			f.closeCandidate(&backup),
			f.closeCandidate(&destination),
			f.closeStateObject(&intentObject),
			f.closeStateObject(&guard),
		)
	}
	authorityLeaf, authorityProof, err := selectStateAuthority(intent, destination, backup, temp)
	if err != nil {
		return StateFileSnapshot{}, errors.Join(
			f.stateRecoveryError(destinationPath, err),
			f.closeCandidate(&temp),
			f.closeCandidate(&backup),
			f.closeCandidate(&destination),
			f.closeStateObject(&intentObject),
			f.closeStateObject(&guard),
		)
	}
	authority, missing, err := f.openStateLeaf(ctx, authorityLeaf, statePayloadReadSpec())
	if err != nil || missing {
		if err == nil {
			err = windows.ERROR_FILE_NOT_FOUND
		}
		return StateFileSnapshot{}, errors.Join(
			f.stateRecoveryError(filepath.Join(f.pins[1].path.String(), authorityLeaf), err),
			f.closeCandidate(&temp),
			f.closeCandidate(&backup),
			f.closeCandidate(&destination),
			f.closeStateObject(&intentObject),
			f.closeStateObject(&guard),
		)
	}
	if err := f.validateStatePayloadMetadata(authority, maxBytes, &authorityProof); err != nil {
		return StateFileSnapshot{}, errors.Join(
			err,
			f.closeStateObject(&authority),
			f.closeCandidate(&temp),
			f.closeCandidate(&backup),
			f.closeCandidate(&destination),
			f.closeStateObject(&intentObject),
			f.closeStateObject(&guard),
		)
	}
	proofCloseErr := errors.Join(
		f.closeCandidate(&temp),
		f.closeCandidate(&backup),
		f.closeCandidate(&destination),
		f.closeStateObject(&intentObject),
	)
	if proofCloseErr != nil {
		return StateFileSnapshot{}, errors.Join(
			proofCloseErr,
			f.closeStateObject(&authority),
			f.closeStateObject(&guard),
		)
	}
	if err := f.closeStateObject(&guard); err != nil {
		return StateFileSnapshot{}, errors.Join(err, f.closeStateObject(&authority))
	}
	return f.snapshotFromPayload(ctx, kind, authority, maxBytes, &authorityProof)
}

func (f *StateFiles) statePath(kind StateFileKind) (string, error) {
	switch kind {
	case StateBackend:
		return f.layout.BackendStateFile(), nil
	case StateMutation:
		return f.layout.MutationStateFile(), nil
	case StateUpdate:
		return f.layout.UpdateStateFile(), nil
	case StateEnvironment:
		return f.layout.EnvironmentStateFile(), nil
	default:
		return "", fmt.Errorf("%w: invalid state kind", ErrInvalidArgument)
	}
}

func stateGuardLeaf(kind StateFileKind) string {
	return fmt.Sprintf(".%s.guard", kind)
}

func stateIntentLeaf(kind StateFileKind) string {
	return fmt.Sprintf(".%s.intent", kind)
}

func stateTransactionLeaf(kind StateFileKind, role string, nonce string) string {
	return fmt.Sprintf(".%s.%s.%s", kind, role, nonce)
}

func statePayloadReadSpec() openSpec {
	return openSpec{
		access:    windows.FILE_READ_DATA | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE,
		share:     windows.FILE_SHARE_READ | windows.FILE_SHARE_DELETE,
		creation:  windows.OPEN_EXISTING,
		options:   windows.FILE_FLAG_OPEN_REPARSE_POINT,
		directory: false,
	}
}

func stateGuardNTSpec(mode stateGuardMode, disposition uint32) (ntCreateSpec, error) {
	share := uint32(0)
	switch mode {
	case stateGuardShared:
		share = windows.FILE_SHARE_READ
	case stateGuardExclusive:
	default:
		return ntCreateSpec{}, fmt.Errorf("%w: invalid state guard mode", ErrInvalidArgument)
	}
	spec := ntCreateSpec{
		desiredAccess:     windows.FILE_READ_DATA | windows.FILE_READ_ATTRIBUTES,
		shareAccess:       share,
		createDisposition: disposition,
		createOptions:     ntFileOpenReparsePoint | ntFileNonDirectoryFile,
	}
	if err := validateStateGuardNTSpec(mode, disposition, spec); err != nil {
		return ntCreateSpec{}, err
	}
	return spec, nil
}

func validateStateGuardNTSpec(
	mode stateGuardMode,
	disposition uint32,
	spec ntCreateSpec,
) error {
	wantShare := uint32(0)
	switch mode {
	case stateGuardShared:
		wantShare = windows.FILE_SHARE_READ
	case stateGuardExclusive:
	default:
		return fmt.Errorf("%w: invalid state guard mode", ErrInvalidArgument)
	}
	if spec.desiredAccess != windows.FILE_READ_DATA|windows.FILE_READ_ATTRIBUTES ||
		spec.shareAccess != wantShare ||
		(disposition != ntFileCreate && disposition != ntFileOpen) ||
		spec.createDisposition != disposition ||
		spec.createOptions != ntFileOpenReparsePoint|ntFileNonDirectoryFile {
		return fmt.Errorf("%w: invalid state guard native parameters", ErrInvalidArgument)
	}
	return nil
}

func (f *StateFiles) acquireStateGuard(
	ctx context.Context,
	kind StateFileKind,
	mode stateGuardMode,
) (pinnedObject, error) {
	if ctx == nil || !kind.Valid() {
		return pinnedObject{}, fmt.Errorf("%w: invalid state guard request", ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return pinnedObject{}, err
	}
	leaf := stateGuardLeaf(kind)
	path := filepath.Join(f.pins[1].path.String(), leaf)
	createSpec, err := stateGuardNTSpec(mode, ntFileCreate)
	if err != nil {
		return pinnedObject{}, err
	}
	handle, err := f.api.ntCreateRelative(f.pins[1].handle, leaf, createSpec)
	if err == nil {
		return f.validateStateGuardHandle(ctx, kind, handle)
	}
	pendingOperation := "guard-create"
	pendingErr := err
	if !isStateGuardCollision(err) {
		if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			if err := f.waitGate(ctx, stateGateRetryDelay); err != nil {
				return pinnedObject{}, stateGuardWaitError(pendingOperation, path, pendingErr, err)
			}
		} else {
			return pinnedObject{}, &FileError{
				Operation: pendingOperation,
				Path:      path,
				Err:       err,
			}
		}
	}
	openSpec, err := stateGuardNTSpec(mode, ntFileOpen)
	if err != nil {
		return pinnedObject{}, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return pinnedObject{}, stateGuardWaitError(pendingOperation, path, pendingErr, err)
		}
		handle, err = f.api.ntCreateRelative(f.pins[1].handle, leaf, openSpec)
		if err == nil {
			return f.validateStateGuardHandle(ctx, kind, handle)
		}
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return pinnedObject{}, &FileError{
				Operation: "guard-open",
				Path:      path,
				Err:       err,
			}
		}
		pendingOperation = "guard-open"
		pendingErr = err
		if err := f.waitGate(ctx, stateGateRetryDelay); err != nil {
			return pinnedObject{}, stateGuardWaitError(pendingOperation, path, pendingErr, err)
		}
	}
}

func stateGuardWaitError(operation, path string, cause, waitErr error) error {
	return errors.Join(
		&FileError{Operation: operation, Path: path, Err: cause},
		waitErr,
	)
}

func isStateGuardCollision(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_EXISTS) ||
		errors.Is(err, windows.ERROR_ALREADY_EXISTS)
}

func (f *StateFiles) validateStateGuardHandle(
	ctx context.Context,
	kind StateFileKind,
	handle windows.Handle,
) (pinnedObject, error) {
	leaf := stateGuardLeaf(kind)
	path := filepath.Join(f.pins[1].path.String(), leaf)
	closeOnFailure := func(operationErr error) (pinnedObject, error) {
		return pinnedObject{}, errors.Join(
			operationErr,
			wrapFileError("close", path, f.api.closeHandle(handle)),
		)
	}
	identity, err := f.api.identity(handle)
	if err != nil {
		return closeOnFailure(&FileError{Operation: "guard-identify", Path: path, Err: err})
	}
	if err := validateOrdinaryStateIdentity(identity, f.pins[1].identity.volumeSerial); err != nil {
		return closeOnFailure(&FileError{Operation: "guard-identity", Path: path, Err: err})
	}
	finalPath, err := f.api.finalPath(handle)
	if err != nil {
		return closeOnFailure(&FileError{Operation: "guard-final-path", Path: path, Err: err})
	}
	canonical, err := canonicalizeContextWith(ctx, finalPath, f.api)
	if err != nil {
		return closeOnFailure(err)
	}
	expected, err := canonicalizeContextWith(ctx, path, f.api)
	if err != nil {
		return closeOnFailure(err)
	}
	if !canonical.Equal(expected) {
		return closeOnFailure(&FileError{Operation: "guard-path", Path: path, Err: ErrIdentityChanged})
	}
	object := pinnedObject{path: canonical, handle: handle, identity: identity}
	if err := validateParentIdentityWith(ctx, f.pins[1], object, f.api); err != nil {
		return closeOnFailure(err)
	}
	duplicate, err := f.api.duplicateHandle(handle)
	if err != nil {
		return closeOnFailure(&FileError{Operation: "guard-duplicate", Path: path, Err: err})
	}
	duplicateIdentity, identityErr := f.api.identity(duplicate)
	duplicateCloseErr := f.api.closeHandle(duplicate)
	if identityErr != nil || duplicateCloseErr != nil {
		return closeOnFailure(errors.Join(
			wrapFileError("guard-identify-duplicate", path, identityErr),
			wrapFileError("close", path, duplicateCloseErr),
		))
	}
	if !sameObjectIdentity(identity, duplicateIdentity) {
		return closeOnFailure(&FileError{Operation: "guard-identity", Path: path, Err: ErrIdentityChanged})
	}
	return object, nil
}

func validateOrdinaryStateIdentity(identity objectIdentity, volumeSerial uint64) error {
	if identity.attributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return ErrIdentityChanged
	}
	if identity.numberOfLinks != 1 {
		return ErrUnsafeHardLink
	}
	if identity.volumeSerial != volumeSerial || identity.fileID == ([16]byte{}) {
		return ErrIdentityChanged
	}
	return nil
}

func (f *StateFiles) openStateLeaf(
	ctx context.Context,
	leaf string,
	spec openSpec,
) (pinnedObject, bool, error) {
	if err := validateStateLeafName(leaf); err != nil {
		return pinnedObject{}, false, err
	}
	object, err := openRelativeCheckedWith(ctx, f.pins[1], leaf, spec, f.api)
	if isInitialStateLeafMissing(err) {
		return pinnedObject{}, true, nil
	}
	if err != nil {
		return pinnedObject{}, false, err
	}
	expected, err := canonicalizeContextWith(
		ctx,
		filepath.Join(f.pins[1].path.String(), leaf),
		f.api,
	)
	if err != nil {
		return pinnedObject{}, false, errors.Join(err, f.closeStateObject(&object))
	}
	if !object.path.Equal(expected) {
		return pinnedObject{}, false, errors.Join(
			&FileError{Operation: "state-leaf-path", Path: expected.String(), Err: ErrIdentityChanged},
			f.closeStateObject(&object),
		)
	}
	if err := validateOrdinaryStateIdentity(object.identity, f.pins[1].identity.volumeSerial); err != nil {
		return pinnedObject{}, false, errors.Join(
			&FileError{Operation: "state-leaf-identity", Path: object.path.String(), Err: err},
			f.closeStateObject(&object),
		)
	}
	return object, false, nil
}

func isInitialStateLeafMissing(err error) bool {
	var fileErr *FileError
	return errors.As(err, &fileErr) &&
		fileErr.Operation == "open-relative" &&
		errors.Is(fileErr.Err, windows.ERROR_FILE_NOT_FOUND)
}

func validateStateLeafName(leaf string) error {
	if leaf == "" || filepath.Base(leaf) != leaf {
		return fmt.Errorf("%w: invalid state leaf", ErrInvalidArgument)
	}
	if err := validateNonexistentComponent(leaf); err != nil {
		return err
	}
	return nil
}

func (f *StateFiles) inspectStateCandidate(
	ctx context.Context,
	leaf string,
) (stateCandidate, error) {
	object, missing, err := f.openStateLeaf(ctx, leaf, statePayloadReadSpec())
	if err != nil {
		return stateCandidate{}, f.stateRecoveryError(
			filepath.Join(f.pins[1].path.String(), leaf),
			err,
		)
	}
	if missing {
		return stateCandidate{leaf: leaf}, nil
	}
	payload, digest, err := f.readPinnedBytes(ctx, object, MaxStateFileBytes)
	if err != nil {
		return stateCandidate{}, errors.Join(
			f.stateRecoveryError(object.path.String(), err),
			f.closeStateObject(&object),
		)
	}
	return stateCandidate{
		leaf:    leaf,
		object:  object,
		payload: payload,
		digest:  digest,
		present: true,
	}, nil
}

func (c stateCandidate) matches(proof stateObjectProof) bool {
	return c.present &&
		c.object.identity.volumeSerial == proof.VolumeSerial &&
		c.object.identity.fileID == proof.FileID &&
		c.object.identity.size == proof.Size &&
		c.digest == proof.Digest
}

func selectStateAuthority(
	intent stateIntent,
	destination stateCandidate,
	backup stateCandidate,
	temp stateCandidate,
) (string, stateObjectProof, error) {
	if backup.present && !backup.matches(intent.Old) {
		return "", stateObjectProof{}, errors.Join(ErrStateRecoveryRequired, ErrIdentityChanged)
	}
	if temp.present && !temp.matches(intent.New) {
		return "", stateObjectProof{}, errors.Join(ErrStateRecoveryRequired, ErrIdentityChanged)
	}
	switch {
	case destination.matches(intent.Old):
		if backup.present {
			return "", stateObjectProof{}, ErrStateRecoveryRequired
		}
		return destination.leaf, intent.Old, nil
	case !destination.present:
		if backup.matches(intent.Old) && temp.matches(intent.New) {
			return backup.leaf, intent.Old, nil
		}
		return "", stateObjectProof{}, ErrStateRecoveryRequired
	case destination.matches(intent.New):
		if temp.present {
			return "", stateObjectProof{}, ErrStateRecoveryRequired
		}
		if backup.present && !backup.matches(intent.Old) {
			return "", stateObjectProof{}, ErrStateRecoveryRequired
		}
		return destination.leaf, intent.New, nil
	default:
		if backup.matches(intent.Old) && temp.matches(intent.New) {
			return backup.leaf, intent.Old, nil
		}
		return "", stateObjectProof{}, errors.Join(ErrStateRecoveryRequired, ErrIdentityChanged)
	}
}

func (f *StateFiles) validateIntentBinding(
	kind StateFileKind,
	destinationLeaf string,
	intentLeaf string,
	intentObject pinnedObject,
	intent stateIntent,
) error {
	if intent.Version != stateIntentVersion ||
		intent.Kind != kind ||
		intent.DestinationLeaf != destinationLeaf ||
		intent.IntentLeaf != intentLeaf ||
		intent.Root.VolumeSerial != f.pins[1].identity.volumeSerial ||
		intent.Root.FileID != f.pins[1].identity.fileID ||
		intent.IntentObject.VolumeSerial != intentObject.identity.volumeSerial ||
		intent.IntentObject.FileID != intentObject.identity.fileID {
		return f.stateRecoveryError(intentObject.path.String(), ErrIdentityChanged)
	}
	if err := validateStateLeafName(intent.TempLeaf); err != nil {
		return f.stateRecoveryError(intentObject.path.String(), err)
	}
	if err := validateStateLeafName(intent.BackupLeaf); err != nil {
		return f.stateRecoveryError(intentObject.path.String(), err)
	}
	if intent.TempLeaf == intent.BackupLeaf ||
		intent.TempLeaf == destinationLeaf ||
		intent.TempLeaf == intentLeaf ||
		intent.BackupLeaf == destinationLeaf ||
		intent.BackupLeaf == intentLeaf {
		return f.stateRecoveryError(intentObject.path.String(), ErrIdentityChanged)
	}
	return nil
}

func encodeStateIntent(intent stateIntent) ([]byte, error) {
	if err := validateStateIntentValue(intent); err != nil {
		return nil, err
	}
	body, err := json.Marshal(intent)
	if err != nil {
		return nil, fmt.Errorf("marshal state intent body: %w", err)
	}
	envelope := stateIntentEnvelope{
		Intent: intent,
		Seal:   sha256.Sum256(body),
	}
	result, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("marshal state intent envelope: %w", err)
	}
	if int64(len(result)) > maxStateIntentBytes {
		return nil, fmt.Errorf("%w: state intent is too large", ErrInvalidArgument)
	}
	return result, nil
}

func decodeStateIntent(payload []byte) (stateIntent, error) {
	if len(payload) == 0 || int64(len(payload)) > maxStateIntentBytes {
		return stateIntent{}, errors.New("state intent has an invalid length")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var envelope stateIntentEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return stateIntent{}, fmt.Errorf("decode state intent: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("state intent has trailing data")
		}
		return stateIntent{}, fmt.Errorf("decode state intent trailing data: %w", err)
	}
	if err := validateStateIntentValue(envelope.Intent); err != nil {
		return stateIntent{}, err
	}
	body, err := json.Marshal(envelope.Intent)
	if err != nil {
		return stateIntent{}, fmt.Errorf("marshal state intent body: %w", err)
	}
	if sha256.Sum256(body) != envelope.Seal {
		return stateIntent{}, errors.New("state intent seal is invalid")
	}
	canonical, err := json.Marshal(envelope)
	if err != nil {
		return stateIntent{}, fmt.Errorf("marshal state intent envelope: %w", err)
	}
	if !bytes.Equal(canonical, payload) {
		return stateIntent{}, errors.New("state intent is not canonical")
	}
	return envelope.Intent, nil
}

func validateStateIntentValue(intent stateIntent) error {
	if intent.Version != stateIntentVersion ||
		!intent.Kind.Valid() ||
		!validStateIntentNonce(intent.Nonce) ||
		intent.Root.VolumeSerial == 0 ||
		intent.Root.FileID == ([16]byte{}) ||
		intent.IntentObject.VolumeSerial == 0 ||
		intent.IntentObject.FileID == ([16]byte{}) ||
		!validStateObjectProof(intent.Old) ||
		!validStateObjectProof(intent.New) {
		return errors.New("state intent fields are invalid")
	}
	for _, leaf := range []string{
		intent.DestinationLeaf,
		intent.IntentLeaf,
	} {
		if err := validateStateLeafName(leaf); err != nil {
			return err
		}
	}
	if intent.IntentLeaf != stateIntentLeaf(intent.Kind) ||
		intent.TempLeaf != stateTransactionLeaf(intent.Kind, "temp", intent.Nonce) ||
		intent.BackupLeaf != stateTransactionLeaf(intent.Kind, "backup", intent.Nonce) {
		return errors.New("state intent leaves are invalid")
	}
	return nil
}

func validStateIntentNonce(nonce string) bool {
	if len(nonce) != stateIntentNonceLength {
		return false
	}
	for i := 0; i < len(nonce); i++ {
		if (nonce[i] < '0' || nonce[i] > '9') && (nonce[i] < 'a' || nonce[i] > 'f') {
			return false
		}
	}
	return true
}

func validStateObjectProof(proof stateObjectProof) bool {
	return proof.VolumeSerial != 0 &&
		proof.FileID != ([16]byte{}) &&
		proof.Size >= 0 &&
		proof.Size <= MaxStateFileBytes &&
		proof.Digest != ([32]byte{})
}

func (f *StateFiles) validateStatePayloadMetadata(
	object pinnedObject,
	maxBytes int64,
	expected *stateObjectProof,
) error {
	if err := validateOrdinaryStateIdentity(
		object.identity,
		f.pins[1].identity.volumeSerial,
	); err != nil {
		return &FileError{
			Operation: "state-leaf-identity",
			Path:      object.path.String(),
			Err:       err,
		}
	}
	if object.identity.size > maxBytes {
		return &FileError{
			Operation: "size-limit",
			Path:      object.path.String(),
			Err:       ErrStateFileTooLarge,
		}
	}
	if object.identity.size < 0 {
		return &FileError{
			Operation: "state-leaf-size",
			Path:      object.path.String(),
			Err:       ErrIdentityChanged,
		}
	}
	if expected != nil &&
		(object.identity.volumeSerial != expected.VolumeSerial ||
			object.identity.fileID != expected.FileID ||
			object.identity.size != expected.Size) {
		return f.stateRecoveryError(object.path.String(), ErrIdentityChanged)
	}
	return nil
}

func (f *StateFiles) snapshotFromPayload(
	ctx context.Context,
	kind StateFileKind,
	payload pinnedObject,
	maxBytes int64,
	expected *stateObjectProof,
) (StateFileSnapshot, error) {
	bytesValue, digest, readErr := f.readPinnedBytes(ctx, payload, maxBytes)
	if readErr == nil && expected != nil && digest != expected.Digest {
		readErr = f.stateRecoveryError(payload.path.String(), ErrIdentityChanged)
	}
	closeErr := f.closeStateObject(&payload)
	if readErr != nil || closeErr != nil {
		return StateFileSnapshot{}, errors.Join(readErr, closeErr)
	}
	return StateFileSnapshot{
		owner:        f.owner,
		kind:         kind,
		volumeSerial: payload.identity.volumeSerial,
		fileID:       payload.identity.fileID,
		size:         int64(len(bytesValue)),
		digest:       digest,
		bytes:        append([]byte(nil), bytesValue...),
	}, nil
}

func (f *StateFiles) readPinnedBytes(
	ctx context.Context,
	object pinnedObject,
	maxBytes int64,
) ([]byte, [32]byte, error) {
	if object.identity.size > maxBytes {
		return nil, [32]byte{}, &FileError{
			Operation: "size-limit",
			Path:      object.path.String(),
			Err:       ErrStateFileTooLarge,
		}
	}
	buffer := make([]byte, int(maxBytes)+1)
	total := 0
	for total < len(buffer) {
		if err := ctx.Err(); err != nil {
			return nil, [32]byte{}, err
		}
		n, err := f.api.readFile(object.handle, buffer[total:])
		if n < 0 || n > len(buffer)-total {
			return nil, [32]byte{}, &FileError{
				Operation: "read",
				Path:      object.path.String(),
				Err:       errors.New("state read returned an invalid byte count"),
			}
		}
		total += n
		if err != nil {
			return nil, [32]byte{}, &FileError{
				Operation: "read",
				Path:      object.path.String(),
				Err:       err,
			}
		}
		if n == 0 {
			break
		}
	}
	if int64(total) > maxBytes {
		return nil, [32]byte{}, &FileError{
			Operation: "size-limit",
			Path:      object.path.String(),
			Err:       ErrStateFileTooLarge,
		}
	}
	identity, err := f.api.identity(object.handle)
	if err != nil {
		return nil, [32]byte{}, &FileError{
			Operation: "identify-after-read",
			Path:      object.path.String(),
			Err:       err,
		}
	}
	if !sameObjectIdentity(identity, object.identity) ||
		identity.size != int64(total) {
		return nil, [32]byte{}, &FileError{
			Operation: "identify-after-read",
			Path:      object.path.String(),
			Err:       ErrIdentityChanged,
		}
	}
	payload := append([]byte(nil), buffer[:total]...)
	return payload, sha256.Sum256(payload), nil
}

func stateWriteFailure(
	phase StateWritePhase,
	mutationApplied bool,
	recoveryRequired bool,
	cause error,
	cleanupError error,
) (WriteAtomicResult, error) {
	result := WriteAtomicResult{
		MutationApplied:  mutationApplied,
		RecoveryRequired: recoveryRequired,
	}
	return result, &StateWriteError{
		Phase:            phase,
		MutationApplied:  result.MutationApplied,
		RecoveryRequired: result.RecoveryRequired,
		Cause:            cause,
		CleanupError:     cleanupError,
	}
}

func stateMutationOpenSpec() openSpec {
	return openSpec{
		access: windows.FILE_READ_DATA |
			windows.FILE_READ_ATTRIBUTES |
			windows.DELETE |
			windows.SYNCHRONIZE,
		share:     windows.FILE_SHARE_READ | windows.FILE_SHARE_DELETE,
		creation:  windows.OPEN_EXISTING,
		options:   windows.FILE_FLAG_OPEN_REPARSE_POINT,
		directory: false,
	}
}

func stateMutationCreateSpec() openSpec {
	spec := stateMutationOpenSpec()
	spec.access |= windows.FILE_WRITE_DATA
	spec.creation = windows.CREATE_NEW
	return spec
}

func stateAbsenceProbeSpec() openSpec {
	return openSpec{
		access:    windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE,
		share:     windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE,
		creation:  windows.OPEN_EXISTING,
		options:   windows.FILE_FLAG_OPEN_REPARSE_POINT,
		directory: false,
	}
}

func stateUnlinkOpenSpec() openSpec {
	return openSpec{
		access:    windows.DELETE | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE,
		share:     windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE,
		creation:  windows.OPEN_EXISTING,
		options:   windows.FILE_FLAG_OPEN_REPARSE_POINT,
		directory: false,
	}
}

func (f *StateFiles) newStateNonce() (string, error) {
	bytesValue := make([]byte, stateIntentNonceLength/2)
	if err := f.fillNonce(bytesValue); err != nil {
		return "", fmt.Errorf("fill state nonce: %w", err)
	}
	return hex.EncodeToString(bytesValue), nil
}

func (f *StateFiles) validateOpenedStateObject(
	ctx context.Context,
	leaf string,
	handle windows.Handle,
) (pinnedObject, error) {
	path := filepath.Join(f.pins[1].path.String(), leaf)
	closeOnFailure := func(operationErr error) (pinnedObject, error) {
		return pinnedObject{}, errors.Join(
			operationErr,
			wrapFileError("close", path, f.api.closeHandle(handle)),
		)
	}
	identity, err := f.api.identity(handle)
	if err != nil {
		return closeOnFailure(&FileError{Operation: "state-leaf-identify", Path: path, Err: err})
	}
	if err := validateOrdinaryStateIdentity(identity, f.pins[1].identity.volumeSerial); err != nil {
		return closeOnFailure(&FileError{Operation: "state-leaf-identity", Path: path, Err: err})
	}
	finalPath, err := f.api.finalPath(handle)
	if err != nil {
		return closeOnFailure(&FileError{Operation: "state-leaf-final-path", Path: path, Err: err})
	}
	canonical, err := canonicalizeContextWith(ctx, finalPath, f.api)
	if err != nil {
		return closeOnFailure(err)
	}
	expected, err := canonicalizeContextWith(ctx, path, f.api)
	if err != nil {
		return closeOnFailure(err)
	}
	if !canonical.Equal(expected) {
		return closeOnFailure(&FileError{
			Operation: "state-leaf-path",
			Path:      path,
			Err:       ErrIdentityChanged,
		})
	}
	object := pinnedObject{path: canonical, handle: handle, identity: identity}
	if err := validateParentIdentityWith(ctx, f.pins[1], object, f.api); err != nil {
		return closeOnFailure(err)
	}
	return object, nil
}

func (f *StateFiles) createStateLeaf(
	ctx context.Context,
	leaf string,
) (pinnedObject, error) {
	if err := validateStateLeafName(leaf); err != nil {
		return pinnedObject{}, err
	}
	object, err := openRelativeCheckedWith(
		ctx,
		f.pins[1],
		leaf,
		stateMutationCreateSpec(),
		f.api,
	)
	if err != nil {
		return pinnedObject{}, err
	}
	if err := validateOrdinaryStateIdentity(
		object.identity,
		f.pins[1].identity.volumeSerial,
	); err != nil {
		return pinnedObject{}, errors.Join(
			&FileError{Operation: "state-leaf-identity", Path: object.path.String(), Err: err},
			f.closeStateObject(&object),
		)
	}
	return object, nil
}

func (f *StateFiles) openStateMutationLeaf(
	ctx context.Context,
	leaf string,
) (pinnedObject, bool, error) {
	return f.openStateLeaf(ctx, leaf, stateMutationOpenSpec())
}

func (f *StateFiles) writeAllStateBytes(
	ctx context.Context,
	object pinnedObject,
	payload []byte,
) error {
	written := 0
	for written < len(payload) {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := f.api.writeFile(object.handle, payload[written:])
		if n < 0 || n > len(payload)-written {
			return &FileError{
				Operation: "write",
				Path:      object.path.String(),
				Err:       errors.New("state write returned an invalid byte count"),
			}
		}
		written += n
		if err != nil {
			return &FileError{Operation: "write", Path: object.path.String(), Err: err}
		}
		if n == 0 {
			return &FileError{
				Operation: "write",
				Path:      object.path.String(),
				Err:       io.ErrShortWrite,
			}
		}
	}
	return nil
}

func rewindStateHandle(object pinnedObject) error {
	if _, err := windows.SetFilePointer(object.handle, 0, nil, windows.FILE_BEGIN); err != nil {
		return &FileError{Operation: "rewind", Path: object.path.String(), Err: err}
	}
	return nil
}

func (f *StateFiles) sealWrittenStateObject(
	ctx context.Context,
	object *pinnedObject,
	payload []byte,
) (stateObjectProof, error) {
	if err := f.api.flushFile(object.handle); err != nil {
		return stateObjectProof{}, &FileError{
			Operation: "flush",
			Path:      object.path.String(),
			Err:       err,
		}
	}
	identity, err := f.api.identity(object.handle)
	if err != nil {
		return stateObjectProof{}, &FileError{
			Operation: "identify-after-write",
			Path:      object.path.String(),
			Err:       err,
		}
	}
	if identity.volumeSerial != object.identity.volumeSerial ||
		identity.fileID != object.identity.fileID {
		return stateObjectProof{}, &FileError{
			Operation: "identify-after-write",
			Path:      object.path.String(),
			Err:       ErrIdentityChanged,
		}
	}
	if err := validateOrdinaryStateIdentity(
		identity,
		f.pins[1].identity.volumeSerial,
	); err != nil {
		return stateObjectProof{}, &FileError{
			Operation: "identify-after-write",
			Path:      object.path.String(),
			Err:       err,
		}
	}
	if identity.size != int64(len(payload)) {
		return stateObjectProof{}, &FileError{
			Operation: "identify-after-write",
			Path:      object.path.String(),
			Err:       ErrIdentityChanged,
		}
	}
	object.identity = identity
	if err := rewindStateHandle(*object); err != nil {
		return stateObjectProof{}, err
	}
	actual, digest, err := f.readPinnedBytes(ctx, *object, int64(len(payload)))
	if err != nil {
		return stateObjectProof{}, err
	}
	if !bytes.Equal(actual, payload) {
		return stateObjectProof{}, &FileError{
			Operation: "verify-write",
			Path:      object.path.String(),
			Err:       ErrIdentityChanged,
		}
	}
	return stateObjectProof{
		stateIdentityProof: proofIdentityValue(object.identity),
		Size:               object.identity.size,
		Digest:             digest,
	}, nil
}

func proofIdentityValue(identity objectIdentity) stateIdentityProof {
	return stateIdentityProof{
		VolumeSerial: identity.volumeSerial,
		FileID:       identity.fileID,
	}
}

func (f *StateFiles) ensurePOSIXUnlinkCapability(
	ctx context.Context,
	kind StateFileKind,
) error {
	if f.probePassed[kind] {
		return nil
	}
	nonce, err := f.newStateNonce()
	if err != nil {
		return errors.Join(ErrPOSIXUnlinkUnsupported, err)
	}
	leaf := stateTransactionLeaf(kind, "probe", nonce)
	spec := ntCreateSpec{
		desiredAccess: windows.FILE_READ_DATA |
			windows.FILE_WRITE_DATA |
			windows.FILE_READ_ATTRIBUTES |
			windows.DELETE |
			windows.SYNCHRONIZE,
		shareAccess:       windows.FILE_SHARE_READ | windows.FILE_SHARE_DELETE,
		createDisposition: ntFileCreate,
		createOptions: ntFileOpenReparsePoint |
			ntFileNonDirectoryFile |
			ntFileSynchronousNonalert |
			ntFileDeleteOnClose,
	}
	handle, err := f.api.ntCreateRelative(f.pins[1].handle, leaf, spec)
	if err != nil {
		return errors.Join(ErrPOSIXUnlinkUnsupported, err)
	}
	object, err := f.validateOpenedStateObject(ctx, leaf, handle)
	if err != nil {
		return errors.Join(ErrPOSIXUnlinkUnsupported, err)
	}
	marker := []byte("auto-mas-state-posix-probe")
	probeErr := f.writeAllStateBytes(ctx, object, marker)
	if probeErr == nil {
		probeErr = f.api.flushFile(object.handle)
	}
	if probeErr == nil {
		identity, identityErr := f.api.identity(object.handle)
		if identityErr != nil {
			probeErr = identityErr
		} else if identity.volumeSerial != object.identity.volumeSerial ||
			identity.fileID != object.identity.fileID ||
			identity.size != int64(len(marker)) {
			probeErr = ErrIdentityChanged
		} else {
			object.identity = identity
		}
	}
	if probeErr == nil {
		proof := stateObjectProof{
			stateIdentityProof: proofIdentityValue(object.identity),
			Size:               int64(len(marker)),
			Digest:             sha256.Sum256(marker),
		}
		probeErr = f.unlinkStateObject(ctx, leaf, &object, &proof)
	}
	closeErr := f.closeStateObject(&object)
	absenceErr := f.requireStateLeafAbsent(context.WithoutCancel(ctx), leaf)
	if absenceErr != nil {
		absenceErr = fmt.Errorf("verify probe absent after anchor close: %w", absenceErr)
	}
	if probeErr != nil || closeErr != nil || absenceErr != nil {
		return errors.Join(ErrPOSIXUnlinkUnsupported, probeErr, closeErr, absenceErr)
	}
	f.probePassed[kind] = true
	return nil
}

func statePOSIXDispositionSpec() stateDispositionSpec {
	return stateDispositionSpec{
		informationClass: fileDispositionExClass,
		flags:            fileDispositionDelete | fileDispositionPOSIX,
	}
}

func (f *StateFiles) requireStateLeafAbsent(ctx context.Context, leaf string) error {
	object, missing, err := f.openStateLeaf(ctx, leaf, stateAbsenceProbeSpec())
	if err != nil {
		return err
	}
	if !missing {
		return errors.Join(
			&FileError{Operation: "verify-unlink", Path: object.path.String(), Err: ErrIdentityChanged},
			f.closeStateObject(&object),
		)
	}
	return nil
}

func (f *StateFiles) unlinkStateObject(
	ctx context.Context,
	leaf string,
	object *pinnedObject,
	expected *stateObjectProof,
) error {
	if object == nil || object.handle == 0 || object.handle == windows.InvalidHandle {
		return fmt.Errorf("%w: state unlink object is invalid", ErrInvalidArgument)
	}
	current, err := f.api.identity(object.handle)
	if err != nil {
		return &FileError{Operation: "identify-before-unlink", Path: object.path.String(), Err: err}
	}
	if err := validateOrdinaryStateIdentity(
		current,
		f.pins[1].identity.volumeSerial,
	); err != nil {
		return &FileError{Operation: "identify-before-unlink", Path: object.path.String(), Err: err}
	}
	if current.volumeSerial != object.identity.volumeSerial ||
		current.fileID != object.identity.fileID {
		return &FileError{
			Operation: "identify-before-unlink",
			Path:      object.path.String(),
			Err:       ErrIdentityChanged,
		}
	}
	if expected != nil &&
		(current.volumeSerial != expected.VolumeSerial ||
			current.fileID != expected.FileID ||
			current.size != expected.Size) {
		return &FileError{
			Operation: "verify-before-unlink",
			Path:      object.path.String(),
			Err:       ErrIdentityChanged,
		}
	}
	object.identity = current
	if err := f.verifyStateAnchorReadable(ctx, object, expected); err != nil {
		return err
	}
	unlink, missing, err := f.openStateLeaf(ctx, leaf, stateUnlinkOpenSpec())
	if err != nil {
		return err
	}
	if missing {
		return &FileError{
			Operation: "open-unlink",
			Path:      object.path.String(),
			Err:       ErrIdentityChanged,
		}
	}
	if unlink.identity.volumeSerial != object.identity.volumeSerial ||
		unlink.identity.fileID != object.identity.fileID {
		return errors.Join(
			&FileError{
				Operation: "identify-unlink",
				Path:      unlink.path.String(),
				Err:       ErrIdentityChanged,
			},
			f.closeStateObject(&unlink),
		)
	}
	dispositionErr := f.api.setStateDisposition(unlink.handle, statePOSIXDispositionSpec())
	closeErr := f.closeStateObject(&unlink)
	if dispositionErr != nil || closeErr != nil {
		return &stateUnlinkMutationError{
			dispositionErr: wrapFileError(
				"posix-unlink",
				object.path.String(),
				dispositionErr,
			),
			closeErr: closeErr,
		}
	}
	if err := f.requireStateLeafAbsent(ctx, leaf); err != nil {
		return err
	}
	return f.verifyStateAnchorReadable(ctx, object, expected)
}

func (f *StateFiles) verifyStateAnchorReadable(
	ctx context.Context,
	object *pinnedObject,
	expected *stateObjectProof,
) error {
	current, err := f.api.identity(object.handle)
	if err != nil {
		return &FileError{
			Operation: "identify-anchor",
			Path:      object.path.String(),
			Err:       err,
		}
	}
	if current.volumeSerial != object.identity.volumeSerial ||
		current.fileID != object.identity.fileID ||
		current.attributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return &FileError{
			Operation: "identify-anchor",
			Path:      object.path.String(),
			Err:       ErrIdentityChanged,
		}
	}
	if expected != nil &&
		(current.volumeSerial != expected.VolumeSerial ||
			current.fileID != expected.FileID ||
			current.size != expected.Size) {
		return &FileError{
			Operation: "verify-anchor",
			Path:      object.path.String(),
			Err:       ErrIdentityChanged,
		}
	}
	object.identity = current
	if err := rewindStateHandle(*object); err != nil {
		return err
	}
	_, digest, err := f.readPinnedBytes(ctx, *object, current.size)
	if err != nil {
		return err
	}
	if expected != nil && digest != expected.Digest {
		return &FileError{
			Operation: "verify-anchor",
			Path:      object.path.String(),
			Err:       ErrIdentityChanged,
		}
	}
	return nil
}

func (f *StateFiles) inspectStateMutationCandidate(
	ctx context.Context,
	leaf string,
) (stateCandidate, error) {
	object, missing, err := f.openStateMutationLeaf(ctx, leaf)
	if err != nil {
		return stateCandidate{}, f.stateRecoveryError(
			filepath.Join(f.pins[1].path.String(), leaf),
			err,
		)
	}
	if missing {
		return stateCandidate{leaf: leaf}, nil
	}
	payload, digest, err := f.readPinnedBytes(ctx, object, MaxStateFileBytes)
	if err != nil {
		return stateCandidate{}, errors.Join(
			f.stateRecoveryError(object.path.String(), err),
			f.closeStateObject(&object),
		)
	}
	return stateCandidate{
		leaf:    leaf,
		object:  object,
		payload: payload,
		digest:  digest,
		present: true,
	}, nil
}

func (f *StateFiles) recoverStateNamespace(
	ctx context.Context,
	kind StateFileKind,
) error {
	destinationPath, err := f.statePath(kind)
	if err != nil {
		return err
	}
	destinationLeaf := filepath.Base(destinationPath)
	intentLeaf := stateIntentLeaf(kind)
	intentObject, intentMissing, err := f.openStateMutationLeaf(ctx, intentLeaf)
	if err != nil || intentMissing {
		return err
	}
	intentBytes, _, err := f.readPinnedBytes(ctx, intentObject, maxStateIntentBytes)
	if err != nil {
		return errors.Join(
			f.stateRecoveryError(intentObject.path.String(), err),
			f.closeStateObject(&intentObject),
		)
	}
	intent, err := decodeStateIntent(intentBytes)
	if err != nil {
		return errors.Join(
			f.stateRecoveryError(intentObject.path.String(), err),
			f.closeStateObject(&intentObject),
		)
	}
	if err := f.validateIntentBinding(
		kind,
		destinationLeaf,
		intentLeaf,
		intentObject,
		intent,
	); err != nil {
		return errors.Join(err, f.closeStateObject(&intentObject))
	}
	destination, err := f.inspectStateMutationCandidate(ctx, destinationLeaf)
	if err != nil {
		return errors.Join(err, f.closeStateObject(&intentObject))
	}
	backup, err := f.inspectStateMutationCandidate(ctx, intent.BackupLeaf)
	if err != nil {
		return errors.Join(err, f.closeCandidate(&destination), f.closeStateObject(&intentObject))
	}
	temp, err := f.inspectStateMutationCandidate(ctx, intent.TempLeaf)
	if err != nil {
		return errors.Join(
			err,
			f.closeCandidate(&backup),
			f.closeCandidate(&destination),
			f.closeStateObject(&intentObject),
		)
	}

	cleanupContext := context.WithoutCancel(ctx)
	var operationErr error
	switch {
	case destination.matches(intent.Old) && !backup.present:
		if temp.present {
			if !temp.matches(intent.New) {
				operationErr = errors.Join(ErrStateRecoveryRequired, ErrIdentityChanged)
				break
			}
			operationErr = f.unlinkStateObject(cleanupContext, temp.leaf, &temp.object, &intent.New)
		}
		if operationErr == nil {
			operationErr = f.unlinkStateObject(cleanupContext, intentLeaf, &intentObject, nil)
		}
	case !destination.present && backup.matches(intent.Old) && temp.matches(intent.New):
		if err := f.api.renameState(
			backup.object.handle,
			f.pins[1].handle,
			destinationLeaf,
			0,
		); err != nil {
			operationErr = &FileError{
				Operation: "recover-rollback",
				Path:      destinationPath,
				Err:       err,
			}
			break
		}
		if err := f.unlinkStateObject(cleanupContext, temp.leaf, &temp.object, &intent.New); err != nil {
			operationErr = err
			break
		}
		operationErr = f.unlinkStateObject(cleanupContext, intentLeaf, &intentObject, nil)
	case destination.matches(intent.New) && !temp.present:
		if backup.present {
			if !backup.matches(intent.Old) {
				operationErr = errors.Join(ErrStateRecoveryRequired, ErrIdentityChanged)
				break
			}
			if err := f.unlinkStateObject(
				cleanupContext,
				backup.leaf,
				&backup.object,
				&intent.Old,
			); err != nil {
				operationErr = err
				break
			}
		}
		operationErr = f.unlinkStateObject(cleanupContext, intentLeaf, &intentObject, nil)
	default:
		operationErr = errors.Join(ErrStateRecoveryRequired, ErrIdentityChanged)
	}
	closeErr := errors.Join(
		f.closeCandidate(&temp),
		f.closeCandidate(&backup),
		f.closeCandidate(&destination),
		f.closeStateObject(&intentObject),
	)
	if operationErr != nil || closeErr != nil {
		return f.stateRecoveryError(destinationPath, errors.Join(operationErr, closeErr))
	}
	return nil
}

func (f *StateFiles) writeStateLocked(
	ctx context.Context,
	kind StateFileKind,
	payload []byte,
	guard pinnedObject,
) (WriteAtomicResult, error) {
	nonce, err := f.newStateNonce()
	if err != nil {
		closeErr := f.closeStateObject(&guard)
		return stateWriteFailure(StateWritePhaseCreate, false, closeErr != nil, err, closeErr)
	}
	tempLeaf := stateTransactionLeaf(kind, "temp", nonce)
	backupLeaf := stateTransactionLeaf(kind, "backup", nonce)
	stagingLeaf := stateTransactionLeaf(kind, "intent", nonce)
	intentLeaf := stateIntentLeaf(kind)
	destinationPath, err := f.statePath(kind)
	if err != nil {
		closeErr := f.closeStateObject(&guard)
		return stateWriteFailure(StateWritePhaseRecover, false, true, err, closeErr)
	}
	destinationLeaf := filepath.Base(destinationPath)

	temp, err := f.createStateLeaf(ctx, tempLeaf)
	if err != nil {
		closeErr := f.closeStateObject(&guard)
		return stateWriteFailure(StateWritePhaseCreate, false, closeErr != nil, err, closeErr)
	}
	if err := f.writeAllStateBytes(ctx, temp, payload); err != nil {
		cleanupErr := errors.Join(
			f.unlinkStateObject(context.WithoutCancel(ctx), tempLeaf, &temp, nil),
			f.closeStateObject(&temp),
			f.closeStateObject(&guard),
		)
		return stateWriteFailure(
			StateWritePhaseWrite,
			false,
			cleanupErr != nil,
			err,
			cleanupErr,
		)
	}
	newProof, err := f.sealWrittenStateObject(ctx, &temp, payload)
	if err != nil {
		cleanupErr := errors.Join(
			f.unlinkStateObject(context.WithoutCancel(ctx), tempLeaf, &temp, nil),
			f.closeStateObject(&temp),
			f.closeStateObject(&guard),
		)
		return stateWriteFailure(
			StateWritePhaseSync,
			false,
			cleanupErr != nil,
			err,
			cleanupErr,
		)
	}
	intentProbe, intentMissing, err := f.openStateMutationLeaf(ctx, intentLeaf)
	if err != nil || !intentMissing {
		if err == nil {
			err = f.stateRecoveryError(intentProbe.path.String(), ErrIdentityChanged)
		}
		cleanupErr := errors.Join(
			f.closeStateObject(&intentProbe),
			f.unlinkStateObject(context.WithoutCancel(ctx), tempLeaf, &temp, &newProof),
			f.closeStateObject(&temp),
			f.closeStateObject(&guard),
		)
		return stateWriteFailure(StateWritePhaseRecover, false, true, err, cleanupErr)
	}
	old, err := f.inspectStateMutationCandidate(ctx, destinationLeaf)
	if err != nil {
		cleanupErr := errors.Join(
			f.unlinkStateObject(context.WithoutCancel(ctx), tempLeaf, &temp, &newProof),
			f.closeStateObject(&temp),
			f.closeStateObject(&guard),
		)
		return stateWriteFailure(StateWritePhaseRecover, false, true, err, cleanupErr)
	}
	if !old.present {
		if err := ctx.Err(); err != nil {
			cleanupErr := errors.Join(
				f.unlinkStateObject(context.WithoutCancel(ctx), tempLeaf, &temp, &newProof),
				f.closeStateObject(&temp),
				f.closeStateObject(&guard),
			)
			return stateWriteFailure(StateWritePhaseRename, false, cleanupErr != nil, err, cleanupErr)
		}
		if err := f.api.renameState(temp.handle, f.pins[1].handle, destinationLeaf, 0); err != nil {
			cleanupErr := errors.Join(
				f.unlinkStateObject(context.WithoutCancel(ctx), tempLeaf, &temp, &newProof),
				f.closeStateObject(&temp),
				f.closeStateObject(&guard),
			)
			return stateWriteFailure(StateWritePhaseRename, false, cleanupErr != nil, err, cleanupErr)
		}
		closeErr := errors.Join(f.closeStateObject(&temp), f.closeStateObject(&guard))
		if closeErr != nil {
			return stateWriteFailure(StateWritePhaseClose, true, true, closeErr, nil)
		}
		return WriteAtomicResult{MutationApplied: true}, nil
	}
	oldProof := stateObjectProof{
		stateIdentityProof: proofIdentityValue(old.object.identity),
		Size:               old.object.identity.size,
		Digest:             old.digest,
	}
	return f.replaceStateLocked(
		ctx,
		kind,
		destinationLeaf,
		intentLeaf,
		stagingLeaf,
		tempLeaf,
		backupLeaf,
		nonce,
		old,
		oldProof,
		temp,
		newProof,
		guard,
	)
}

func (f *StateFiles) replaceStateLocked(
	ctx context.Context,
	kind StateFileKind,
	destinationLeaf string,
	intentLeaf string,
	stagingLeaf string,
	tempLeaf string,
	backupLeaf string,
	nonce string,
	old stateCandidate,
	oldProof stateObjectProof,
	temp pinnedObject,
	newProof stateObjectProof,
	guard pinnedObject,
) (WriteAtomicResult, error) {
	staging, err := f.createStateLeaf(ctx, stagingLeaf)
	if err != nil {
		cleanupErr := errors.Join(
			f.unlinkStateObject(context.WithoutCancel(ctx), tempLeaf, &temp, &newProof),
			f.closeStateObject(&temp),
			f.closeCandidate(&old),
			f.closeStateObject(&guard),
		)
		return stateWriteFailure(StateWritePhaseCreate, false, cleanupErr != nil, err, cleanupErr)
	}
	intent := stateIntent{
		Version:         stateIntentVersion,
		Kind:            kind,
		DestinationLeaf: destinationLeaf,
		IntentLeaf:      intentLeaf,
		TempLeaf:        tempLeaf,
		BackupLeaf:      backupLeaf,
		Nonce:           nonce,
		Root:            proofIdentityValue(f.pins[1].identity),
		IntentObject:    proofIdentityValue(staging.identity),
		Old:             oldProof,
		New:             newProof,
	}
	envelope, err := encodeStateIntent(intent)
	if err != nil {
		cleanupErr := f.cleanupUnpublishedState(
			ctx,
			tempLeaf,
			&temp,
			&newProof,
			stagingLeaf,
			&staging,
			&old,
			&guard,
		)
		return stateWriteFailure(StateWritePhaseCreate, false, cleanupErr != nil, err, cleanupErr)
	}
	if err := f.writeAllStateBytes(ctx, staging, envelope); err != nil {
		cleanupErr := f.cleanupUnpublishedState(
			ctx,
			tempLeaf,
			&temp,
			&newProof,
			stagingLeaf,
			&staging,
			&old,
			&guard,
		)
		return stateWriteFailure(StateWritePhaseWrite, false, cleanupErr != nil, err, cleanupErr)
	}
	if _, err := f.sealWrittenStateObject(ctx, &staging, envelope); err != nil {
		cleanupErr := f.cleanupUnpublishedState(
			ctx,
			tempLeaf,
			&temp,
			&newProof,
			stagingLeaf,
			&staging,
			&old,
			&guard,
		)
		return stateWriteFailure(StateWritePhaseSync, false, cleanupErr != nil, err, cleanupErr)
	}
	if err := ctx.Err(); err != nil {
		cleanupErr := f.cleanupUnpublishedState(
			ctx,
			tempLeaf,
			&temp,
			&newProof,
			stagingLeaf,
			&staging,
			&old,
			&guard,
		)
		return stateWriteFailure(StateWritePhaseRename, false, cleanupErr != nil, err, cleanupErr)
	}
	if err := f.api.renameState(staging.handle, f.pins[1].handle, intentLeaf, 0); err != nil {
		cleanupErr := f.cleanupUnpublishedState(
			ctx,
			tempLeaf,
			&temp,
			&newProof,
			stagingLeaf,
			&staging,
			&old,
			&guard,
		)
		return stateWriteFailure(StateWritePhaseRename, false, cleanupErr != nil, err, cleanupErr)
	}
	if err := ctx.Err(); err != nil {
		cleanupErr := f.cleanupPublishedBeforeBackup(
			ctx,
			intentLeaf,
			&staging,
			tempLeaf,
			&temp,
			&newProof,
			&old,
			&guard,
		)
		return stateWriteFailure(StateWritePhaseRename, false, cleanupErr != nil, err, cleanupErr)
	}
	if err := f.api.renameState(old.object.handle, f.pins[1].handle, backupLeaf, 0); err != nil {
		cleanupErr := f.cleanupPublishedBeforeBackup(
			ctx,
			intentLeaf,
			&staging,
			tempLeaf,
			&temp,
			&newProof,
			&old,
			&guard,
		)
		return stateWriteFailure(StateWritePhaseRename, false, cleanupErr != nil, err, cleanupErr)
	}
	if err := ctx.Err(); err != nil {
		cleanupErr := f.rollbackStateBeforeCommit(
			ctx,
			destinationLeaf,
			intentLeaf,
			&staging,
			tempLeaf,
			&temp,
			&newProof,
			&old,
			&guard,
		)
		return stateWriteFailure(StateWritePhaseRename, false, cleanupErr != nil, err, cleanupErr)
	}
	if err := f.api.renameState(temp.handle, f.pins[1].handle, destinationLeaf, 0); err != nil {
		cleanupErr := f.rollbackStateBeforeCommit(
			ctx,
			destinationLeaf,
			intentLeaf,
			&staging,
			tempLeaf,
			&temp,
			&newProof,
			&old,
			&guard,
		)
		return stateWriteFailure(StateWritePhaseRename, false, cleanupErr != nil, err, cleanupErr)
	}

	finalizeContext := context.WithoutCancel(ctx)
	finalizeErr := f.unlinkStateObject(finalizeContext, backupLeaf, &old.object, &oldProof)
	if finalizeErr == nil {
		finalizeErr = f.unlinkStateObject(finalizeContext, intentLeaf, &staging, nil)
	}
	closeErr := errors.Join(
		f.closeStateObject(&staging),
		f.closeStateObject(&temp),
		f.closeCandidate(&old),
		f.closeStateObject(&guard),
	)
	if finalizeErr != nil {
		return stateWriteFailure(
			StateWritePhaseFinalize,
			true,
			true,
			finalizeErr,
			closeErr,
		)
	}
	if closeErr != nil {
		return stateWriteFailure(StateWritePhaseClose, true, true, closeErr, nil)
	}
	return WriteAtomicResult{MutationApplied: true}, nil
}

func (f *StateFiles) cleanupUnpublishedState(
	ctx context.Context,
	tempLeaf string,
	temp *pinnedObject,
	newProof *stateObjectProof,
	stagingLeaf string,
	staging *pinnedObject,
	old *stateCandidate,
	guard *pinnedObject,
) error {
	cleanupContext := context.WithoutCancel(ctx)
	return errors.Join(
		f.unlinkStateObject(cleanupContext, stagingLeaf, staging, nil),
		f.unlinkStateObject(cleanupContext, tempLeaf, temp, newProof),
		f.closeStateObject(staging),
		f.closeStateObject(temp),
		f.closeCandidate(old),
		f.closeStateObject(guard),
	)
}

func (f *StateFiles) cleanupPublishedBeforeBackup(
	ctx context.Context,
	intentLeaf string,
	intent *pinnedObject,
	tempLeaf string,
	temp *pinnedObject,
	newProof *stateObjectProof,
	old *stateCandidate,
	guard *pinnedObject,
) error {
	cleanupContext := context.WithoutCancel(ctx)
	return errors.Join(
		f.unlinkStateObject(cleanupContext, tempLeaf, temp, newProof),
		f.unlinkStateObject(cleanupContext, intentLeaf, intent, nil),
		f.closeStateObject(intent),
		f.closeStateObject(temp),
		f.closeCandidate(old),
		f.closeStateObject(guard),
	)
}

func (f *StateFiles) rollbackStateBeforeCommit(
	ctx context.Context,
	destinationLeaf string,
	intentLeaf string,
	intent *pinnedObject,
	tempLeaf string,
	temp *pinnedObject,
	newProof *stateObjectProof,
	old *stateCandidate,
	guard *pinnedObject,
) error {
	cleanupContext := context.WithoutCancel(ctx)
	rollbackErr := f.api.renameState(
		old.object.handle,
		f.pins[1].handle,
		destinationLeaf,
		0,
	)
	if rollbackErr != nil {
		return errors.Join(
			&FileError{
				Operation: "rollback",
				Path:      filepath.Join(f.pins[1].path.String(), destinationLeaf),
				Err:       rollbackErr,
			},
			f.closeStateObject(intent),
			f.closeStateObject(temp),
			f.closeCandidate(old),
			f.closeStateObject(guard),
		)
	}
	return errors.Join(
		f.unlinkStateObject(cleanupContext, tempLeaf, temp, newProof),
		f.unlinkStateObject(cleanupContext, intentLeaf, intent, nil),
		f.closeStateObject(intent),
		f.closeStateObject(temp),
		f.closeCandidate(old),
		f.closeStateObject(guard),
	)
}

func (f *StateFiles) stateRecoveryError(path string, cause error) error {
	return &FileError{
		Operation: "recover",
		Path:      path,
		Err:       errors.Join(ErrStateRecoveryRequired, cause),
	}
}

func (f *StateFiles) closeCandidate(candidate *stateCandidate) error {
	if candidate == nil || !candidate.present {
		return nil
	}
	return f.closeStateObject(&candidate.object)
}

func (f *StateFiles) closeStateObject(object *pinnedObject) error {
	return closePinnedObject(f.api, object)
}

type stateFileNotFoundError struct {
	kind StateFileKind
	path string
}

func (e *stateFileNotFoundError) Error() string {
	return fmt.Sprintf("state file %s is not present", e.kind)
}

func (e *stateFileNotFoundError) Is(target error) bool {
	return target == ErrStateFileNotFound
}

// Close 关闭 StateFiles 固定的 app/state root pin。
func (f *StateFiles) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return f.closeErr
	}
	f.closed = true
	f.closeErr = closePinnedObjects(f.api, f.pins[:])
	return f.closeErr
}
