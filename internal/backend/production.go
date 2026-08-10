package backend

import (
	"context"
	"errors"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/gitrepo"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/logging"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/process"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/uv"
)

type productionEntryChecker struct {
	layout *config.Layout
}

func (c productionEntryChecker) Check(ctx context.Context, path string) error {
	if c.layout == nil || path == "" {
		return ErrEntryUnsafe
	}
	inspection, err := filesystem.InspectManagedFile(ctx, c.layout, path)
	if err != nil {
		return errors.Join(ErrEntryUnsafe, err)
	}
	if !inspection.Exists {
		return ErrEntryNotFound
	}
	return nil
}

type productionRepository struct{ service *gitrepo.Service }

func (r productionRepository) Check(ctx context.Context) (RepositoryResult, error) {
	result, err := r.service.Check(ctx)
	if err != nil {
		return RepositoryResult{}, err
	}
	return RepositoryResult{
		Healthy: result.Healthy,
		Version: result.Version,
		Commit:  result.Commit,
		Reason:  result.Reason,
	}, nil
}

type productionUV struct {
	runner   *uv.UVRunner
	expected string
}

func (r productionUV) Check(ctx context.Context) error {
	return r.runner.CheckVersion(ctx, r.expected, protocol.StageBackendSpawn, nil)
}

func (r productionUV) Executable() string { return r.runner.Executable }

func (r productionUV) StartManaged(
	ctx context.Context,
	args []string,
	options uv.ManagedOptions,
	sink process.StreamSink,
) (ManagedProcess, error) {
	return r.runner.StartManaged(ctx, args, options, sink)
}

type productionLogger struct{ logger *logging.Logger }

func (l productionLogger) Record(ctx context.Context, record process.StreamRecord) error {
	message := record.Fragment
	_, err := l.logger.Record(ctx, logging.LevelInfo, message, map[string]any{
		"stream":        record.Stream,
		"lineId":        record.LineID,
		"endOfLine":     record.EndOfLine,
		"truncated":     record.Truncated,
		"originalBytes": record.OriginalBytes,
	})
	return err
}

func (l productionLogger) LogPath() string { return l.logger.LogPath() }
func (l productionLogger) Close() error    { return l.logger.Close() }
