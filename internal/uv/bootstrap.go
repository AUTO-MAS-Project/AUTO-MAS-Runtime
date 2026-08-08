package uv

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/logging"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/process"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

const (
	maxUVArchiveBytes  = 128 << 20
	maxUVArchiveFiles  = 32
	maxUVDownloadBytes = 256 << 20
)

// DownloadClient 是 bootstrap 所需的受控制品下载能力。
type DownloadClient interface {
	Download(ctx context.Context, request mirror.DownloadRequest) (mirror.DownloadResult, error)
}

// ArchiveExtractor 把已校验的 uv zip 解压到指定暂存目录。
type ArchiveExtractor interface {
	Extract(ctx context.Context, archivePath, stagingDir string) error
}

// Publisher 把经过验证的 uv 暂存目录原子发布到版本目录。
type Publisher interface {
	Publish(ctx context.Context, request PublishRequest) (PublishResult, error)
}

// VersionChecker 负责执行固定 uv 的版本实测。
type VersionChecker interface {
	Check(ctx context.Context, executable, expected string, line LineFunc) error
}

// PublishRequest 描述一次版本化 uv 目录发布。
type PublishRequest struct {
	Source      string
	Destination string
	OperationID string
	Version     string
}

// PublishResult 报告目录 rename 是否已经产生副作用。
type PublishResult struct {
	MutationApplied bool
}

// BootstrapOptions 配置 Bootstrapper 的可替换依赖。
type BootstrapOption func(*bootstrapOptions) error

type bootstrapOptions struct {
	downloader DownloadClient
	extractor  ArchiveExtractor
	publisher  Publisher
	remover    TreeRemover
	checker    VersionChecker
	catalog    *mirror.Catalog
	rotator    rotationRunner
	artifact   Artifact
}

type rotationRunner interface {
	Run(
		ctx context.Context,
		plan mirror.Plan,
		target mirror.Target,
		attempt mirror.AttemptFunc,
	) (mirror.RotationResult, error)
}

// WithDownloader 注入制品下载器。
func WithDownloader(downloader DownloadClient) BootstrapOption {
	return func(options *bootstrapOptions) error {
		if downloader == nil {
			return errors.New("uv bootstrap downloader is invalid")
		}
		options.downloader = downloader
		return nil
	}
}

// WithArchiveExtractor 注入 zip 解压器。
func WithArchiveExtractor(extractor ArchiveExtractor) BootstrapOption {
	return func(options *bootstrapOptions) error {
		if extractor == nil {
			return errors.New("uv bootstrap extractor is invalid")
		}
		options.extractor = extractor
		return nil
	}
}

// WithPublisher 注入版本目录发布器。
func WithPublisher(publisher Publisher) BootstrapOption {
	return func(options *bootstrapOptions) error {
		if publisher == nil {
			return errors.New("uv bootstrap publisher is invalid")
		}
		options.publisher = publisher
		return nil
	}
}

// WithBootstrapRemover 注入版本目录的受控删除能力。
func WithBootstrapRemover(remover TreeRemover) BootstrapOption {
	return func(options *bootstrapOptions) error {
		if remover == nil {
			return errors.New("uv bootstrap remover is invalid")
		}
		options.remover = remover
		return nil
	}
}

// WithVersionChecker 注入版本实测替身。
func WithVersionChecker(checker VersionChecker) BootstrapOption {
	return func(options *bootstrapOptions) error {
		if checker == nil {
			return errors.New("uv bootstrap version checker is invalid")
		}
		options.checker = checker
		return nil
	}
}

// WithArtifact 注入测试制品；生产构造默认使用 D-open-6 固定值。
func WithArtifact(artifact Artifact) BootstrapOption {
	return func(options *bootstrapOptions) error {
		if artifact.Version == "" || artifact.Name == "" || artifact.SHA256 == "" {
			return errors.New("uv bootstrap artifact is invalid")
		}
		options.artifact = artifact
		return nil
	}
}

// WithBootstrapCatalog 注入测试用 source catalog。
func WithBootstrapCatalog(catalog *mirror.Catalog) BootstrapOption {
	return func(options *bootstrapOptions) error {
		if catalog == nil {
			return errors.New("uv bootstrap catalog is invalid")
		}
		options.catalog = catalog
		return nil
	}
}

// WithBootstrapRotator 注入镜像轮换器。
func WithBootstrapRotator(rotator rotationRunner) BootstrapOption {
	return func(options *bootstrapOptions) error {
		if rotator == nil {
			return errors.New("uv bootstrap rotator is invalid")
		}
		options.rotator = rotator
		return nil
	}
}

// Bootstrapper 负责固定 uv 制品的缓存、校验、解压和原子发布。
type Bootstrapper struct {
	layout     *config.Layout
	downloader DownloadClient
	extractor  ArchiveExtractor
	publisher  Publisher
	remover    TreeRemover
	checker    VersionChecker
	catalog    *mirror.Catalog
	rotator    rotationRunner
	artifact   Artifact
}

// NewBootstrapper 创建生产 bootstrap 服务。
func NewBootstrapper(
	layout *config.Layout,
	optionValues ...BootstrapOption,
) (*Bootstrapper, error) {
	if layout == nil {
		return nil, errors.New("uv bootstrap layout is invalid")
	}
	catalog, err := mirror.DefaultCatalog()
	if err != nil {
		return nil, fmt.Errorf("build uv source catalog: %w", err)
	}
	downloader, err := mirror.NewDownloader(layout)
	if err != nil {
		return nil, fmt.Errorf("create uv downloader: %w", err)
	}
	rotator, err := mirror.NewRotator()
	if err != nil {
		return nil, fmt.Errorf("create uv rotator: %w", err)
	}
	values := bootstrapOptions{
		downloader: downloader,
		extractor:  zipExtractor{},
		publisher:  filesystemPublisher{layout: layout},
		remover:    filesystemVersionRemover{layout: layout},
		checker:    runnerVersionChecker{layout: layout},
		catalog:    catalog,
		rotator:    rotator,
		artifact:   WindowsX64ArtifactSpec(),
	}
	for _, option := range optionValues {
		if option == nil {
			return nil, errors.New("uv bootstrap option is nil")
		}
		if err := option(&values); err != nil {
			return nil, err
		}
	}
	if values.downloader == nil || values.extractor == nil || values.publisher == nil ||
		values.remover == nil ||
		values.checker == nil || values.catalog == nil || values.rotator == nil ||
		values.artifact.Version == "" {
		return nil, errors.New("uv bootstrap dependencies are incomplete")
	}
	return &Bootstrapper{
		layout:     layout,
		downloader: values.downloader,
		extractor:  values.extractor,
		publisher:  values.publisher,
		remover:    values.remover,
		checker:    values.checker,
		catalog:    values.catalog,
		rotator:    values.rotator,
		artifact:   values.artifact,
	}, nil
}

// Ensure 返回已通过版本实测的 uv.exe；缓存命中不访问网络。
func (b *Bootstrapper) Ensure(
	ctx context.Context,
	operationID string,
	policy mirror.Policy,
) (executablePath string, returnErr error) {
	return b.ensure(ctx, operationID, policy, nil)
}

// EnsureWithLine 返回已通过版本实测的 uv.exe，并转发版本检查输出。
func (b *Bootstrapper) EnsureWithLine(
	ctx context.Context,
	operationID string,
	policy mirror.Policy,
	line LineFunc,
) (executablePath string, returnErr error) {
	return b.ensure(ctx, operationID, policy, line)
}

func (b *Bootstrapper) ensure(
	ctx context.Context,
	operationID string,
	policy mirror.Policy,
	line LineFunc,
) (executablePath string, returnErr error) {
	if ctx == nil || b == nil || b.layout == nil {
		return "", newError(
			protocol.CodeUVDownloadFailed,
			protocol.StageUVCheck,
			"uv 准备失败",
			map[string]any{},
			errors.New("uv bootstrap request is invalid"),
		)
	}
	if err := requireUVPlatform(protocol.StageUVCheck); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	spec := b.artifact
	executable, err := b.layout.UVExecutable(spec.Version)
	if err != nil {
		return "", wrapBootstrapError(protocol.StageUVCheck, err)
	}
	versionDir, err := b.layout.UVVersionDir(spec.Version)
	if err != nil {
		return "", wrapBootstrapError(protocol.StageUVVerify, err)
	}
	if regularFile(executable) {
		if err := b.checkExecutableVersion(ctx, executable, line); err == nil {
			return executable, nil
		} else if !isVersionMismatch(err) {
			return "", err
		}
	}
	if err := b.removeInvalidVersionDirectory(ctx, operationID, versionDir); err != nil {
		return "", err
	}
	archivePath, err := b.layout.DownloadFile(spec.Name)
	if err != nil {
		return "", wrapBootstrapError(protocol.StageUVDownload, err)
	}
	if !regularFile(archivePath) {
		downloadResult, downloadErr := b.download(ctx, policy, spec)
		if downloadErr != nil {
			return "", downloadErr
		}
		archivePath = downloadResult.Path
	}
	archiveLease, err := filesystem.OpenManagedDirectory(ctx, b.layout, filepath.Dir(archivePath))
	if err != nil {
		return "", wrapBootstrapError(protocol.StageUVVerify, err)
	}
	defer func() {
		if closeErr := archiveLease.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, wrapBootstrapError(protocol.StageUVVerify, closeErr))
		}
	}()
	if err := verifySHA256(ctx, archivePath, spec.SHA256); err != nil {
		if errors.Is(err, errChecksumMismatch) {
			if removeErr := invalidateDownloadCache(ctx, b.layout, archivePath); removeErr != nil {
				err = errors.Join(err, removeErr)
			}
			return "", newError(
				protocol.CodeUVChecksumMismatch,
				protocol.StageUVVerify,
				"uv 校验和不匹配",
				map[string]any{"expectedSHA256": spec.SHA256},
				err,
			)
		}
		return "", wrapBootstrapError(protocol.StageUVVerify, err)
	}
	staging, err := b.layout.UVStagingDir(spec.Version, operationID)
	if err != nil {
		return "", wrapBootstrapError(protocol.StageUVDownload, err)
	}
	if err := ensureManagedDirectory(ctx, b.layout, filepath.Dir(versionDir)); err != nil {
		return "", wrapBootstrapError(protocol.StageUVDownload, err)
	}
	if err := ensureStagingParent(ctx, b.layout, staging); err != nil {
		return "", wrapBootstrapError(protocol.StageUVDownload, err)
	}
	stagingLease, err := prepareStagingDirectory(ctx, b.layout, staging)
	if err != nil {
		return "", wrapBootstrapError(protocol.StageUVDownload, err)
	}
	published := false
	defer func() {
		if closeErr := stagingLease.Close(); closeErr != nil {
			closeError := newError(
				protocol.CodeUVDownloadFailed,
				protocol.StageUVDownload,
				"uv 暂存目录收口失败",
				map[string]any{"committed": published},
				closeErr,
			)
			if published {
				closeError = newCommittedError(
					protocol.CodeUVDownloadFailed,
					protocol.StageUVDownload,
					"uv 暂存目录收口失败",
					map[string]any{"committed": true},
					closeErr,
				)
			}
			returnErr = errors.Join(returnErr, closeError)
		}
	}()
	if err := b.extractor.Extract(ctx, archivePath, staging); err != nil {
		return "", wrapBootstrapError(protocol.StageUVDownload, err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	result, err := b.publisher.Publish(ctx, PublishRequest{
		Source:      staging,
		Destination: versionDir,
		OperationID: operationID,
		Version:     spec.Version,
	})
	published = result.MutationApplied
	if err != nil {
		if result.MutationApplied {
			return "", newCommittedError(
				protocol.CodeUVDownloadFailed,
				protocol.StageUVDownload,
				"uv 发布失败",
				map[string]any{"committed": true},
				err,
			)
		}
		return "", wrapBootstrapError(protocol.StageUVDownload, err)
	}
	if err := b.checkExecutableVersion(ctx, executable, line); err != nil {
		return "", committedBootstrapError(err)
	}
	return executable, nil
}

// Check 只检查版本化 uv 是否存在且版本一致，不访问网络。
func (b *Bootstrapper) Check(ctx context.Context) (bool, error) {
	return b.check(ctx, nil)
}

// CheckWithLine 只检查版本化 uv 是否存在且版本一致，并转发版本检查输出。
func (b *Bootstrapper) CheckWithLine(ctx context.Context, line LineFunc) (bool, error) {
	return b.check(ctx, line)
}

func (b *Bootstrapper) check(ctx context.Context, line LineFunc) (bool, error) {
	if ctx == nil || b == nil || b.layout == nil {
		return false, errors.New("uv bootstrap check request is invalid")
	}
	if err := requireUVPlatform(protocol.StageUVCheck); err != nil {
		return false, err
	}
	path, err := b.layout.UVExecutable(b.artifact.Version)
	if err != nil {
		return false, err
	}
	if !regularFile(path) {
		return false, nil
	}
	if err := b.checkExecutableVersion(ctx, path, line); err != nil {
		if isVersionMismatch(err) {
			return false, nil
		}
		return false, wrapBootstrapError(protocol.StageUVVerify, err)
	}
	return true, nil
}

// Repair 删除当前固定版本的 uv 受管事实后重新执行完整 bootstrap。
func (b *Bootstrapper) Repair(
	ctx context.Context,
	operationID string,
	policy mirror.Policy,
) (string, error) {
	return b.repair(ctx, operationID, policy, nil)
}

// RepairWithLine 删除当前固定版本的 uv 受管事实后重新执行完整 bootstrap，并转发输出。
func (b *Bootstrapper) RepairWithLine(
	ctx context.Context,
	operationID string,
	policy mirror.Policy,
	line LineFunc,
) (string, error) {
	return b.repair(ctx, operationID, policy, line)
}

func (b *Bootstrapper) repair(
	ctx context.Context,
	operationID string,
	policy mirror.Policy,
	line LineFunc,
) (string, error) {
	if ctx == nil || b == nil || b.layout == nil || operationID == "" {
		return "", errors.New("uv bootstrap repair request is invalid")
	}
	if err := requireUVPlatform(protocol.StageUVCheck); err != nil {
		return "", err
	}
	versionDir, err := b.layout.UVVersionDir(b.artifact.Version)
	if err != nil {
		return "", wrapBootstrapError(protocol.StageUVVerify, err)
	}
	if err := b.removeInvalidVersionDirectory(ctx, operationID, versionDir); err != nil {
		return "", err
	}
	archivePath, err := b.layout.DownloadFile(b.artifact.Name)
	if err != nil {
		return "", wrapBootstrapError(protocol.StageUVDownload, err)
	}
	if regularFile(archivePath) {
		lease, leaseErr := filesystem.OpenManagedDirectory(ctx, b.layout, filepath.Dir(archivePath))
		if leaseErr != nil {
			return "", wrapBootstrapError(protocol.StageUVVerify, leaseErr)
		}
		removeErr := invalidateDownloadCache(ctx, b.layout, archivePath)
		closeErr := lease.Close()
		if removeErr != nil || closeErr != nil {
			return "", wrapBootstrapError(protocol.StageUVVerify, errors.Join(removeErr, closeErr))
		}
	}
	return b.ensure(ctx, operationID, policy, line)
}

func requireUVPlatform(stage protocol.Stage) error {
	if process.Supported() {
		return nil
	}
	return newError(
		protocol.CodeUnsupportedMode,
		stage,
		"当前平台尚不支持 uv 进程树监督",
		map[string]any{},
		process.ErrUnsupported,
	)
}

func (b *Bootstrapper) download(
	ctx context.Context,
	policy mirror.Policy,
	spec Artifact,
) (mirror.DownloadResult, error) {
	target, err := mirror.NewTarget(mirror.TargetSpec{UVVersion: spec.Version})
	if err != nil {
		return mirror.DownloadResult{}, wrapBootstrapError(protocol.StageUVDownload, err)
	}
	plan, err := mirror.BuildPlan(b.catalog, policy, mirror.KindUV)
	if err != nil {
		return mirror.DownloadResult{}, wrapBootstrapError(protocol.StageUVDownload, err)
	}
	var downloadedPath string
	rotationResult, rotationErr := b.rotator.Run(ctx, plan, target, func(
		attemptContext context.Context,
		attempt mirror.Attempt,
	) mirror.AttemptOutcome {
		url := strings.TrimRight(attempt.Source.BaseURL(), "/") + "/" + spec.Version + "/" + spec.Name
		downloadResult, downloadErr := b.downloader.Download(attemptContext, mirror.DownloadRequest{
			URL:              url,
			FileName:         spec.Name,
			ExpectedSize:     0,
			AllowUnknownSize: true,
			MaxSize:          maxUVDownloadBytes,
			ExpectedSHA256:   spec.SHA256,
		})
		if downloadErr == nil {
			downloadedPath = downloadResult.Path
			return mirror.AttemptOutcome{Kind: mirror.OutcomeSucceeded}
		}
		if isDownloadIntegrityFailure(downloadErr) {
			return mirror.AttemptOutcome{
				Kind:        mirror.OutcomeIntegrityFailure,
				FailureKind: mirror.FailureKind("checksum_mismatch"),
				Err:         downloadErr,
			}
		}
		return mirror.AttemptOutcome{
			Kind:        mirror.OutcomeSwitchSource,
			FailureKind: mirror.FailureKind("download_failed"),
			Err:         downloadErr,
		}
	})
	if rotationErr != nil {
		if _, ok := rotationErr.(*mirror.IntegrityExhaustedError); ok {
			return mirror.DownloadResult{}, newError(
				protocol.CodeUVChecksumMismatch,
				protocol.StageUVVerify,
				"uv 校验和不匹配",
				map[string]any{"expectedSHA256": spec.SHA256},
				rotationErr,
			)
		}
		return mirror.DownloadResult{}, newError(
			protocol.CodeUVDownloadFailed,
			protocol.StageUVDownload,
			"uv 下载失败",
			map[string]any{},
			rotationErr,
		)
	}
	if len(rotationResult.Reports) == 0 {
		return mirror.DownloadResult{}, newError(
			protocol.CodeUVDownloadFailed,
			protocol.StageUVDownload,
			"uv 下载失败",
			map[string]any{},
			errors.New("uv download returned no attempt report"),
		)
	}
	if downloadedPath != "" {
		expectedPath, pathErr := b.layout.DownloadFile(spec.Name)
		if pathErr != nil {
			return mirror.DownloadResult{}, wrapBootstrapError(protocol.StageUVDownload, pathErr)
		}
		if !samePath(downloadedPath, expectedPath) {
			return mirror.DownloadResult{}, wrapBootstrapError(
				protocol.StageUVDownload,
				errors.New("uv downloader returned an unexpected cache path"),
			)
		}
		return mirror.DownloadResult{Path: downloadedPath}, nil
	}
	path, err := b.layout.DownloadFile(spec.Name)
	if err != nil {
		return mirror.DownloadResult{}, wrapBootstrapError(protocol.StageUVDownload, err)
	}
	return mirror.DownloadResult{Path: path}, nil
}

func (b *Bootstrapper) checkVersion(ctx context.Context, executable string, line LineFunc) error {
	return b.checker.Check(ctx, executable, b.artifact.Version, line)
}

func (b *Bootstrapper) checkExecutableVersion(ctx context.Context, executable string, line LineFunc) error {
	lease, err := filesystem.OpenManagedDirectory(ctx, b.layout, filepath.Dir(executable))
	if err != nil {
		return wrapBootstrapError(protocol.StageUVVerify, err)
	}
	checkErr := b.checkVersion(ctx, executable, line)
	closeErr := lease.Close()
	if closeErr != nil {
		closeErr = wrapBootstrapError(protocol.StageUVVerify, closeErr)
	}
	if checkErr != nil {
		checkErr = wrapBootstrapError(protocol.StageUVVerify, checkErr)
	}
	if checkErr != nil || closeErr != nil {
		return errors.Join(checkErr, closeErr)
	}
	return nil
}

func (b *Bootstrapper) removeInvalidVersionDirectory(
	ctx context.Context,
	operationID string,
	versionDir string,
) error {
	_, err := os.Lstat(versionDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return wrapBootstrapError(protocol.StageUVVerify, err)
	}
	result, err := b.remover.RemoveTree(ctx, filesystem.DeleteRequest{
		Kind:        filesystem.DeleteUVVersion,
		Target:      versionDir,
		OperationID: operationID,
		Version:     b.artifact.Version,
		Reason:      "remove incomplete uv version",
	})
	if err != nil {
		return wrapBootstrapError(protocol.StageUVVerify, err)
	}
	if result.Partial {
		if err == nil {
			err = errors.New("uv version removal was partial")
		}
		return newError(
			protocol.CodeUVVersionMismatch,
			protocol.StageUVVerify,
			"uv 版本目录无法修复",
			map[string]any{"removed": result.Removed, "partial": result.Partial},
			err,
		)
	}
	return nil
}

func isVersionMismatch(err error) bool {
	var operationErr interface{ Code() protocol.Code }
	return errors.As(err, &operationErr) && operationErr.Code() == protocol.CodeUVVersionMismatch
}

func committedBootstrapError(err error) error {
	if err == nil {
		return nil
	}
	if committed, ok := err.(interface{ Committed() bool }); ok && committed.Committed() {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return newCommittedError(
			protocol.CodeOperationCancelled,
			protocol.StageUVVerify,
			"操作已取消",
			map[string]any{"committed": true},
			err,
		)
	}
	var coded interface {
		Code() protocol.Code
		Stage() protocol.Stage
		Message() string
		Details() map[string]any
	}
	if errors.As(err, &coded) && protocol.IsKnownCode(coded.Code()) {
		details := coded.Details()
		if details == nil {
			details = map[string]any{}
		}
		details["committed"] = true
		stage := coded.Stage()
		if !protocol.IsKnownStage(stage) {
			stage = protocol.StageUVVerify
		}
		return newCommittedError(coded.Code(), stage, coded.Message(), details, err)
	}
	return newCommittedError(
		protocol.CodeUVVersionMismatch,
		protocol.StageUVVerify,
		"uv 发布后版本校验失败",
		map[string]any{"committed": true},
		err,
	)
}

func isDownloadIntegrityFailure(err error) bool {
	var failure *mirror.DownloadFailure
	return errors.As(err, &failure) && failure.Kind == mirror.FailureChecksumMismatch
}

var errChecksumMismatch = errors.New("uv checksum mismatch")

func verifySHA256(ctx context.Context, path, expected string) error {
	if ctx == nil {
		return errors.New("uv checksum context is invalid")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !regularFile(path) {
		return errors.New("uv archive is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	hash := sha256.New()
	copyErr := error(nil)
	if _, copyErr = io.Copy(hash, contextReader{ctx: ctx, reader: file}); copyErr != nil {
		closeErr := file.Close()
		return errors.Join(copyErr, closeErr)
	}
	if closeErr := file.Close(); closeErr != nil {
		return closeErr
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("%w: got %s", errChecksumMismatch, actual)
	}
	return nil
}

func samePath(left, right string) bool {
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}

func regularFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

func ensureStagingParent(ctx context.Context, layout *config.Layout, staging string) error {
	if ctx == nil || layout == nil || staging == "" {
		return errors.New("uv staging layout is invalid")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return ensureManagedDirectory(ctx, layout, filepath.Dir(staging))
}

func ensureManagedDirectory(
	ctx context.Context,
	layout *config.Layout,
	path string,
) error {
	if ctx == nil || layout == nil || path == "" {
		return errors.New("managed directory request is invalid")
	}
	inspection, err := filesystem.InspectManagedDirectory(ctx, layout, path)
	if err != nil {
		return err
	}
	if inspection.Exists {
		return nil
	}
	rootInspection, err := filesystem.InspectManagedDirectory(ctx, layout, layout.AppRoot())
	if err != nil {
		return err
	}
	if !rootInspection.Exists {
		return errors.New("managed app root is missing")
	}
	missing := make([]string, 0, 4)
	current := filepath.Clean(path)
	for {
		inspection, inspectErr := filesystem.InspectManagedDirectory(ctx, layout, current)
		if inspectErr != nil {
			return inspectErr
		}
		if inspection.Exists {
			break
		}
		missing = append(missing, current)
		if strings.EqualFold(current, layout.AppRoot()) {
			return errors.New("managed app root is missing")
		}
		parent := filepath.Dir(current)
		if strings.EqualFold(parent, current) {
			return errors.New("managed directory parent is invalid")
		}
		current = parent
	}
	for index := len(missing) - 1; index >= 0; index-- {
		lease, prepareErr := filesystem.PrepareManagedDirectory(ctx, layout, missing[index])
		if prepareErr != nil {
			return prepareErr
		}
		if closeErr := lease.Close(); closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func prepareStagingDirectory(
	ctx context.Context,
	layout *config.Layout,
	staging string,
) (*filesystem.DirectoryLease, error) {
	if ctx == nil || layout == nil || staging == "" {
		return nil, errors.New("uv staging directory request is invalid")
	}
	info, err := os.Lstat(staging)
	if errors.Is(err, os.ErrNotExist) {
		return filesystem.PrepareManagedDirectory(ctx, layout, staging)
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("uv staging directory is not a regular directory")
	}
	return filesystem.OpenManagedDirectory(ctx, layout, staging)
}

func invalidateDownloadCache(
	ctx context.Context,
	layout *config.Layout,
	path string,
) error {
	if ctx == nil || layout == nil || path == "" {
		return errors.New("uv cache invalidation request is invalid")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !regularFile(path) {
		return errors.New("uv download cache entry is not a regular file")
	}
	cacheDir := layout.DownloadCacheDir()
	if !samePath(filepath.Dir(path), cacheDir) {
		return errors.New("uv download cache entry is outside the managed cache")
	}
	lease, err := filesystem.OpenManagedDirectory(ctx, layout, cacheDir)
	if err != nil {
		return err
	}
	removeErr := os.Remove(path)
	closeErr := lease.Close()
	return errors.Join(removeErr, closeErr)
}

func wrapBootstrapError(stage protocol.Stage, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return newError(
		bootstrapErrorCode(err),
		stage,
		"uv 准备失败",
		map[string]any{},
		err,
	)
}

func bootstrapErrorCode(err error) protocol.Code {
	var coded interface{ Code() protocol.Code }
	if errors.As(err, &coded) && protocol.IsKnownCode(coded.Code()) {
		return coded.Code()
	}
	if errors.Is(err, filesystem.ErrUnsafeHardLink) {
		return protocol.CodeUnsafeReparsePoint
	}
	if errors.Is(err, filesystem.ErrIdentityChanged) {
		return protocol.CodePathOutsideManagedRoot
	}
	return protocol.CodeUVDownloadFailed
}

type zipExtractor struct{}

func (zipExtractor) Extract(ctx context.Context, archivePath, stagingDir string) error {
	if ctx == nil || archivePath == "" || stagingDir == "" {
		return errors.New("uv archive extraction request is invalid")
	}
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	if len(reader.File) > maxUVArchiveFiles {
		return errors.New("uv archive contains too many files")
	}
	var found bool
	var totalUncompressed uint64
	for _, entry := range reader.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Name == "" || filepath.IsAbs(filepath.FromSlash(entry.Name)) ||
			strings.Contains(entry.Name, "\\") || hasParentSegment(entry.Name) {
			return errors.New("uv archive entry path is unsafe")
		}
		if entry.Name != "uv.exe" {
			return errors.New("uv archive contains an unexpected entry")
		}
		fileInfo := entry.FileInfo()
		if found || !fileInfo.Mode().IsRegular() ||
			fileInfo.Mode()&os.ModeSymlink != 0 ||
			entry.UncompressedSize64 > maxUVArchiveBytes {
			return errors.New("uv archive uv.exe entry is invalid")
		}
		if totalUncompressed > maxUVArchiveBytes-entry.UncompressedSize64 {
			return errors.New("uv archive exceeds maximum uncompressed size")
		}
		totalUncompressed += entry.UncompressedSize64
		stagingInfo, err := os.Lstat(stagingDir)
		if err != nil {
			return err
		}
		if !stagingInfo.IsDir() || stagingInfo.Mode()&os.ModeSymlink != 0 {
			return errors.New("uv staging directory is unsafe")
		}
		destination := filepath.Join(stagingDir, "uv.exe")
		file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
		if err != nil {
			return err
		}
		input, err := entry.Open()
		if err == nil {
			written, copyErr := io.Copy(file, io.LimitReader(contextReader{ctx: ctx, reader: input}, maxUVArchiveBytes+1))
			err = copyErr
			if err == nil && written > maxUVArchiveBytes {
				err = errors.New("uv archive uv.exe is too large")
			}
		}
		if input != nil {
			_ = input.Close()
		}
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			removeErr := os.Remove(destination)
			if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, removeErr)
			}
			return err
		}
		found = true
	}
	if !found {
		return errors.New("uv archive does not contain uv.exe")
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func hasParentSegment(name string) bool {
	for _, segment := range strings.Split(name, "/") {
		if segment == ".." || segment == "." || segment == "" {
			return true
		}
	}
	return false
}

type filesystemPublisher struct {
	layout *config.Layout
}

type filesystemVersionRemover struct {
	layout *config.Layout
}

type runnerVersionChecker struct {
	layout *config.Layout
}

func (c runnerVersionChecker) Check(ctx context.Context, executable, expected string, line LineFunc) error {
	if c.layout == nil {
		return errors.New("uv version checker layout is invalid")
	}
	runner, err := NewRunner(RunnerConfig{
		Executable: executable,
		// --version 不依赖后端仓库；首次 bootstrap 时 repo 可能尚未创建，
		// 但受管 app-root 已由调用方准备并可作为稳定工作目录。
		ProjectDir:       c.layout.AppRoot(),
		PythonInstallDir: c.layout.PythonDir(),
		ProjectEnvDir:    c.layout.VenvDir(),
		CacheDir:         c.layout.UVCacheDir(),
	})
	if err != nil {
		return err
	}
	return runner.CheckVersion(ctx, expected, protocol.StageUVVerify, line)
}

func (p filesystemPublisher) Publish(
	ctx context.Context,
	request PublishRequest,
) (PublishResult, error) {
	if p.layout == nil {
		return PublishResult{}, errors.New("uv publisher layout is invalid")
	}
	if err := ensureStagingParent(ctx, p.layout, request.Source); err != nil {
		return PublishResult{}, err
	}
	if err := ensureManagedDirectory(ctx, p.layout, filepath.Dir(request.Destination)); err != nil {
		return PublishResult{}, err
	}
	logger, err := logging.New(ctx, p.layout, io.Discard, "uv-bootstrap", request.OperationID)
	if err != nil {
		return PublishResult{}, err
	}
	operator, err := filesystem.New(ctx, p.layout, deletionLogger{logger: logger})
	if err != nil {
		return PublishResult{}, errors.Join(err, logger.Close())
	}
	result, err := operator.AtomicRename(ctx, filesystem.RenameRequest{
		Kind:        filesystem.RenameUVStagingToVersion,
		Source:      request.Source,
		Destination: request.Destination,
		OperationID: request.OperationID,
		Version:     request.Version,
		Reason:      "uv bootstrap publish",
	})
	return PublishResult{MutationApplied: result.MutationApplied}, errors.Join(err, logger.Close())
}

func (r filesystemVersionRemover) RemoveTree(
	ctx context.Context,
	request filesystem.DeleteRequest,
) (filesystem.DeleteResult, error) {
	if r.layout == nil {
		return filesystem.DeleteResult{}, errors.New("uv version remover layout is invalid")
	}
	versionDir, err := r.layout.UVVersionDir(request.Version)
	if err != nil {
		return filesystem.DeleteResult{}, err
	}
	if err := ensureStagingParent(ctx, r.layout, versionDir); err != nil {
		return filesystem.DeleteResult{}, err
	}
	logger, err := logging.New(ctx, r.layout, io.Discard, "uv-repair", request.OperationID)
	if err != nil {
		return filesystem.DeleteResult{}, err
	}
	auditor := deletionLogger{logger: logger}
	operator, err := filesystem.New(ctx, r.layout, auditor)
	if err != nil {
		return filesystem.DeleteResult{}, errors.Join(err, logger.Close())
	}
	result, removeErr := operator.RemoveTree(ctx, request)
	return result, errors.Join(removeErr, logger.Close())
}

type deletionLogger struct {
	logger *logging.Logger
}

func (a deletionLogger) RecordDeletion(
	ctx context.Context,
	record filesystem.DeleteAuditRecord,
) error {
	if a.logger == nil {
		return errors.New("uv deletion logger is invalid")
	}
	_, err := a.logger.Record(ctx, logging.LevelInfo, "受控删除审计", map[string]any{
		"phase":       record.Phase.String(),
		"operationId": record.OperationID,
		"kind":        record.Kind.String(),
		"target":      record.Target,
		"reason":      record.Reason,
		"removed":     record.Removed,
		"partial":     record.Partial,
		"result":      record.Result,
	})
	return err
}

var (
	_ ArchiveExtractor = zipExtractor{}
	_ Publisher        = filesystemPublisher{}
	_ TreeRemover      = filesystemVersionRemover{}
)
