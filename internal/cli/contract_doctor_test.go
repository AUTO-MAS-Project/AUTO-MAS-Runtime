package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/doctor"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol/contracttest"
)

// TestContract_RegisterDoctor 以 contracttest.Register 一行接入通用契约测试。
func TestContract_RegisterDoctor(t *testing.T) {
	contracttest.Register(t, "doctor", doctorRunner)
}

func doctorRunner(t *testing.T, terminal contracttest.Terminal) contracttest.Transcript {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	options := []Option{
		WithCWD(t.TempDir()),
		WithClock(func() time.Time {
			return time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
		}),
	}
	ctx := context.Background()
	var args []string
	switch terminal {
	case contracttest.TerminalSuccess:
		args = []string{"--output", "ndjson", "--app-root", healthyDoctorFixture(t), "doctor"}
		options = append(options, WithDoctorFactory(func(layout *config.Layout, _ doctor.Probes) (doctorService, error) {
			return doctor.New(layout, doctorTestProbes())
		}))
	case contracttest.TerminalFailure:
		args = []string{"--output", "ndjson", "doctor"}
		options = append(options, WithDoctorFactory(func(*config.Layout, doctor.Probes) (doctorService, error) {
			return fakeDoctorService{
				run: func(context.Context, *protocol.Emitter) (doctor.Report, error) {
					return doctor.Report{}, doctor.NewError(
						protocol.CodeMutexOperationFailed,
						protocol.StageDoctor,
						"锁探测失败",
						map[string]any{},
						nil,
					)
				},
			}, nil
		}))
	case contracttest.TerminalCancelled:
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		ctx = cancelled
		args = []string{"--output", "ndjson", "doctor"}
		options = append(options, WithDoctorFactory(func(*config.Layout, doctor.Probes) (doctorService, error) {
			return fakeDoctorService{
				run: func(context.Context, *protocol.Emitter) (doctor.Report, error) {
					return doctor.Report{}, context.Canceled
				},
			}, nil
		}))
	default:
		t.Fatalf("unexpected terminal %q", terminal)
	}
	code := Execute(
		ctx,
		args,
		IO{
			In:  strings.NewReader(""),
			Out: &stdout,
			Err: &stderr,
		},
		options...,
	)
	switch terminal {
	case contracttest.TerminalSuccess:
		if code != 0 {
			t.Errorf("exit code = %d, want 0", code)
		}
	case contracttest.TerminalFailure:
		if code != protocol.ExitCodeOperationConflict {
			t.Errorf("exit code = %d, want %d", code, protocol.ExitCodeOperationConflict)
		}
	case contracttest.TerminalCancelled:
		if code != 130 {
			t.Errorf("exit code = %d, want 130", code)
		}
	}
	return contracttest.Transcript{Stdout: stdout.Bytes()}
}
