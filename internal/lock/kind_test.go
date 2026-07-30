//go:build windows

package lock

import (
	"errors"
	"strings"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestMutexName_UsesLocalNamespaceAndPhysicalIdentityHash(t *testing.T) {
	var fileID [16]byte
	for i := range fileID {
		fileID[i] = byte(i)
	}
	identity := rootIdentity{
		volumeSerial: 0x0102030405060708,
		fileID:       fileID,
	}
	const suffix = "83839c3a1d1a406c38e8b2a0d187f211"
	tests := []struct {
		name string
		kind Kind
		want string
	}{
		{
			name: "backend",
			kind: KindBackend,
			want: "Local\\AUTO-MAS-Runtime-backend-" + suffix,
		},
		{
			name: "mutation",
			kind: KindMutation,
			want: "Local\\AUTO-MAS-Runtime-mutation-" + suffix,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := mutexName(test.kind, identity)
			if err != nil {
				t.Fatalf("mutexName() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("mutexName() = %q, want %q", got, test.want)
			}
			if strings.Contains(strings.ToLower(got), `c:\`) {
				t.Fatalf("mutexName() = %q, want no absolute path", got)
			}
		})
	}
}

func TestKind_ValidAndString(t *testing.T) {
	tests := []struct {
		name  string
		kind  Kind
		valid bool
		text  string
	}{
		{name: "backend", kind: KindBackend, valid: true, text: "backend"},
		{name: "mutation", kind: KindMutation, valid: true, text: "mutation"},
		{name: "empty", kind: "", valid: false, text: ""},
		{name: "unknown", kind: "other", valid: false, text: "other"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.kind.Valid(); got != test.valid {
				t.Fatalf("Valid() = %t, want %t", got, test.valid)
			}
			if got := test.kind.String(); got != test.text {
				t.Fatalf("String() = %q, want %q", got, test.text)
			}
		})
	}
}

func TestMutexName_RejectsUnknownKindBeforeWin32(t *testing.T) {
	_, err := mutexName("unknown", rootIdentity{})
	if err == nil {
		t.Fatal("mutexName() error = nil, want invalid-kind error")
	}
}

func TestOperationError_UsesMutexOperationFailed(t *testing.T) {
	testRootOperationErrors(t)
}

func testRootOperationErrors(t *testing.T) {
	t.Helper()
	injectedErr := errors.New("injected root failure")
	tests := []struct {
		name      string
		operation string
		run       func() error
		cause     error
	}{
		{
			name:      "encode-root",
			operation: "encode-root",
			run: func() error {
				_, err := openRootIdentity(
					t.Context(),
					newTestWindowsAPI(),
					"bad\x00root",
				)
				return err
			},
		},
		{
			name:      "open-root",
			operation: "open-root",
			cause:     injectedErr,
			run: func() error {
				api := newTestWindowsAPI()
				api.createFileErr = injectedErr
				_, err := openRootIdentity(t.Context(), api, `C:\app`)
				return err
			},
		},
		{
			name:      "inspect-root",
			operation: "inspect-root",
			cause:     injectedErr,
			run: func() error {
				api := newTestWindowsAPI()
				api.inspectRootErr = injectedErr
				_, err := openRootIdentity(t.Context(), api, `C:\app`)
				return err
			},
		},
		{
			name:      "read-root-file-id",
			operation: "read-root-file-id",
			cause:     injectedErr,
			run: func() error {
				api := newTestWindowsAPI()
				api.fileIDErr = injectedErr
				_, err := openRootIdentity(t.Context(), api, `C:\app`)
				return err
			},
		},
		{
			name:      "close-root",
			operation: "close-root",
			cause:     injectedErr,
			run: func() error {
				api := newTestWindowsAPI()
				api.inspectRootErr = errors.New("force cleanup")
				api.closeErr = func(call apiCall) error {
					if call.Handle == testRootHandle {
						return injectedErr
					}
					return nil
				}
				_, err := openRootIdentity(t.Context(), api, `C:\app`)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertOperationError(
				t,
				test.run(),
				test.operation,
				test.cause,
			)
		})
	}
}

func assertOperationError(
	t *testing.T,
	err error,
	operation string,
	cause error,
) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s error = nil, want *OperationError", operation)
	}
	var found *OperationError
	walkErrors(err, func(candidate error) {
		var operationErr *OperationError
		if errors.As(candidate, &operationErr) &&
			operationErr.Operation == operation {
			found = operationErr
		}
	})
	if found == nil {
		t.Fatalf("error = %v, want operation %q", err, operation)
	}
	if got := found.Code(); got != protocol.CodeMutexOperationFailed {
		t.Fatalf(
			"Code() = %q, want %q",
			got,
			protocol.CodeMutexOperationFailed,
		)
	}
	if cause != nil && !errors.Is(err, cause) {
		t.Fatalf("error = %v, want injected cause", err)
	}
}

func walkErrors(err error, visit func(error)) {
	if err == nil {
		return
	}
	visit(err)
	switch value := err.(type) {
	case interface{ Unwrap() []error }:
		for _, child := range value.Unwrap() {
			walkErrors(child, visit)
		}
	case interface{ Unwrap() error }:
		walkErrors(value.Unwrap(), visit)
	}
}
