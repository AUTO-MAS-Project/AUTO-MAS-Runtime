package logging

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

type logWriter interface {
	io.Writer
	Path() string
	Close() error
}

type retainedFile interface {
	Name() string
	Path() string
	retainedLogFile()
}

type removeResult struct {
	mutationApplied bool
}

type logFiles interface {
	OpenAppend(ctx context.Context, command string, localDate time.Time) (logWriter, error)
	List(ctx context.Context) ([]retainedFile, error)
	Remove(ctx context.Context, file retainedFile) (removeResult, error)
	Close() error
}

type logFilesFactory func(
	ctx context.Context,
	layout *config.Layout,
) (logFiles, error)

func newFilesystemLogFiles(
	ctx context.Context,
	layout *config.Layout,
) (logFiles, error) {
	files, err := filesystem.NewRuntimeLogFiles(ctx, layout)
	if err != nil {
		return nil, err
	}
	return &filesystemLogFiles{files: files}, nil
}

type filesystemLogFiles struct {
	files *filesystem.RuntimeLogFiles
}

type filesystemLogWriter struct {
	writer *filesystem.RuntimeLogWriter
}

type filesystemRetainedFile struct {
	token filesystem.RuntimeLogFile
}

func (f *filesystemLogFiles) OpenAppend(
	ctx context.Context,
	command string,
	localDate time.Time,
) (logWriter, error) {
	writer, err := f.files.OpenAppend(ctx, command, localDate)
	if err != nil {
		return nil, err
	}
	return &filesystemLogWriter{writer: writer}, nil
}

func (f *filesystemLogFiles) List(ctx context.Context) ([]retainedFile, error) {
	files, err := f.files.List(ctx)
	if err != nil {
		return nil, err
	}
	retained := make([]retainedFile, 0, len(files))
	for _, file := range files {
		retained = append(retained, filesystemRetainedFile{token: file})
	}
	return retained, nil
}

func (f *filesystemLogFiles) Remove(
	ctx context.Context,
	file retainedFile,
) (removeResult, error) {
	retained, ok := file.(filesystemRetainedFile)
	if !ok {
		return removeResult{}, fmt.Errorf("remove runtime log token: %w", filesystem.ErrInvalidToken)
	}
	result, err := f.files.Remove(ctx, retained.token)
	return removeResult{mutationApplied: result.MutationApplied}, err
}

func (f *filesystemLogFiles) Close() error {
	return f.files.Close()
}

func (w *filesystemLogWriter) Write(p []byte) (int, error) {
	return w.writer.Write(p)
}

func (w *filesystemLogWriter) Path() string {
	return w.writer.Path()
}

func (w *filesystemLogWriter) Close() error {
	return w.writer.Close()
}

func (f filesystemRetainedFile) Name() string {
	return f.token.Name()
}

func (f filesystemRetainedFile) Path() string {
	return f.token.Path()
}

func (filesystemRetainedFile) retainedLogFile() {}
