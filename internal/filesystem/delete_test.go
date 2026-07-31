package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

type recordingDeleteAuditor struct {
	records []DeleteAuditRecord
	err     error
}

func (a *recordingDeleteAuditor) RecordDeletion(
	_ context.Context,
	record DeleteAuditRecord,
) error {
	a.records = append(a.records, record)
	return a.err
}

func TestNew_RejectsInvalidLayoutAuditorAndOptions(t *testing.T) {
	layout := newDeleteTestLayout(t)
	auditor := &recordingDeleteAuditor{}
	tests := []struct {
		name    string
		ctx     context.Context
		layout  *config.Layout
		auditor Auditor
		options []Option
	}{
		{name: "nil context", ctx: nil, layout: layout, auditor: auditor},
		{name: "nil layout", ctx: t.Context(), auditor: auditor},
		{name: "nil auditor", ctx: t.Context(), layout: layout},
		{name: "nil option", ctx: t.Context(), layout: layout, auditor: auditor, options: []Option{nil}},
		{
			name:    "nil wait",
			ctx:     t.Context(),
			layout:  layout,
			auditor: auditor,
			options: []Option{WithWait(nil)},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operator, err := New(test.ctx, test.layout, test.auditor, test.options...)
			if operator != nil {
				t.Fatalf("New() operator = %#v, want nil", operator)
			}
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("New() error = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func TestNew_RejectsDuplicateOrUnboundedRenameOptions(t *testing.T) {
	layout := newDeleteTestLayout(t)
	auditor := &recordingDeleteAuditor{}
	wait := func(context.Context, time.Duration) error { return nil }
	seventeen := make([]time.Duration, 17)
	for i := range seventeen {
		seventeen[i] = time.Millisecond
	}
	tests := []struct {
		name    string
		options []Option
	}{
		{name: "duplicate wait", options: []Option{WithWait(wait), WithWait(wait)}},
		{
			name: "duplicate delays",
			options: []Option{
				WithRenameDelays(time.Millisecond),
				WithRenameDelays(time.Millisecond),
			},
		},
		{name: "empty delays", options: []Option{WithRenameDelays()}},
		{name: "zero delay", options: []Option{WithRenameDelays(0)}},
		{name: "negative delay", options: []Option{WithRenameDelays(-time.Millisecond)}},
		{name: "seventeen delays", options: []Option{WithRenameDelays(seventeen...)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operator, err := New(t.Context(), layout, auditor, test.options...)
			if operator != nil {
				t.Fatalf("New() operator = %#v, want nil", operator)
			}
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("New() error = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func TestNew_ContextRejectedBeforeIO(t *testing.T) {
	layout := newDeleteTestLayout(t)
	auditor := &recordingDeleteAuditor{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	operator, err := New(ctx, layout, auditor)
	if operator != nil {
		t.Fatalf("New() operator = %#v, want nil", operator)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("New() error = %v, want context.Canceled", err)
	}
	if len(auditor.records) != 0 {
		t.Fatalf("audit calls = %d, want 0", len(auditor.records))
	}
}

func TestNew_MatchesDirectoryPinMatrixAndParentIdentity(t *testing.T) {
	layout := newDeleteTestLayout(t)
	auditor := &recordingDeleteAuditor{}
	api := newProductionPathAPI()
	openPath := api.openPath
	var specs []openSpec
	api.openPath = func(path string, spec openSpec) (windows.Handle, error) {
		specs = append(specs, spec)
		return openPath(path, spec)
	}
	_, err := newWithDependencies(
		t.Context(),
		layout,
		auditor,
		options{wait: defaultWait, delays: defaultRenameDelays()},
		operatorDependencies{
			api: api,
			finishedContext: func(
				ctx context.Context,
			) (context.Context, context.CancelFunc) {
				return context.WithCancel(context.WithoutCancel(ctx))
			},
		},
	)
	if err != nil {
		t.Fatalf("newWithDependencies() error = %v", err)
	}
	if len(specs) == 0 {
		t.Fatal("directory pin specs = 0, want at least 1")
	}
	found := false
	for _, spec := range specs {
		if spec == directoryPinSpec() {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("open specs = %#v, want directory pin spec", specs)
	}
}

func TestAuthorizeDeleteRequest_AcceptsOnlyExactLayoutIdentity(t *testing.T) {
	fixture := newDeleteFixture(t)
	type requestCase struct {
		name          string
		kind          DeleteKind
		target        string
		version       string
		dynamicTarget func(operationID string) string
	}
	repoUpdate := func(operationID string) string {
		path, err := fixture.layout.RepoUpdateDir(operationID)
		if err != nil {
			t.Fatalf("RepoUpdateDir() error = %v", err)
		}
		return path
	}
	repoRetired := func(operationID string) string {
		path, err := fixture.layout.RepoPreviousDir(operationID)
		if err != nil {
			t.Fatalf("RepoPreviousDir() error = %v", err)
		}
		return path
	}
	uvStaging := func(operationID string) string {
		path, err := fixture.layout.UVStagingDir("0.9.0", operationID)
		if err != nil {
			t.Fatalf("UVStagingDir() error = %v", err)
		}
		return path
	}
	tests := []requestCase{
		{name: "uv cache", kind: DeleteUVCache, target: fixture.layout.UVCacheDir()},
		{name: "venv", kind: DeleteManagedVenv, target: fixture.layout.VenvDir()},
		{name: "python", kind: DeleteManagedPython, target: fixture.layout.PythonDir()},
		{
			name:          "repo update",
			kind:          DeleteRepositoryUpdate,
			dynamicTarget: repoUpdate,
		},
		{
			name:          "repo retired",
			kind:          DeleteRepositoryRetired,
			dynamicTarget: repoRetired,
		},
		{
			name:   "download temporary",
			kind:   DeleteDownloadTemporary,
			target: fixture.layout.DownloadCacheDir(),
		},
		{
			name:          "uv staging",
			kind:          DeleteUVStaging,
			version:       "0.9.0",
			dynamicTarget: uvStaging,
		},
		{
			name:   "python cache",
			kind:   DeletePythonCache,
			target: filepath.Join(fixture.layout.RepoDir(), "pkg", "__pycache__"),
		},
		{name: "build cache", kind: DeleteBuildCache, target: fixture.layout.BuildCacheDir()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := test.target
			if test.dynamicTarget != nil {
				target = test.dynamicTarget("operation-a")
			}
			request := DeleteRequest{
				Kind:        test.kind,
				Target:      target,
				OperationID: "operation-a",
				Version:     test.version,
				Reason:      "test cleanup",
			}
			authorized, err := fixture.operator.authorizeDeleteRequest(t.Context(), request)
			if err != nil {
				t.Fatalf("authorizeDeleteRequest() error = %v", err)
			}
			if closeErr := authorized.chain.close(); closeErr != nil {
				t.Fatalf("authorized chain close error = %v", closeErr)
			}

			if test.dynamicTarget == nil {
				request.OperationID = "another-safe-audit-id"
				authorized, err = fixture.operator.authorizeDeleteRequest(t.Context(), request)
				if err != nil {
					t.Fatalf("fixed identity with alternate OperationID error = %v", err)
				}
				if closeErr := authorized.chain.close(); closeErr != nil {
					t.Fatalf("alternate chain close error = %v", closeErr)
				}
				return
			}
			request.OperationID = "operation-b"
			_, err = fixture.operator.authorizeDeleteRequest(t.Context(), request)
			assertPathOutside(t, err)
		})
	}
}

func TestAuthorizeDeleteRequest_RejectsInvalidFieldsBeforeIO(t *testing.T) {
	fixture := newDeleteFixture(t)
	api := fixture.operator.api
	attributes := api.attributes
	ioCalls := 0
	api.attributes = func(path string) (uint32, error) {
		ioCalls++
		return attributes(path)
	}
	fixture.operator.api = api

	valid := DeleteRequest{
		Kind:        DeleteUVCache,
		Target:      fixture.layout.UVCacheDir(),
		OperationID: "operation-a",
		Reason:      "cleanup",
	}
	tests := []struct {
		name   string
		change func(*DeleteRequest)
	}{
		{name: "unknown kind", change: func(r *DeleteRequest) { r.Kind = "unknown" }},
		{name: "empty target", change: func(r *DeleteRequest) { r.Target = "" }},
		{name: "empty operation", change: func(r *DeleteRequest) { r.OperationID = "" }},
		{name: "operation nul", change: func(r *DeleteRequest) { r.OperationID = "a\x00b" }},
		{name: "operation cr", change: func(r *DeleteRequest) { r.OperationID = "a\rb" }},
		{name: "operation lf", change: func(r *DeleteRequest) { r.OperationID = "a\nb" }},
		{name: "empty reason", change: func(r *DeleteRequest) { r.Reason = "" }},
		{name: "reason lf", change: func(r *DeleteRequest) { r.Reason = "a\nb" }},
		{name: "non-staging version", change: func(r *DeleteRequest) { r.Version = "0.9.0" }},
		{
			name: "staging empty version",
			change: func(r *DeleteRequest) {
				r.Kind = DeleteUVStaging
				r.Version = ""
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ioCalls = 0
			request := valid
			test.change(&request)
			_, err := fixture.operator.authorizeDeleteRequest(t.Context(), request)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("authorizeDeleteRequest() error = %v, want ErrInvalidArgument", err)
			}
			if ioCalls != 0 {
				t.Fatalf("I/O calls = %d, want 0", ioCalls)
			}
			if len(fixture.auditor.records) != 0 {
				t.Fatalf("audit calls = %d, want 0", len(fixture.auditor.records))
			}
		})
	}
}

func TestAuthorizeDeleteRequest_RejectsRootsProtectedAndOutside(t *testing.T) {
	fixture := newDeleteFixture(t)
	outside := filepath.Join(t.TempDir(), "cache")
	tests := []string{
		fixture.layout.AppRoot(),
		fixture.layout.RepoDir(),
		fixture.layout.StateDir(),
		fixture.layout.LogsDir(),
		fixture.layout.ConfigDir(),
		filepath.Join(fixture.layout.ConfigDir(), "child"),
		outside,
	}
	for _, target := range tests {
		t.Run(strings.ReplaceAll(target, `\`, "_"), func(t *testing.T) {
			_, err := fixture.operator.authorizeDeleteRequest(t.Context(), DeleteRequest{
				Kind:        DeleteBuildCache,
				Target:      target,
				OperationID: "operation-a",
				Reason:      "cleanup",
			})
			assertPathOutside(t, err)
		})
	}
}

func TestAuthorizeDeleteRequest_NotFoundStillRequiresValidIdentity(t *testing.T) {
	fixture := newDeleteFixture(t)
	authorized, err := fixture.operator.authorizeDeleteRequest(t.Context(), DeleteRequest{
		Kind:        DeleteBuildCache,
		Target:      fixture.layout.BuildCacheDir(),
		OperationID: "operation-a",
		Reason:      "cleanup",
	})
	if err != nil {
		t.Fatalf("authorizeDeleteRequest() error = %v", err)
	}
	if authorized.exists {
		t.Fatal("authorized.exists = true, want false")
	}
	if err := authorized.chain.close(); err != nil {
		t.Fatalf("authorized chain close error = %v", err)
	}

	_, err = fixture.operator.authorizeDeleteRequest(t.Context(), DeleteRequest{
		Kind:        DeleteBuildCache,
		Target:      filepath.Join(fixture.layout.AppRoot(), "not-build-cache"),
		OperationID: "operation-a",
		Reason:      "cleanup",
	})
	assertPathOutside(t, err)
}

func TestRecordDeleteStarted_FailureReturnsNonAppliedAuditError(t *testing.T) {
	fixture := newDeleteFixture(t)
	injected := errors.New("audit unavailable")
	fixture.auditor.err = injected
	target := mustCanonicalize(t, fixture.layout.BuildCacheDir())
	err := fixture.operator.recordDeleteStarted(t.Context(), authorizedDelete{
		request: DeleteRequest{
			Kind:        DeleteBuildCache,
			Target:      fixture.layout.BuildCacheDir(),
			OperationID: "operation-a",
			Reason:      "cleanup",
		},
		target: target,
	})
	var auditErr *AuditError
	if !errors.As(err, &auditErr) {
		t.Fatalf("error = %v, want *AuditError", err)
	}
	if auditErr.Phase != DeleteAuditStarted || auditErr.MutationApplied {
		t.Fatalf("AuditError = %#v, want started/non-applied", auditErr)
	}
	if !errors.Is(err, injected) {
		t.Fatalf("error = %v, want injected cause", err)
	}
}

type deleteFixture struct {
	layout   *config.Layout
	auditor  *recordingDeleteAuditor
	operator *Operator
}

func newDeleteFixture(t *testing.T) deleteFixture {
	t.Helper()
	layout := newDeleteTestLayout(t)
	if err := os.MkdirAll(layout.RepoDir(), 0o700); err != nil {
		t.Fatalf("os.MkdirAll(repo) error = %v", err)
	}
	auditor := &recordingDeleteAuditor{}
	operator, err := New(t.Context(), layout, auditor)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return deleteFixture{layout: layout, auditor: auditor, operator: operator}
}

func newDeleteTestLayout(t *testing.T) *config.Layout {
	t.Helper()
	root := t.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	return layout
}

func assertPathOutside(t *testing.T, err error) {
	t.Helper()
	var filesystemErr *Error
	if !errors.As(err, &filesystemErr) {
		t.Fatalf("error = %v, want *Error", err)
	}
	if got := filesystemErr.Code(); got != protocol.CodePathOutsideManagedRoot {
		t.Fatalf("Code() = %q, want %q", got, protocol.CodePathOutsideManagedRoot)
	}
}
