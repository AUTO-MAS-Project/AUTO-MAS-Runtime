package filesystem

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

const cstrEqual int32 = 2

// CanonicalPath 保存 Windows 路径的显示形式、原生形式和结构身份。
type CanonicalPath struct {
	display       string
	native        string
	comparisonKey string
	volumeKey     string
}

// DirectoryInspection 描述受管目录的只读存在性事实。
type DirectoryInspection struct {
	Exists   bool
	Identity *DirectoryIdentity
}

type objectIdentity = DirectoryIdentity

func matchesDirectoryIdentity(expected *DirectoryIdentity, actual objectIdentity) bool {
	return expected.Equal(&actual)
}

type directoryEntry struct {
	name       string
	attributes uint32
}

type openSpec struct {
	access    uint32
	share     uint32
	creation  uint32
	options   uint32
	directory bool
}

type ntCreateSpec struct {
	desiredAccess     uint32
	shareAccess       uint32
	createDisposition uint32
	createOptions     uint32
}

type stateDispositionSpec struct {
	informationClass uint32
	flags            uint32
}

type pathAPI struct {
	makeDirectory       func(path string) error
	attributes          func(path string) (uint32, error)
	openPath            func(path string, spec openSpec) (windows.Handle, error)
	openRelative        func(parent windows.Handle, name string, spec openSpec) (windows.Handle, error)
	ntCreateRelative    func(parent windows.Handle, name string, spec ntCreateSpec) (windows.Handle, error)
	finalPath           func(handle windows.Handle) (string, error)
	identity            func(handle windows.Handle) (objectIdentity, error)
	caseSensitive       func(handle windows.Handle) (bool, error)
	driveType           func(root string) (uint32, error)
	duplicateHandle     func(handle windows.Handle) (windows.Handle, error)
	listDirectory       func(handle windows.Handle) ([]directoryEntry, error)
	readFile            func(handle windows.Handle, buffer []byte) (int, error)
	writeFile           func(handle windows.Handle, buffer []byte) (int, error)
	flushFile           func(handle windows.Handle) error
	setDisposition      func(handle windows.Handle) error
	setStateDisposition func(handle windows.Handle, spec stateDispositionSpec) error
	rename              func(source windows.Handle, targetParent windows.Handle, name string, replace bool) error
	renameState         func(source windows.Handle, targetParent windows.Handle, name string, flags uint32) error
	closeHandle         func(handle windows.Handle) error
}

type pinnedObject struct {
	path     CanonicalPath
	handle   windows.Handle
	identity objectIdentity
}

type pinnedChain struct {
	api     pathAPI
	objects []pinnedObject
}

// Canonicalize 将绝对 Windows 路径转换为失败关闭的规范值对象。
func Canonicalize(path string) (CanonicalPath, error) {
	return canonicalizeContextWith(context.Background(), path, newProductionPathAPI())
}

// InspectManagedDirectory 校验目录路径链不含重解析点，并返回目标是否存在。
//
// 该能力只读且不创建目录；不存在的末端允许保留，但所有已存在的祖先仍必须
// 通过同一组句柄校验。调用方应把任何错误视为无法证明目录身份。
func InspectManagedDirectory(
	ctx context.Context,
	layout *config.Layout,
	path string,
) (DirectoryInspection, error) {
	return inspectManagedDirectoryWith(ctx, layout, path, newProductionPathAPI())
}

func inspectManagedDirectoryWith(
	ctx context.Context,
	layout *config.Layout,
	path string,
	api pathAPI,
) (DirectoryInspection, error) {
	if ctx == nil || layout == nil || path == "" || !api.valid() {
		return DirectoryInspection{}, fmt.Errorf("%w: invalid managed-directory input", ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return DirectoryInspection{}, err
	}
	target, err := canonicalizeContextWith(ctx, path, api)
	if err != nil {
		return DirectoryInspection{}, err
	}
	attributes, attributesErr := api.attributes(target.Native())
	exists := attributesErr == nil
	if attributesErr != nil && !isWindowsNotFound(attributesErr) {
		return DirectoryInspection{}, &FileError{
			Operation: "attributes",
			Path:      target.String(),
			Err:       attributesErr,
		}
	}
	if exists && attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return DirectoryInspection{}, &FileError{
			Operation: "type",
			Path:      target.String(),
			Err:       ErrIdentityChanged,
		}
	}
	if err := validateExistingLayoutPathWith(ctx, layout, path, api); err != nil {
		return DirectoryInspection{}, err
	}
	if !exists {
		if err := rejectMissingReparseChain(ctx, target, api); err != nil {
			return DirectoryInspection{}, err
		}
		return DirectoryInspection{Exists: false}, nil
	}
	identity, err := inspectExistingDirectoryIdentity(ctx, layout, target, api)
	if err != nil {
		return DirectoryInspection{}, err
	}
	return DirectoryInspection{Exists: true, Identity: &identity}, nil
}

func inspectExistingDirectoryIdentity(
	ctx context.Context,
	layout *config.Layout,
	target CanonicalPath,
	api pathAPI,
) (objectIdentity, error) {
	root, err := canonicalizeContextWith(ctx, layout.AppRoot(), api)
	if err != nil {
		return objectIdentity{}, err
	}
	if root.Equal(target) {
		handle, err := api.openPath(target.Native(), directoryPinSpec())
		if err != nil {
			return objectIdentity{}, &FileError{Operation: "open-directory-identity", Path: target.String(), Err: err}
		}
		identity, identityErr := api.identity(handle)
		if identityErr != nil {
			return objectIdentity{}, errors.Join(
				identityErr,
				wrapFileError("close", target.String(), api.closeHandle(handle)),
			)
		}
		if err := validatePinnedDirectory(ctx, pinnedObject{path: target, handle: handle, identity: identity}, api); err != nil {
			return objectIdentity{}, errors.Join(err, api.closeHandle(handle))
		}
		if err := api.closeHandle(handle); err != nil {
			return objectIdentity{}, wrapFileError("close", target.String(), err)
		}
		return identity, nil
	}
	chain, err := openPinnedChainWith(ctx, root, target, directoryPinSpec(), api)
	if err != nil {
		return objectIdentity{}, err
	}
	identity := chain.objects[len(chain.objects)-1].identity
	if err := chain.close(); err != nil {
		return objectIdentity{}, err
	}
	return identity, nil
}

// rejectMissingReparseChain 检查 GetFileAttributes 无法分辨的悬空重解析点。
//
// Win32 属性查询会跟随符号链接；当链接目标已消失时，它会把实际存在的链接
// 报成 ERROR_FILE_NOT_FOUND。逐级以 OPEN_REPARSE_POINT 探测原始对象，避免把
// 该对象当成安全的不存在末端交给后续 clone/rename 流程。
func rejectMissingReparseChain(
	ctx context.Context,
	target CanonicalPath,
	api pathAPI,
) error {
	current := target.String()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var probeErr error
		for _, directory := range []bool{true, false} {
			handle, err := api.openPath(nativeWindowsPath(current), reparseProbeSpec(directory))
			if err != nil {
				if !isWindowsNotFound(err) {
					probeErr = err
				}
				continue
			}
			identity, identityErr := api.identity(handle)
			closeErr := api.closeHandle(handle)
			if identityErr != nil || closeErr != nil {
				return errors.Join(
					wrapFileError("identify-reparse-probe", current, identityErr),
					wrapFileError("close-reparse-probe", current, closeErr),
				)
			}
			if identity.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
				return unsafeReparseError(current)
			}
			return nil
		}
		if probeErr != nil {
			return &FileError{Operation: "probe-reparse", Path: current, Err: probeErr}
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

func reparseProbeSpec(directory bool) openSpec {
	return openSpec{
		access:    windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE,
		share:     windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE,
		creation:  windows.OPEN_EXISTING,
		options:   windows.FILE_FLAG_BACKUP_SEMANTICS | windows.FILE_FLAG_OPEN_REPARSE_POINT,
		directory: directory,
	}
}

func canonicalizeContextWith(
	ctx context.Context,
	path string,
	api pathAPI,
) (CanonicalPath, error) {
	if ctx == nil {
		return CanonicalPath{}, fmt.Errorf("%w: context is nil", ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return CanonicalPath{}, err
	}
	if !api.valid() {
		return CanonicalPath{}, fmt.Errorf("%w: incomplete path api", ErrInvalidArgument)
	}
	cleaned, err := cleanAbsoluteWindowsPath(path)
	if err != nil {
		return CanonicalPath{}, err
	}

	existing := cleaned
	tail := make([]string, 0, 4)
	var attributes uint32
	for {
		if err := ctx.Err(); err != nil {
			return CanonicalPath{}, err
		}
		attributes, err = api.attributes(nativeWindowsPath(existing))
		if err == nil {
			break
		}
		if !isWindowsNotFound(err) {
			return CanonicalPath{}, &FileError{Operation: "attributes", Path: existing, Err: err}
		}
		volume := filepath.VolumeName(existing)
		if isWindowsVolumeRoot(existing, volume) {
			return CanonicalPath{}, &FileError{Operation: "attributes", Path: existing, Err: err}
		}
		name := filepath.Base(existing)
		if err := validateNonexistentComponent(name); err != nil {
			return CanonicalPath{}, err
		}
		tail = append(tail, name)
		parent := filepath.Dir(existing)
		if parent == existing {
			return CanonicalPath{}, fmt.Errorf("%w: path has no existing prefix", ErrInvalidArgument)
		}
		existing = parent
	}

	spec := openSpec{
		access:    windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE,
		share:     windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE,
		creation:  windows.OPEN_EXISTING,
		options:   windows.FILE_FLAG_BACKUP_SEMANTICS | windows.FILE_FLAG_OPEN_REPARSE_POINT,
		directory: attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0,
	}
	if err := ctx.Err(); err != nil {
		return CanonicalPath{}, err
	}
	handle, err := api.openPath(nativeWindowsPath(existing), spec)
	if err != nil {
		return CanonicalPath{}, &FileError{Operation: "open-canonical-prefix", Path: existing, Err: err}
	}
	if err := ctx.Err(); err != nil {
		return CanonicalPath{}, joinOperationCleanup(
			err,
			wrapFileError("close", existing, api.closeHandle(handle)),
		)
	}
	finalPath, finalErr := api.finalPath(handle)
	closeErr := api.closeHandle(handle)
	if finalErr != nil || closeErr != nil {
		return CanonicalPath{}, joinOperationCleanup(
			wrapFileError("final-path", existing, finalErr),
			wrapFileError("close", existing, closeErr),
		)
	}

	display, err := displayWindowsPath(finalPath)
	if err != nil {
		return CanonicalPath{}, err
	}
	for i := len(tail) - 1; i >= 0; i-- {
		display = filepath.Join(display, tail[i])
	}
	comparisonKey := structuralWindowsPath(display)
	volume := filepath.VolumeName(comparisonKey)
	if volume == "" {
		return CanonicalPath{}, fmt.Errorf("%w: path has no volume", ErrInvalidArgument)
	}
	volumeKey := structuralWindowsPath(volume)
	return CanonicalPath{
		display:       display,
		native:        nativeWindowsPath(display),
		comparisonKey: comparisonKey,
		volumeKey:     volumeKey,
	}, nil
}

func cleanAbsoluteWindowsPath(path string) (string, error) {
	if path == "" || strings.ContainsRune(path, '\x00') {
		return "", fmt.Errorf("%w: path is empty or contains nul", ErrInvalidArgument)
	}
	normalized, err := displayWindowsPath(strings.ReplaceAll(path, "/", `\`))
	if err != nil {
		return "", err
	}
	upper := strings.ToUpper(normalized)
	if strings.HasPrefix(upper, `\\.\`) ||
		strings.HasPrefix(upper, `\??\`) ||
		strings.HasPrefix(upper, `\\?\GLOBALROOT\`) ||
		strings.HasPrefix(upper, `\\?\VOLUME{`) {
		return "", fmt.Errorf("%w: device path is not allowed", ErrInvalidArgument)
	}
	volume := filepath.VolumeName(normalized)
	if volume == "" || !filepath.IsAbs(normalized) {
		return "", fmt.Errorf("%w: path is not absolute", ErrInvalidArgument)
	}
	if hasInteriorEmptyComponent(normalized, volume) {
		return "", fmt.Errorf("%w: path contains an empty component", ErrInvalidArgument)
	}
	if err := validateAbsoluteComponents(normalized, volume); err != nil {
		return "", err
	}
	return filepath.Clean(normalized), nil
}

func displayWindowsPath(path string) (string, error) {
	switch {
	case strings.HasPrefix(path, `\\?\UNC\`):
		return `\\` + strings.TrimPrefix(path, `\\?\UNC\`), nil
	case strings.HasPrefix(path, `\\?\`):
		withoutPrefix := strings.TrimPrefix(path, `\\?\`)
		if len(filepath.VolumeName(withoutPrefix)) != 2 {
			return "", fmt.Errorf("%w: unsupported extended path", ErrInvalidArgument)
		}
		return withoutPrefix, nil
	case strings.HasPrefix(path, `\\.\`):
		return "", fmt.Errorf("%w: device path is not allowed", ErrInvalidArgument)
	default:
		return path, nil
	}
}

func nativeWindowsPath(path string) string {
	if strings.HasPrefix(path, `\\?\`) {
		return path
	}
	if strings.HasPrefix(path, `\\`) {
		return `\\?\UNC\` + strings.TrimPrefix(path, `\\`)
	}
	return `\\?\` + path
}

func structuralWindowsPath(path string) string {
	cleaned := filepath.Clean(strings.ReplaceAll(path, "/", `\`))
	volume := filepath.VolumeName(cleaned)
	remainder := strings.Trim(strings.TrimPrefix(cleaned, volume), `\`)
	if remainder == "" {
		return strings.TrimRight(volume, `\`) + `\`
	}
	return strings.TrimRight(cleaned, `\`)
}

func hasInteriorEmptyComponent(path, volume string) bool {
	remainder := strings.TrimPrefix(path, volume)
	remainder = strings.TrimPrefix(remainder, `\`)
	remainder = strings.TrimSuffix(remainder, `\`)
	return strings.Contains(remainder, `\\`)
}

func validateAbsoluteComponents(path, volume string) error {
	remainder := strings.Trim(strings.TrimPrefix(path, volume), `\`)
	if remainder == "" {
		return nil
	}
	for _, name := range strings.Split(remainder, `\`) {
		if err := validateNonexistentComponent(name); err != nil {
			return err
		}
	}
	return nil
}

func validateNonexistentComponent(name string) error {
	if name == "" || name == "." || name == ".." ||
		strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") ||
		strings.ContainsAny(name, `<>:"/\|?*`) || isReservedWindowsName(name) {
		return fmt.Errorf("%w: unsafe nonexistent path component %q", ErrInvalidArgument, name)
	}
	return nil
}

func isReservedWindowsName(name string) bool {
	base := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if len(base) != 4 || base[3] < '1' || base[3] > '9' {
		return false
	}
	return base[:3] == "COM" || base[:3] == "LPT"
}

func isWindowsNotFound(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}

func isWindowsVolumeRoot(path, volume string) bool {
	if volume == "" {
		return false
	}
	result, err := compareStringOrdinal(
		structuralWindowsPath(path),
		structuralWindowsPath(volume+`\`),
		true,
	)
	return err == nil && result == cstrEqual
}

func wrapFileError(operation, path string, err error) error {
	if err == nil {
		return nil
	}
	return &FileError{Operation: operation, Path: path, Err: err}
}

func (p CanonicalPath) String() string { return p.display }

// Native 返回适合 Win32 长路径调用的扩展路径形式。
func (p CanonicalPath) Native() string { return p.native }

// Equal 使用 Windows ordinal 大小写无关规则判断同一规范路径。
func (p CanonicalPath) Equal(other CanonicalPath) bool {
	if p.comparisonKey == "" || other.comparisonKey == "" {
		return false
	}
	volumeResult, err := compareStringOrdinal(p.volumeKey, other.volumeKey, true)
	if err != nil || volumeResult != cstrEqual {
		return false
	}
	pathResult, err := compareStringOrdinal(p.comparisonKey, other.comparisonKey, true)
	return err == nil && pathResult == cstrEqual
}

// Contains 使用 volume 和完整组件边界判断严格后代关系。
func (p CanonicalPath) Contains(other CanonicalPath) bool {
	if p.comparisonKey == "" || other.comparisonKey == "" {
		return false
	}
	volumeResult, err := compareStringOrdinal(p.volumeKey, other.volumeKey, true)
	if err != nil || volumeResult != cstrEqual {
		return false
	}
	parent := canonicalComponents(p.comparisonKey)
	child := canonicalComponents(other.comparisonKey)
	if len(child) <= len(parent) {
		return false
	}
	for i := range parent {
		result, err := compareStringOrdinal(parent[i], child[i], true)
		if err != nil || result != cstrEqual {
			return false
		}
	}
	return true
}

func compareStringOrdinal(left, right string, ignoreCase bool) (int32, error) {
	leftUTF16, err := windows.UTF16FromString(left)
	if err != nil {
		return 0, fmt.Errorf("encode left ordinal string: %w", err)
	}
	rightUTF16, err := windows.UTF16FromString(right)
	if err != nil {
		return 0, fmt.Errorf("encode right ordinal string: %w", err)
	}
	ignoreCaseValue := uintptr(0)
	if ignoreCase {
		ignoreCaseValue = 1
	}
	procedure := windows.NewLazySystemDLL("kernel32.dll").NewProc("CompareStringOrdinal")
	result, _, callErr := procedure.Call(
		uintptr(unsafe.Pointer(&leftUTF16[0])),
		uintptr(int32(len(leftUTF16)-1)),
		uintptr(unsafe.Pointer(&rightUTF16[0])),
		uintptr(int32(len(rightUTF16)-1)),
		ignoreCaseValue,
	)
	if result == 0 {
		if errors.Is(callErr, windows.ERROR_SUCCESS) {
			callErr = errors.New("compare string ordinal returned no result")
		}
		return 0, fmt.Errorf("compare string ordinal: %w", callErr)
	}
	return int32(result), nil
}

func canonicalComponents(path string) []string {
	volume := filepath.VolumeName(path)
	remainder := strings.Trim(strings.TrimPrefix(path, volume), `\`)
	if remainder == "" {
		return nil
	}
	return strings.Split(remainder, `\`)
}

func (a pathAPI) valid() bool {
	return a.makeDirectory != nil &&
		a.attributes != nil &&
		a.openPath != nil &&
		a.openRelative != nil &&
		a.ntCreateRelative != nil &&
		a.finalPath != nil &&
		a.identity != nil &&
		a.caseSensitive != nil &&
		a.driveType != nil &&
		a.duplicateHandle != nil &&
		a.listDirectory != nil &&
		a.readFile != nil &&
		a.writeFile != nil &&
		a.flushFile != nil &&
		a.setDisposition != nil &&
		a.setStateDisposition != nil &&
		a.rename != nil &&
		a.renameState != nil &&
		a.closeHandle != nil
}

func newProductionPathAPI() pathAPI {
	return pathAPI{
		makeDirectory:       makeDirectoryWindows,
		attributes:          attributesWindows,
		openPath:            openPathWindows,
		openRelative:        openRelativeWindows,
		ntCreateRelative:    ntCreateRelativeWindows,
		finalPath:           finalPathWindows,
		identity:            identityWindows,
		caseSensitive:       caseSensitiveWindows,
		driveType:           driveTypeWindows,
		duplicateHandle:     duplicateHandleWindows,
		listDirectory:       listDirectoryWindows,
		readFile:            readFileWindows,
		writeFile:           writeFileWindows,
		flushFile:           windows.FlushFileBuffers,
		setDisposition:      setDispositionWindows,
		setStateDisposition: setStateDispositionWindows,
		rename:              renameWindows,
		renameState:         renameStateWindows,
		closeHandle:         windows.CloseHandle,
	}
}

func makeDirectoryWindows(path string) error {
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("encode directory path: %w", err)
	}
	if err := windows.CreateDirectory(pathUTF16, nil); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	return nil
}

func attributesWindows(path string) (uint32, error) {
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("encode attribute path: %w", err)
	}
	attributes, err := windows.GetFileAttributes(pathUTF16)
	if err != nil {
		return 0, fmt.Errorf("get file attributes: %w", err)
	}
	return attributes, nil
}

func openPathWindows(path string, spec openSpec) (windows.Handle, error) {
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, fmt.Errorf("encode open path: %w", err)
	}
	handle, err := windows.CreateFile(
		pathUTF16,
		spec.access,
		spec.share,
		nil,
		spec.creation,
		spec.options,
		0,
	)
	if err != nil {
		return windows.InvalidHandle, fmt.Errorf("create file: %w", err)
	}
	return handle, nil
}

type ntUnicodeString struct {
	length        uint16
	maximumLength uint16
	buffer        *uint16
}

type ntObjectAttributes struct {
	length                   uint32
	rootDirectory            windows.Handle
	objectName               *ntUnicodeString
	attributes               uint32
	securityDescriptor       uintptr
	securityQualityOfService uintptr
}

type ntIOStatusBlock struct {
	status      uintptr
	information uintptr
}

const (
	ntObjectCaseInsensitive   = uint32(0x00000040)
	ntFileOpen                = uint32(0x00000001)
	ntFileCreate              = uint32(0x00000002)
	ntFileOpenIf              = uint32(0x00000003)
	ntFileOverwrite           = uint32(0x00000004)
	ntFileOverwriteIf         = uint32(0x00000005)
	ntFileDirectoryFile       = uint32(0x00000001)
	ntFileSynchronousNonalert = uint32(0x00000020)
	ntFileNonDirectoryFile    = uint32(0x00000040)
	ntFileOpenReparsePoint    = uint32(0x00200000)
	fileAttributeNormal       = uint32(0x00000080)
	fileNameNormalized        = uint32(0)
	volumeNameDOS             = uint32(0)
	fileIDBothDirectoryInfo   = uint32(10)
	fileIDBothDirectoryStart  = uint32(11)
	fileBasicInfoClass        = uint32(0)
	fileStandardInfoClass     = uint32(1)
	fileIDInfoClass           = uint32(18)
	fileDispositionInfoClass  = uint32(4)
	fileDispositionExClass    = uint32(21)
	fileCaseSensitiveClass    = uint32(23)
	fileDispositionDelete     = uint32(0x00000001)
	fileDispositionPOSIX      = uint32(0x00000002)
	fileDispositionIgnoreRead = uint32(0x00000010)
	fileCaseSensitiveDir      = uint32(0x00000001)
)

func openRelativeWindows(
	parent windows.Handle,
	name string,
	spec openSpec,
) (windows.Handle, error) {
	if spec.access&windows.SYNCHRONIZE == 0 {
		return windows.InvalidHandle, fmt.Errorf(
			"%w: synchronous relative open requires SYNCHRONIZE",
			ErrInvalidArgument,
		)
	}
	disposition, err := ntDisposition(spec.creation)
	if err != nil {
		return windows.InvalidHandle, err
	}
	options := ntFileNonDirectoryFile
	if spec.directory {
		options = ntFileDirectoryFile
	}
	if spec.options&windows.FILE_FLAG_OPEN_REPARSE_POINT != 0 {
		options |= ntFileOpenReparsePoint
	}
	options |= ntFileSynchronousNonalert
	return ntCreateFileRelative(
		parent,
		name,
		spec.access,
		spec.share,
		disposition,
		options,
	)
}

func ntCreateRelativeWindows(
	parent windows.Handle,
	name string,
	spec ntCreateSpec,
) (windows.Handle, error) {
	return ntCreateFileRelative(
		parent,
		name,
		spec.desiredAccess,
		spec.shareAccess,
		spec.createDisposition,
		spec.createOptions,
	)
}

func ntCreateFileRelative(
	parent windows.Handle,
	name string,
	desiredAccess uint32,
	shareAccess uint32,
	createDisposition uint32,
	createOptions uint32,
) (windows.Handle, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return windows.InvalidHandle, fmt.Errorf("%w: invalid relative leaf", ErrInvalidArgument)
	}
	nameUTF16, err := windows.UTF16FromString(name)
	if err != nil {
		return windows.InvalidHandle, fmt.Errorf("encode relative leaf: %w", err)
	}
	nameBytes := (len(nameUTF16) - 1) * 2
	if nameBytes > math.MaxUint16 {
		return windows.InvalidHandle, fmt.Errorf("%w: relative leaf is too long", ErrInvalidArgument)
	}
	unicodeName := ntUnicodeString{
		length:        uint16(nameBytes),
		maximumLength: uint16(len(nameUTF16) * 2),
		buffer:        &nameUTF16[0],
	}
	attributes := ntObjectAttributes{
		length:        uint32(unsafe.Sizeof(ntObjectAttributes{})),
		rootDirectory: parent,
		objectName:    &unicodeName,
		attributes:    ntObjectCaseInsensitive,
	}
	var handle windows.Handle
	var status ntIOStatusBlock
	procedure := windows.NewLazySystemDLL("ntdll.dll").NewProc("NtCreateFile")
	result, _, _ := procedure.Call(
		uintptr(unsafe.Pointer(&handle)),
		uintptr(desiredAccess),
		uintptr(unsafe.Pointer(&attributes)),
		uintptr(unsafe.Pointer(&status)),
		0,
		uintptr(fileAttributeNormal),
		uintptr(shareAccess),
		uintptr(createDisposition),
		uintptr(createOptions),
		0,
		0,
	)
	if ntStatus := uint32(result); ntStatus != 0 {
		return windows.InvalidHandle, fmt.Errorf("nt create file: %w", ntStatusError(ntStatus))
	}
	return handle, nil
}

func ntDisposition(creation uint32) (uint32, error) {
	switch creation {
	case windows.CREATE_NEW:
		return ntFileCreate, nil
	case windows.CREATE_ALWAYS:
		return ntFileOverwriteIf, nil
	case windows.OPEN_EXISTING:
		return ntFileOpen, nil
	case windows.OPEN_ALWAYS:
		return ntFileOpenIf, nil
	case windows.TRUNCATE_EXISTING:
		return ntFileOverwrite, nil
	default:
		return 0, fmt.Errorf("%w: unsupported creation disposition", ErrInvalidArgument)
	}
}

func ntStatusError(status uint32) error {
	procedure := windows.NewLazySystemDLL("ntdll.dll").NewProc("RtlNtStatusToDosError")
	code, _, _ := procedure.Call(uintptr(status))
	if code == 0 {
		return syscall.Errno(status)
	}
	return syscall.Errno(code)
}

func finalPathWindows(handle windows.Handle) (string, error) {
	procedure := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetFinalPathNameByHandleW")
	buffer := make([]uint16, 512)
	for {
		length, _, callErr := procedure.Call(
			uintptr(handle),
			uintptr(unsafe.Pointer(&buffer[0])),
			uintptr(len(buffer)),
			uintptr(fileNameNormalized|volumeNameDOS),
		)
		if length == 0 {
			if errors.Is(callErr, windows.ERROR_SUCCESS) {
				callErr = errors.New("get final path returned no data")
			}
			return "", fmt.Errorf("get final path: %w", callErr)
		}
		if length < uintptr(len(buffer)) {
			return windows.UTF16ToString(buffer[:length]), nil
		}
		buffer = make([]uint16, int(length)+1)
	}
}

type fileBasicInfo struct {
	creationTime   int64
	lastAccessTime int64
	lastWriteTime  int64
	changeTime     int64
	attributes     uint32
	_              uint32
}

type fileStandardInfo struct {
	allocationSize int64
	endOfFile      int64
	numberOfLinks  uint32
	deletePending  byte
	directory      byte
	_              [2]byte
}

type fileIDInfo struct {
	volumeSerial uint64
	fileID       [16]byte
}

type fileCaseSensitiveInfo struct {
	flags uint32
}

func identityWindows(handle windows.Handle) (objectIdentity, error) {
	var basic fileBasicInfo
	if err := getFileInformationWindows(
		handle,
		fileBasicInfoClass,
		unsafe.Pointer(&basic),
		unsafe.Sizeof(basic),
	); err != nil {
		return objectIdentity{}, fmt.Errorf("read basic file info: %w", err)
	}
	var standard fileStandardInfo
	if err := getFileInformationWindows(
		handle,
		fileStandardInfoClass,
		unsafe.Pointer(&standard),
		unsafe.Sizeof(standard),
	); err != nil {
		return objectIdentity{}, fmt.Errorf("read standard file info: %w", err)
	}
	var id fileIDInfo
	if err := getFileInformationWindows(
		handle,
		fileIDInfoClass,
		unsafe.Pointer(&id),
		unsafe.Sizeof(id),
	); err != nil {
		return objectIdentity{}, fmt.Errorf("read file id info: %w", err)
	}
	return objectIdentity{
		volumeSerial:  id.volumeSerial,
		fileID:        id.fileID,
		attributes:    basic.attributes,
		numberOfLinks: standard.numberOfLinks,
		size:          standard.endOfFile,
	}, nil
}

func getFileInformationWindows(
	handle windows.Handle,
	class uint32,
	information unsafe.Pointer,
	size uintptr,
) error {
	procedure := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetFileInformationByHandleEx")
	result, _, callErr := procedure.Call(
		uintptr(handle),
		uintptr(class),
		uintptr(information),
		size,
	)
	if result == 0 {
		if errors.Is(callErr, windows.ERROR_SUCCESS) {
			callErr = errors.New("get file information returned false")
		}
		return callErr
	}
	return nil
}

func caseSensitiveWindows(handle windows.Handle) (bool, error) {
	var information fileCaseSensitiveInfo
	if err := getFileInformationWindows(
		handle,
		fileCaseSensitiveClass,
		unsafe.Pointer(&information),
		unsafe.Sizeof(information),
	); err != nil {
		return false, fmt.Errorf("read case-sensitive info: %w", err)
	}
	return information.flags&fileCaseSensitiveDir != 0, nil
}

func driveTypeWindows(root string) (uint32, error) {
	rootUTF16, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return 0, fmt.Errorf("encode volume root: %w", err)
	}
	procedure := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetDriveTypeW")
	result, _, _ := procedure.Call(uintptr(unsafe.Pointer(rootUTF16)))
	driveType := uint32(result)
	if driveType == windows.DRIVE_UNKNOWN || driveType == windows.DRIVE_NO_ROOT_DIR {
		return 0, errors.New("get drive type returned an unusable volume")
	}
	return driveType, nil
}

func duplicateHandleWindows(handle windows.Handle) (windows.Handle, error) {
	process := windows.CurrentProcess()
	var duplicate windows.Handle
	if err := windows.DuplicateHandle(
		process,
		handle,
		process,
		&duplicate,
		0,
		false,
		windows.DUPLICATE_SAME_ACCESS,
	); err != nil {
		return windows.InvalidHandle, fmt.Errorf("duplicate handle: %w", err)
	}
	return duplicate, nil
}

type fileIDBothDirectoryInformation struct {
	nextEntryOffset uint32
	fileIndex       uint32
	creationTime    int64
	lastAccessTime  int64
	lastWriteTime   int64
	changeTime      int64
	endOfFile       int64
	allocationSize  int64
	fileAttributes  uint32
	fileNameLength  uint32
	eaSize          uint32
	shortNameLength byte
	_               byte
	shortName       [12]uint16
	fileID          int64
	fileName        [1]uint16
}

func listDirectoryWindows(handle windows.Handle) ([]directoryEntry, error) {
	buffer := make([]byte, 64*1024)
	class := fileIDBothDirectoryStart
	entries := make([]directoryEntry, 0, 16)
	for {
		err := getFileInformationWindows(
			handle,
			class,
			unsafe.Pointer(&buffer[0]),
			uintptr(len(buffer)),
		)
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return entries, nil
		}
		if err != nil {
			return nil, fmt.Errorf("list directory: %w", err)
		}
		class = fileIDBothDirectoryInfo

		offset := uint32(0)
		for {
			if int(offset)+int(unsafe.Sizeof(fileIDBothDirectoryInformation{})) > len(buffer) {
				return nil, errors.New("list directory returned a truncated entry")
			}
			information := (*fileIDBothDirectoryInformation)(unsafe.Pointer(&buffer[offset]))
			nameUnits := int(information.fileNameLength / 2)
			nameOffset := int(offset) + int(unsafe.Offsetof(information.fileName))
			nameEnd := nameOffset + nameUnits*2
			if information.fileNameLength%2 != 0 || nameEnd > len(buffer) {
				return nil, errors.New("list directory returned an invalid name")
			}
			namePointer := (*uint16)(unsafe.Pointer(&buffer[nameOffset]))
			name := windows.UTF16ToString(unsafe.Slice(namePointer, nameUnits))
			if name != "." && name != ".." {
				entries = append(entries, directoryEntry{
					name:       name,
					attributes: information.fileAttributes,
				})
			}
			if information.nextEntryOffset == 0 {
				break
			}
			if information.nextEntryOffset%4 != 0 {
				return nil, errors.New("list directory returned an invalid offset")
			}
			offset += information.nextEntryOffset
			if int(offset) >= len(buffer) {
				return nil, errors.New("list directory offset exceeds buffer")
			}
		}
	}
}

func readFileWindows(handle windows.Handle, buffer []byte) (int, error) {
	var read uint32
	if err := windows.ReadFile(handle, buffer, &read, nil); err != nil {
		return int(read), fmt.Errorf("read file: %w", err)
	}
	return int(read), nil
}

func writeFileWindows(handle windows.Handle, buffer []byte) (int, error) {
	var written uint32
	if err := windows.WriteFile(handle, buffer, &written, nil); err != nil {
		return int(written), fmt.Errorf("write file: %w", err)
	}
	return int(written), nil
}

type fileDispositionInfo struct {
	deleteFile byte
}

type fileDispositionInfoEx struct {
	flags uint32
}

func setDispositionWindows(handle windows.Handle) error {
	extended := fileDispositionInfoEx{flags: deleteDispositionFlags()}
	err := setFileInformationWindows(
		handle,
		fileDispositionExClass,
		unsafe.Pointer(&extended),
		uintptr(unsafe.Sizeof(extended)),
	)
	if err == nil {
		return nil
	}
	if !errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return fmt.Errorf("set disposition ex: %w", err)
	}
	legacy := fileDispositionInfo{deleteFile: 1}
	if err := setFileInformationWindows(
		handle,
		fileDispositionInfoClass,
		unsafe.Pointer(&legacy),
		uintptr(unsafe.Sizeof(legacy)),
	); err != nil {
		return fmt.Errorf("set disposition: %w", err)
	}
	return nil
}

func deleteDispositionFlags() uint32 {
	return fileDispositionDelete | fileDispositionIgnoreRead
}

func setStateDispositionWindows(
	handle windows.Handle,
	spec stateDispositionSpec,
) error {
	information := fileDispositionInfoEx{flags: spec.flags}
	if err := setFileInformationWindows(
		handle,
		spec.informationClass,
		unsafe.Pointer(&information),
		uintptr(unsafe.Sizeof(information)),
	); err != nil {
		return fmt.Errorf("set state disposition ex: %w", err)
	}
	return nil
}

type fileRenameInformation struct {
	replaceIfExists uint8
	rootDirectory   windows.Handle
	fileNameLength  uint32
	fileName        [1]uint16
}

func renameWindows(
	source windows.Handle,
	targetParent windows.Handle,
	name string,
	replace bool,
) error {
	if err := renameInformationWindows(source, targetParent, name, replace); err != nil {
		return fmt.Errorf("rename by handle: %w", err)
	}
	return nil
}

func renameStateWindows(
	source windows.Handle,
	targetParent windows.Handle,
	name string,
	flags uint32,
) error {
	if flags != 0 {
		return fmt.Errorf("state rename flags: %w", ErrInvalidArgument)
	}
	if err := renameInformationWindows(source, targetParent, name, false); err != nil {
		return fmt.Errorf("rename state by handle: %w", err)
	}
	return nil
}

func renameInformationWindows(
	source windows.Handle,
	targetParent windows.Handle,
	name string,
	replace bool,
) error {
	nameUTF16, err := windows.UTF16FromString(name)
	if err != nil {
		return fmt.Errorf("encode rename destination: %w", err)
	}
	nameUTF16 = nameUTF16[:len(nameUTF16)-1]
	nameBytes := len(nameUTF16) * 2
	offset := int(unsafe.Offsetof(fileRenameInformation{}.fileName))
	buffer := make([]byte, offset+nameBytes)
	information := (*fileRenameInformation)(unsafe.Pointer(&buffer[0]))
	if replace {
		information.replaceIfExists = 1
	}
	information.rootDirectory = targetParent
	information.fileNameLength = uint32(nameBytes)
	copy(unsafe.Slice(&information.fileName[0], len(nameUTF16)), nameUTF16)
	var status windows.IO_STATUS_BLOCK
	if err := windows.NtSetInformationFile(
		source,
		&status,
		&buffer[0],
		uint32(len(buffer)),
		windows.FileRenameInformation,
	); err != nil {
		if ntStatus, ok := err.(windows.NTStatus); ok {
			return ntStatus.Errno()
		}
		return err
	}
	return nil
}

func setFileInformationWindows(
	handle windows.Handle,
	class uint32,
	information unsafe.Pointer,
	size uintptr,
) error {
	procedure := windows.NewLazySystemDLL("kernel32.dll").NewProc("SetFileInformationByHandle")
	result, _, callErr := procedure.Call(
		uintptr(handle),
		uintptr(class),
		uintptr(information),
		size,
	)
	if result == 0 {
		if errors.Is(callErr, windows.ERROR_SUCCESS) {
			callErr = errors.New("set file information returned false")
		}
		return callErr
	}
	return nil
}

func openPinnedChain(
	ctx context.Context,
	rootParent CanonicalPath,
	target CanonicalPath,
	leafSpec openSpec,
) (*pinnedChain, error) {
	return openPinnedChainWith(ctx, rootParent, target, leafSpec, newProductionPathAPI())
}

func openPinnedChainWith(
	ctx context.Context,
	rootParent CanonicalPath,
	target CanonicalPath,
	leafSpec openSpec,
	api pathAPI,
) (*pinnedChain, error) {
	if ctx == nil || !api.valid() ||
		rootParent == (CanonicalPath{}) || target == (CanonicalPath{}) {
		return nil, fmt.Errorf("%w: invalid pinned-chain input", ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !rootParent.Contains(target) {
		return nil, outsidePathError("pin", target.String(), ErrIdentityChanged)
	}
	if err := ensureLocalVolumeWith(rootParent, api); err != nil {
		return nil, err
	}

	chain := &pinnedChain{
		api:     api,
		objects: make([]pinnedObject, 0, len(canonicalComponents(target.comparisonKey))+1),
	}
	fail := func(operationErr error) (*pinnedChain, error) {
		return nil, errors.Join(operationErr, chain.close())
	}

	rootSpec := directoryPinSpec()
	rootHandle, err := api.openPath(rootParent.Native(), rootSpec)
	if err != nil {
		return fail(&FileError{
			Operation: "open-root-parent",
			Path:      rootParent.String(),
			Err:       err,
		})
	}
	rootIdentity, err := api.identity(rootHandle)
	if err != nil {
		closeErr := api.closeHandle(rootHandle)
		return fail(joinOperationCleanup(
			&FileError{
				Operation: "identify-root-parent",
				Path:      rootParent.String(),
				Err:       err,
			},
			wrapFileError("close", rootParent.String(), closeErr),
		))
	}
	root := pinnedObject{path: rootParent, handle: rootHandle, identity: rootIdentity}
	chain.objects = append(chain.objects, root)
	if err := ensureCaseInsensitiveChain(ctx, chain); err != nil {
		return fail(err)
	}

	parentComponents := canonicalComponents(rootParent.comparisonKey)
	targetComponents := canonicalComponents(target.comparisonKey)
	displayComponents := canonicalComponents(target.display)
	if len(targetComponents) != len(displayComponents) ||
		len(targetComponents) <= len(parentComponents) {
		return fail(outsidePathError("pin", target.String(), ErrIdentityChanged))
	}
	for i := len(parentComponents); i < len(targetComponents); i++ {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		spec := directoryPinSpec()
		if i == len(targetComponents)-1 {
			spec = leafSpec
		}
		child, err := openRelativeCheckedWith(
			ctx,
			chain.objects[len(chain.objects)-1],
			displayComponents[i],
			spec,
			api,
		)
		if err != nil {
			return fail(err)
		}
		chain.objects = append(chain.objects, child)
	}
	if !chain.objects[len(chain.objects)-1].path.Equal(target) {
		return fail(outsidePathError("pin", target.String(), ErrIdentityChanged))
	}
	return chain, nil
}

func openRelativeCheckedWith(
	ctx context.Context,
	parent pinnedObject,
	name string,
	spec openSpec,
	api pathAPI,
) (pinnedObject, error) {
	if ctx == nil || name == "" || !api.valid() {
		return pinnedObject{}, fmt.Errorf("%w: invalid relative-open input", ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return pinnedObject{}, err
	}
	handle, err := api.openRelative(parent.handle, name, spec)
	if err != nil {
		return pinnedObject{}, &FileError{
			Operation: "open-relative",
			Path:      filepath.Join(parent.path.String(), name),
			Err:       err,
		}
	}
	closeOnFailure := func(operationErr error) (pinnedObject, error) {
		closeErr := api.closeHandle(handle)
		return pinnedObject{}, joinOperationCleanup(
			operationErr,
			wrapFileError("close", filepath.Join(parent.path.String(), name), closeErr),
		)
	}

	identity, err := api.identity(handle)
	if err != nil {
		return closeOnFailure(&FileError{
			Operation: "identify",
			Path:      filepath.Join(parent.path.String(), name),
			Err:       err,
		})
	}
	if identity.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return closeOnFailure(unsafeReparseError(filepath.Join(parent.path.String(), name)))
	}
	isDirectory := identity.attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if isDirectory != spec.directory {
		return closeOnFailure(&FileError{
			Operation: "type",
			Path:      filepath.Join(parent.path.String(), name),
			Err:       ErrIdentityChanged,
		})
	}
	finalPath, err := api.finalPath(handle)
	if err != nil {
		return closeOnFailure(&FileError{
			Operation: "final-path",
			Path:      filepath.Join(parent.path.String(), name),
			Err:       err,
		})
	}
	canonical, err := canonicalizeContextWith(ctx, finalPath, api)
	if err != nil {
		return closeOnFailure(err)
	}
	child := pinnedObject{path: canonical, handle: handle, identity: identity}
	if err := validateParentIdentityWith(ctx, parent, child, api); err != nil {
		return closeOnFailure(err)
	}
	if isDirectory {
		if err := validatePinnedDirectory(ctx, child, api); err != nil {
			return closeOnFailure(err)
		}
	}
	return child, nil
}

func validateParentIdentityWith(
	ctx context.Context,
	parent pinnedObject,
	child pinnedObject,
	api pathAPI,
) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is nil", ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	finalPath, err := api.finalPath(child.handle)
	if err != nil {
		return &FileError{Operation: "final-path", Path: child.path.String(), Err: err}
	}
	display, err := displayWindowsPath(finalPath)
	if err != nil {
		return err
	}
	parentPath := filepath.Dir(display)
	handle, err := api.openPath(nativeWindowsPath(parentPath), parentIdentitySpec())
	if err != nil {
		return &FileError{Operation: "open-parent-identity", Path: parentPath, Err: err}
	}
	identity, identityErr := api.identity(handle)
	closeErr := api.closeHandle(handle)
	if identityErr != nil || closeErr != nil {
		return errors.Join(
			wrapFileError("identify-parent", parentPath, identityErr),
			wrapFileError("close", parentPath, closeErr),
		)
	}
	if identity.volumeSerial != parent.identity.volumeSerial ||
		identity.fileID != parent.identity.fileID {
		return &FileError{
			Operation: "validate-parent",
			Path:      child.path.String(),
			Err:       ErrIdentityChanged,
		}
	}
	return nil
}

func validatePinnedDirectory(
	ctx context.Context,
	object pinnedObject,
	api pathAPI,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if object.identity.attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return &FileError{Operation: "type", Path: object.path.String(), Err: ErrIdentityChanged}
	}
	if object.identity.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return unsafeReparseError(object.path.String())
	}
	caseSensitive, err := api.caseSensitive(object.handle)
	if err != nil {
		return &FileError{
			Operation: "case-sensitivity",
			Path:      object.path.String(),
			Err:       errors.Join(ErrUnsupportedCaseSensitivity, err),
		}
	}
	if caseSensitive {
		return &FileError{
			Operation: "case-sensitivity",
			Path:      object.path.String(),
			Err:       ErrUnsupportedCaseSensitivity,
		}
	}
	return nil
}

func ensureCaseInsensitiveChain(ctx context.Context, chain *pinnedChain) error {
	if ctx == nil || chain == nil || !chain.api.valid() {
		return fmt.Errorf("%w: invalid case-insensitive chain", ErrInvalidArgument)
	}
	for _, object := range chain.objects {
		if err := validatePinnedDirectory(ctx, object, chain.api); err != nil {
			return err
		}
	}
	return nil
}

func ensureLocalVolumeWith(path CanonicalPath, api pathAPI) error {
	if strings.HasPrefix(path.volumeKey, `\\`) {
		return outsidePathError("volume", path.String(), ErrIdentityChanged)
	}
	driveType, err := api.driveType(path.volumeKey + `\`)
	if err != nil {
		return &FileError{Operation: "drive-type", Path: path.String(), Err: err}
	}
	if driveType != windows.DRIVE_FIXED && driveType != windows.DRIVE_REMOVABLE {
		return outsidePathError("volume", path.String(), ErrIdentityChanged)
	}
	return nil
}

func directoryPinSpec() openSpec {
	return openSpec{
		access:    windows.FILE_LIST_DIRECTORY | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE,
		share:     windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE,
		creation:  windows.OPEN_EXISTING,
		options:   windows.FILE_FLAG_BACKUP_SEMANTICS | windows.FILE_FLAG_OPEN_REPARSE_POINT,
		directory: true,
	}
}

func parentIdentitySpec() openSpec {
	return openSpec{
		access:    windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE,
		share:     windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE,
		creation:  windows.OPEN_EXISTING,
		options:   windows.FILE_FLAG_BACKUP_SEMANTICS | windows.FILE_FLAG_OPEN_REPARSE_POINT,
		directory: true,
	}
}

func deleteByHandleWith(handle windows.Handle, api pathAPI) error {
	if err := api.setDisposition(handle); err != nil {
		return fmt.Errorf("delete by handle: %w", err)
	}
	return nil
}

func renameByHandleWith(
	source windows.Handle,
	targetParent windows.Handle,
	name string,
	replace bool,
	api pathAPI,
) error {
	if err := api.rename(source, targetParent, name, replace); err != nil {
		return fmt.Errorf("rename by handle: %w", err)
	}
	return nil
}

func (c *pinnedChain) close() error {
	if c == nil {
		return nil
	}
	closeErrors := make([]error, 0, len(c.objects))
	for i := len(c.objects) - 1; i >= 0; i-- {
		handle := &c.objects[i].handle
		if *handle == 0 || *handle == windows.InvalidHandle {
			continue
		}
		if err := c.api.closeHandle(*handle); err != nil {
			closeErrors = append(closeErrors, &FileError{
				Operation: "close",
				Path:      c.objects[i].path.String(),
				Err:       err,
			})
			continue
		}
		*handle = windows.InvalidHandle
	}
	return errors.Join(closeErrors...)
}

func outsidePathError(operation, path string, cause error) error {
	return &Error{
		code:      protocol.CodePathOutsideManagedRoot,
		Operation: operation,
		Path:      path,
		Err:       cause,
	}
}

func unsafeReparseError(path string) error {
	return &Error{
		code:      protocol.CodeUnsafeReparsePoint,
		Operation: "reparse",
		Path:      path,
		Err:       errors.New("filesystem path contains a reparse point"),
	}
}

func validateDangerousRoot(
	ctx context.Context,
	layout *config.Layout,
	targets []string,
) ([]*pinnedChain, error) {
	return validateDangerousRootWith(ctx, layout, targets, newProductionPathAPI())
}

func validateDangerousRootWith(
	ctx context.Context,
	layout *config.Layout,
	targets []string,
	api pathAPI,
) ([]*pinnedChain, error) {
	if ctx == nil || layout == nil || len(targets) == 0 || !api.valid() {
		return nil, fmt.Errorf("%w: invalid root-validation input", ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	appRoot, err := canonicalizeContextWith(ctx, layout.AppRoot(), api)
	if err != nil {
		return nil, err
	}
	rootParent, err := canonicalizeContextWith(ctx, filepath.Dir(layout.AppRoot()), api)
	if err != nil {
		return nil, err
	}
	if !rootParent.Contains(appRoot) {
		return nil, outsidePathError("validate-root", appRoot.String(), ErrIdentityChanged)
	}

	chains := make([]*pinnedChain, 0, len(targets))
	closeAll := func(operationErr error) ([]*pinnedChain, error) {
		closeErrors := make([]error, 0, len(chains))
		for i := len(chains) - 1; i >= 0; i-- {
			closeErrors = append(closeErrors, chains[i].close())
		}
		return nil, errors.Join(operationErr, errors.Join(closeErrors...))
	}
	for _, rawTarget := range targets {
		if err := ctx.Err(); err != nil {
			return closeAll(err)
		}
		target, err := canonicalizeContextWith(ctx, rawTarget, api)
		if err != nil {
			return closeAll(err)
		}
		if !appRoot.Equal(target) && !appRoot.Contains(target) {
			return closeAll(outsidePathError("validate-root", target.String(), ErrIdentityChanged))
		}
		chain, err := openPinnedChainWith(
			ctx,
			rootParent,
			target,
			directoryPinSpec(),
			api,
		)
		if err != nil {
			return closeAll(err)
		}
		chains = append(chains, chain)
	}
	return chains, nil
}
