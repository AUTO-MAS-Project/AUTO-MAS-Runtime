package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
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
	OpenNext(ctx context.Context, command string, localDate time.Time) (logWriter, error)
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
	return &filesystemLogFiles{files: files, layout: layout}, nil
}

type filesystemLogFiles struct {
	files  *filesystem.RuntimeLogFiles
	layout *config.Layout
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
	sequence, err := f.latestSequence(ctx, command, localDate)
	if err != nil {
		return nil, err
	}
	writer, err := f.files.OpenAppendSegment(ctx, command, localDate, sequence)
	if err != nil {
		return nil, err
	}
	return &filesystemLogWriter{writer: writer}, nil
}

func (f *filesystemLogFiles) OpenNext(ctx context.Context, command string, localDate time.Time) (logWriter, error) {
	for range 16 {
		sequence, err := f.latestSequence(ctx, command, localDate)
		if err != nil {
			return nil, err
		}
		if sequence == ^uint64(0) {
			return nil, fmt.Errorf("runtime log segment sequence exhausted: %w", filesystem.ErrRuntimeLogCapacity)
		}
		writer, err := f.files.CreateSegment(ctx, command, localDate, sequence+1)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return &filesystemLogWriter{writer: writer}, nil
	}
	return nil, fmt.Errorf("runtime log segment creation contention: %w", fs.ErrExist)
}

func (f *filesystemLogFiles) latestSequence(ctx context.Context, command string, localDate time.Time) (uint64, error) {
	files, err := f.List(ctx)
	if err != nil {
		return 0, err
	}
	var sequence uint64
	for _, file := range files {
		candidate, ok := parseRetainedFile(f.layout, file, localDate.Location())
		if ok && strings.EqualFold(candidate.command, command) && candidate.localDate.Format("20060102") == localDate.Format("20060102") && candidate.sequence > sequence {
			sequence = candidate.sequence
		}
	}
	return sequence, nil
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
