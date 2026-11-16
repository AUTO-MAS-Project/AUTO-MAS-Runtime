//go:build linux

package lock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

// ProbeResult 描述零等待内核协调探测结果。
type ProbeResult struct {
	Held      bool
	Recovered bool
}

// AcquisitionResult 保存获取的租约与对端状态。
type AcquisitionResult struct {
	lease     *Lease
	peerProbe ProbeResult
	recovered bool
}

// Lease 绑定根 inode 上的随机租约 token，关闭不会删除目录。
type Lease struct {
	set   *Set
	kind  Kind
	token string
	once  sync.Once
	err   error
}

// Set 通过固定根 inode 的 flock 门闩串行化租约元数据访问。
type Set struct {
	mu     sync.Mutex // 保护本实例的活跃租约与句柄生命周期。
	layout *config.Layout
	root   *os.File
	info   os.FileInfo
	lease  *Lease
	closed bool
}
type posixOwner struct {
	PID     uint32 `json:"pid"`
	Created string `json:"created"`
	Token   string `json:"token"`
}

// NewSet 固定安装根身份；不创建状态目录或锁文件。
func NewSet(ctx context.Context, layout *config.Layout) (*Set, error) {
	if ctx == nil || layout == nil {
		return nil, filesystem.ErrInvalidArgument
	}
	root, err := filesystem.OpenManagedDirectoryHandle(ctx, layout, layout.AppRoot())
	if err != nil {
		return nil, err
	}
	info, err := root.Stat()
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	return &Set{layout: layout, root: root, info: info}, nil
}
func (s *Set) gate(ctx context.Context) error {
	if s.closed {
		return ErrClosed
	}
	if ctx == nil {
		return filesystem.ErrInvalidArgument
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Flock(int(s.root.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return err
		}
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	file, err := filesystem.OpenManagedDirectoryHandle(ctx, s.layout, s.layout.AppRoot())
	if err == nil {
		var info os.FileInfo
		info, err = file.Stat()
		err = errors.Join(err, file.Close())
		if err == nil && !os.SameFile(info, s.info) {
			err = filesystem.ErrIdentityChanged
		}
	}
	if err != nil {
		err = errors.Join(err, s.ungate())
	}
	return err
}
func (s *Set) ungate() error          { return unix.Flock(int(s.root.Fd()), unix.LOCK_UN) }
func ownerAttribute(kind Kind) string { return "user.auto-mas-runtime.lock." + string(kind) }
func (s *Set) owner(ctx context.Context, kind Kind) (posixOwner, ProbeResult, error) {
	data := make([]byte, 2048)
	n, err := unix.Fgetxattr(int(s.root.Fd()), ownerAttribute(kind), data)
	if missingPOSIXXattr(err) || err == nil && n == 0 {
		return posixOwner{}, ProbeResult{}, nil
	}
	if err != nil {
		return posixOwner{}, ProbeResult{}, err
	}
	var owner posixOwner
	if err = json.Unmarshal(data[:n], &owner); err != nil {
		return owner, ProbeResult{}, err
	}
	if owner.PID == 0 || owner.Created == "" || len(owner.Token) != 32 {
		return owner, ProbeResult{}, filesystem.ErrIdentityChanged
	}
	probe := state.NewSystemPIDProbe()
	alive, err := probe.Alive(ctx, owner.PID)
	if err != nil {
		return owner, ProbeResult{}, err
	}
	if !alive {
		return owner, ProbeResult{Recovered: true}, nil
	}
	created, err := probe.Identity(ctx, owner.PID)
	if err != nil {
		return owner, ProbeResult{}, err
	}
	if created != owner.Created {
		return owner, ProbeResult{Recovered: true}, nil
	}
	return owner, ProbeResult{Held: true}, nil
}
func (s *Set) conflict(kind, requested Kind) error {
	code := protocol.CodeMutationInProgress
	if kind == KindBackend {
		code = protocol.CodeBackendStillRunning
		if requested == KindBackend {
			code = protocol.CodeBackendAlreadyRunning
		}
	}
	return &ConflictError{code: code, Kind: kind, Name: string(kind)}
}
func (s *Set) acquire(ctx context.Context, kind Kind, staging bool) (result AcquisitionResult, returnErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		if returnErr != nil {
			var conflict *ConflictError
			var operation *OperationError
			if !errors.As(returnErr, &conflict) && !errors.As(returnErr, &operation) {
				returnErr = &OperationError{Operation: "posix-lease", Kind: kind, Cause: returnErr}
			}
		}
	}()
	if err := s.gate(ctx); err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, s.ungate()) }()
	if s.lease != nil {
		return result, s.conflict(s.lease.kind, kind)
	}
	_, primary, err := s.owner(ctx, kind)
	if err != nil {
		return result, err
	}
	if primary.Held {
		return result, s.conflict(kind, kind)
	}
	peer := KindBackend
	if kind == KindBackend {
		peer = KindMutation
	}
	_, other, err := s.owner(ctx, peer)
	if err != nil {
		return result, err
	}
	if other.Held && !(staging && peer == KindBackend) {
		return result, s.conflict(peer, kind)
	}
	created, err := state.NewSystemPIDProbe().Identity(ctx, uint32(os.Getpid()))
	if err != nil {
		return result, err
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return result, err
	}
	owner := posixOwner{PID: uint32(os.Getpid()), Created: created, Token: hex.EncodeToString(nonce[:])}
	data, err := json.Marshal(owner)
	if err != nil {
		return result, err
	}
	if err = unix.Fsetxattr(int(s.root.Fd()), ownerAttribute(kind), data, 0); err != nil {
		return result, err
	}
	lease := &Lease{set: s, kind: kind, token: owner.Token}
	s.lease = lease
	return AcquisitionResult{lease: lease, peerProbe: other, recovered: primary.Recovered}, nil
}

// AcquireBackend 独占 backend 租约并排除 mutation。
func (s *Set) AcquireBackend(ctx context.Context) (AcquisitionResult, error) {
	return s.acquire(ctx, KindBackend, false)
}

// AcquireMutation 独占 mutation 租约并排除 backend。
func (s *Set) AcquireMutation(ctx context.Context) (AcquisitionResult, error) {
	return s.acquire(ctx, KindMutation, false)
}

// AcquireStaging 允许独立暂存操作与已有后端并存。
func (s *Set) AcquireStaging(ctx context.Context) (AcquisitionResult, error) {
	return s.acquire(ctx, KindMutation, true)
}

// Probe 在根 inode 门闩内获取对端存活事实。
func (s *Set) Probe(ctx context.Context, kind Kind) (result ProbeResult, returnErr error) {
	if !kind.Valid() {
		return result, filesystem.ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.gate(ctx); err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, s.ungate()) }()
	_, result, returnErr = s.owner(ctx, kind)
	return result, returnErr
}
func (s *Set) release(lease *Lease) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.gate(ctx); err != nil {
		return err
	}
	owner, _, err := s.owner(ctx, lease.kind)
	if err == nil && owner.Token != lease.token {
		err = filesystem.ErrIdentityChanged
	}
	if err == nil {
		err = unix.Fsetxattr(int(s.root.Fd()), ownerAttribute(lease.kind), nil, 0)
	}
	if err == nil {
		s.lease = nil
	}
	return errors.Join(err, s.ungate())
}

// Close 释放本实例租约及根句柄，其他实例的租约不受影响。
func (s *Set) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	var err error
	if s.lease != nil {
		err = s.release(s.lease)
	}
	s.closed = true
	return errors.Join(err, s.root.Close())
}

// Lease 返回成功获取的租约。
func (r AcquisitionResult) Lease() *Lease { return r.lease }

// Recovered 报告发现了已经死亡的上次所有者。
func (r AcquisitionResult) Recovered() bool { return r.recovered }

// PeerProbe 返回获取时的对端快照。
func (r AcquisitionResult) PeerProbe() ProbeResult { return r.peerProbe }

// Kind 返回租约用途。
func (l *Lease) Kind() Kind { return l.kind }

// Name 返回不含目录信息的锁名称。
func (l *Lease) Name() string { return string(l.kind) }

// Close 幂等释放随机 token 对应的租约。
func (l *Lease) Close() error {
	l.once.Do(func() {
		s := l.set
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.closed {
			l.err = s.release(l)
		}
	})
	return l.err
}
