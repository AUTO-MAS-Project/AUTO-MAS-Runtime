//go:build linux

package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// New 创建只消费封闭布局和类别的受控文件操作器。
func New(ctx context.Context, layout *config.Layout, auditor Auditor, values ...Option) (*Operator, error) {
	if ctx == nil || layout == nil || auditor == nil {
		return nil, ErrInvalidArgument
	}
	options := options{wait: defaultWait, delays: defaultRenameDelays()}
	for _, value := range values {
		if value == nil {
			return nil, ErrInvalidArgument
		}
		if err := value(&options); err != nil {
			return nil, err
		}
	}
	c, err := managedPOSIX(ctx, layout, layout.AppRoot(), false)
	if err != nil {
		return nil, err
	}
	if err = c.close(); err != nil {
		return nil, err
	}
	return &Operator{layout: layout, auditor: auditor, wait: options.wait, delays: options.delays}, nil
}

// RemoveTree 先校验封闭目标并审计，再按目录句柄递归删除。
func (o *Operator) RemoveTree(ctx context.Context, r DeleteRequest) (DeleteResult, error) {
	if o == nil || ctx == nil || !r.Kind.Valid() || validateAuditValue(r.OperationID) != nil || validateAuditValue(r.Reason) != nil {
		return DeleteResult{}, ErrInvalidArgument
	}
	expected, exact, err := o.expectedDeletePath(r)
	if err != nil {
		return DeleteResult{}, err
	}
	if !filepath.IsAbs(r.Target) || !within(o.layout.AppRoot(), r.Target) || filepath.Clean(r.Target) == o.layout.AppRoot() {
		return DeleteResult{}, &Error{code: protocol.CodePathOutsideManagedRoot, Operation: "authorize-delete", Path: r.Target, Err: ErrIdentityChanged}
	}
	if exact && filepath.Clean(expected) != filepath.Clean(r.Target) || !exact && (!within(o.layout.RepoDir(), r.Target) || filepath.Base(r.Target) != "__pycache__") {
		return DeleteResult{}, ErrIdentityChanged
	}
	for _, p := range append(o.layout.ProtectedRootDirs(), o.layout.LogsDir(), o.layout.StateDir()) {
		if within(p, r.Target) || within(r.Target, p) {
			return DeleteResult{}, ErrIdentityChanged
		}
	}
	c, err := managedPOSIX(ctx, o.layout, filepath.Dir(r.Target), false)
	if errors.Is(err, os.ErrNotExist) {
		return DeleteResult{}, nil
	}
	if err != nil {
		return DeleteResult{}, err
	}
	id, exists, err := posixEntryIdentity(c.leaf(), filepath.Base(r.Target))
	if err != nil {
		return DeleteResult{}, errors.Join(err, c.close())
	}
	if !exists {
		return DeleteResult{}, c.close()
	}
	if r.ExpectedIdentity != nil && !r.ExpectedIdentity.Equal(&id) {
		return DeleteResult{}, errors.Join(ErrIdentityChanged, c.close())
	}
	if id.attributes&unix.S_IFMT != unix.S_IFDIR {
		return DeleteResult{}, errors.Join(ErrIdentityChanged, c.close())
	}
	record := DeleteAuditRecord{Phase: DeleteAuditStarted, Kind: r.Kind, Target: r.Target, OperationID: r.OperationID, Reason: r.Reason}
	if err = o.auditor.RecordDeletion(ctx, record); err != nil {
		return DeleteResult{}, errors.Join(&AuditError{Phase: DeleteAuditStarted, Cause: err}, c.close())
	}
	err = c.verify(ctx)
	if err == nil {
		err = removePOSIXTree(
			ctx,
			c.leaf(),
			filepath.Base(r.Target),
			id,
			c.ids[len(c.ids)-1].volumeSerial,
			r.Kind == DeleteUVCache,
			func() error { return c.verify(ctx) },
		)
	}
	result := DeleteResult{Removed: err == nil, Partial: err != nil}
	record.Phase = DeleteAuditFinished
	record.Removed = result.Removed
	record.Partial = result.Partial
	if err == nil {
		record.Result = "removed"
	} else {
		record.Result = "failed"
	}
	auditErr := o.auditor.RecordDeletion(context.WithoutCancel(ctx), record)
	result.AuditCompleted = auditErr == nil
	return result, errors.Join(err, auditErr, c.close())
}
func posixEntryIdentity(parent *os.File, name string) (DirectoryIdentity, bool, error) {
	var st unix.Stat_t
	err := unix.Fstatat(int(parent.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return DirectoryIdentity{}, false, nil
	}
	if err != nil {
		return DirectoryIdentity{}, false, err
	}
	return posixIdentityFromStat(&st).directory, true, nil
}
func removePOSIXTree(
	ctx context.Context,
	parent *os.File,
	name string,
	expected DirectoryIdentity,
	device uint64,
	removeLeafLinks bool,
	verify func() error,
) error {
	if err := verify(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	id, exists, err := posixEntryIdentity(parent, name)
	if err != nil {
		return err
	}
	if !exists || !expected.Equal(&id) || id.volumeSerial != device {
		return ErrIdentityChanged
	}
	mode := id.attributes & unix.S_IFMT
	if mode == unix.S_IFREG {
		if id.numberOfLinks != 1 {
			return ErrUnsafeHardLink
		}
		return unix.Unlinkat(int(parent.Fd()), name, 0)
	}
	if mode == unix.S_IFLNK && removeLeafLinks {
		// uv 缓存用链接引用 archive；只删除已核验链接本身，目标永不进入遍历。
		return unix.Unlinkat(int(parent.Fd()), name, 0)
	}
	if mode != unix.S_IFDIR {
		return ErrIdentityChanged
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(fd), name)
	actual, err := posixIdentity(dir)
	if err == nil && !expected.Equal(&actual) {
		err = ErrIdentityChanged
	}
	var names []string
	if err == nil {
		names, err = dir.Readdirnames(-1)
	}
	for _, child := range names {
		if err != nil {
			break
		}
		id, _, e := posixEntryIdentity(dir, child)
		err = e
		if err == nil {
			err = removePOSIXTree(ctx, dir, child, id, device, removeLeafLinks, func() error {
				if err := verify(); err != nil {
					return err
				}
				current, exists, err := posixEntryIdentity(parent, name)
				if err != nil {
					return err
				}
				if !exists || !expected.Equal(&current) {
					return ErrIdentityChanged
				}
				return nil
			})
		}
	}
	err = errors.Join(err, dir.Close())
	if err != nil {
		return err
	}
	actual, exists, err = posixEntryIdentity(parent, name)
	if err != nil {
		return err
	}
	if !exists || !expected.Equal(&actual) {
		return ErrIdentityChanged
	}
	return unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
}

// AtomicRename 只允许布局定义的同设备迁移，目标存在时失败关闭。
func (o *Operator) AtomicRename(ctx context.Context, r RenameRequest) (RenameResult, error) {
	if o == nil || ctx == nil || !r.Kind.Valid() || validateAuditValue(r.OperationID) != nil || validateAuditValue(r.Reason) != nil {
		return RenameResult{}, ErrInvalidArgument
	}
	source, destination, err := o.expectedRenamePaths(r)
	if err != nil {
		return RenameResult{}, err
	}
	if source != filepath.Clean(r.Source) || destination != filepath.Clean(r.Destination) {
		return RenameResult{}, ErrIdentityChanged
	}
	a, err := managedPOSIX(ctx, o.layout, filepath.Dir(source), false)
	if err != nil {
		return RenameResult{}, err
	}
	b, err := managedPOSIX(ctx, o.layout, filepath.Dir(destination), false)
	if err != nil {
		return RenameResult{}, errors.Join(err, a.close())
	}
	id, exists, err := posixEntryIdentity(a.leaf(), filepath.Base(source))
	if err == nil && (!exists || id.attributes&unix.S_IFMT != unix.S_IFDIR || id.volumeSerial != b.ids[len(b.ids)-1].volumeSerial || r.ExpectedSourceIdentity != nil && !r.ExpectedSourceIdentity.Equal(&id)) {
		err = ErrIdentityChanged
	}
	if err == nil {
		err = errors.Join(a.verify(ctx), b.verify(ctx))
	}
	if err == nil {
		err = renamePOSIXNoReplace(int(a.leaf().Fd()), filepath.Base(source), int(b.leaf().Fd()), filepath.Base(destination))
	}
	applied := err == nil
	if applied {
		err = errors.Join(a.leaf().Sync(), b.leaf().Sync())
	}
	if errors.Is(err, unix.EEXIST) {
		err = errors.Join(ErrDestinationExists, err)
	}
	return RenameResult{MutationApplied: applied}, errors.Join(err, a.close(), b.close())
}
