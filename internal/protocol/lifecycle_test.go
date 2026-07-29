package protocol_test

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// documentedLifecycleTransitions 从 T1.3 设计文档的两张迁移表读取权威顺序。
func documentedLifecycleTransitions(t *testing.T) []protocol.LifecycleTransition {
	t.Helper()

	content := readDoc(t, "设计-T1.3-生命周期状态机.md")
	preparation := docSection(t, content, "### 环境准备与恢复迁移", "### 后端监督迁移")
	backend := docSection(t, content, "### 后端监督迁移", "### 迁移验证 API")

	transitions := make([]protocol.LifecycleTransition, 0, 28)
	transitions = append(transitions, parseLifecycleTransitionSection(t, preparation, 16)...)
	transitions = append(transitions, parseLifecycleTransitionSection(t, backend, 12)...)
	if len(transitions) != 28 {
		t.Fatalf("documented lifecycle transition count = %d, want 28", len(transitions))
	}

	seen := make(map[protocol.LifecycleTransition]struct{}, len(transitions))
	for _, transition := range transitions {
		if _, exists := seen[transition]; exists {
			t.Fatalf("documented lifecycle transitions contain duplicate %q -> %q", transition.From, transition.To)
		}
		seen[transition] = struct{}{}
	}
	return transitions
}

func parseLifecycleTransitionSection(
	t *testing.T,
	section string,
	wantCount int,
) []protocol.LifecycleTransition {
	t.Helper()

	const (
		tableHeader    = "| From | To | 使用场景 |"
		tableSeparator = "| --- | --- | --- |"
	)

	lines := strings.Split(strings.ReplaceAll(section, "\r\n", "\n"), "\n")
	headerIndex := -1
	for i, rawLine := range lines {
		if strings.TrimSpace(rawLine) == tableHeader {
			headerIndex = i
			break
		}
	}
	if headerIndex < 0 {
		t.Fatalf("lifecycle transition table missing header %q", tableHeader)
	}
	if headerIndex+1 >= len(lines) || strings.TrimSpace(lines[headerIndex+1]) != tableSeparator {
		t.Fatalf("lifecycle transition table missing separator %q after header", tableSeparator)
	}

	transitions := make([]protocol.LifecycleTransition, 0, wantCount)
	for lineNumber := headerIndex + 2; lineNumber < len(lines); lineNumber++ {
		rawLine := lines[lineNumber]
		line := strings.TrimSpace(rawLine)
		if line == "" || !strings.HasPrefix(line, "|") {
			break
		}

		cells := strings.Split(line, "|")
		if len(cells) != 5 {
			t.Fatalf("malformed lifecycle transition row at section line %d: %q", lineNumber+1, rawLine)
		}
		from, ok := parseLifecycleStateCell(cells[1])
		if !ok {
			t.Fatalf("malformed lifecycle transition source at section line %d: %q", lineNumber+1, rawLine)
		}
		to, ok := parseLifecycleStateCell(cells[2])
		if !ok {
			t.Fatalf("malformed lifecycle transition target at section line %d: %q", lineNumber+1, rawLine)
		}
		if !protocol.IsKnownStateStatus(from) || !protocol.IsKnownStateStatus(to) {
			t.Fatalf("documented lifecycle transition uses unknown state at section line %d: %q", lineNumber+1, rawLine)
		}
		transitions = append(transitions, protocol.LifecycleTransition{From: from, To: to})
	}
	if len(transitions) != wantCount {
		t.Fatalf("documented lifecycle transition rows = %d, want %d", len(transitions), wantCount)
	}
	return transitions
}

func parseLifecycleStateCell(cell string) (protocol.StateStatus, bool) {
	value := strings.TrimSpace(cell)
	if len(value) < 2 || !strings.HasPrefix(value, "`") || !strings.HasSuffix(value, "`") {
		return "", false
	}
	value = strings.TrimSuffix(strings.TrimPrefix(value, "`"), "`")
	if value == "" || strings.Contains(value, "`") {
		return "", false
	}
	return protocol.StateStatus(value), true
}

func TestLifecycleTransitionsMatchSpecification(t *testing.T) {
	t.Parallel()

	want := documentedLifecycleTransitions(t)
	if got := protocol.AllLifecycleTransitions(); !reflect.DeepEqual(got, want) {
		t.Fatalf("AllLifecycleTransitions() = %#v, documented %#v", got, want)
	}
	if got := len(protocol.AllLifecycleTransitions()); got != 28 {
		t.Fatalf("len(AllLifecycleTransitions()) = %d, want 28", got)
	}

	for _, transition := range want {
		if !protocol.IsKnownLifecycleTransition(transition.From, transition.To) {
			t.Errorf("IsKnownLifecycleTransition(%q, %q) = false, want true", transition.From, transition.To)
		}
	}
	for _, transition := range []protocol.LifecycleTransition{
		{From: protocol.StateUninitialized, To: protocol.StateReadyToStart},
		{From: protocol.StateReadyToStart, To: protocol.StateReadyToStart},
		{From: protocol.StateStatus("future_from"), To: protocol.StatePreparingUV},
		{From: protocol.StateReadyToStart, To: protocol.StateStatus("future_to")},
	} {
		if protocol.IsKnownLifecycleTransition(transition.From, transition.To) {
			t.Errorf("IsKnownLifecycleTransition(%q, %q) = true, want false", transition.From, transition.To)
		}
	}
}

func TestAllLifecycleTransitionsReturnsDefensiveCopy(t *testing.T) {
	t.Parallel()

	want := documentedLifecycleTransitions(t)
	got := protocol.AllLifecycleTransitions()
	if len(got) == 0 {
		t.Fatal("AllLifecycleTransitions() returned no transitions")
	}
	got[0] = protocol.LifecycleTransition{
		From: protocol.StateStopped,
		To:   protocol.StateUninitialized,
	}

	if fresh := protocol.AllLifecycleTransitions(); !reflect.DeepEqual(fresh, want) {
		t.Fatalf("mutating returned transitions changed source: got %#v, documented %#v", fresh, want)
	}
}

func TestNewLifecycleMachineAcceptsOnlyStableInitialStates(t *testing.T) {
	t.Parallel()

	stable := []protocol.StateStatus{
		protocol.StateUninitialized,
		protocol.StateEnvironmentBroken,
		protocol.StateReadyToStart,
	}
	for _, initial := range stable {
		initial := initial
		t.Run(string(initial), func(t *testing.T) {
			t.Parallel()

			machine, err := protocol.NewLifecycleMachine(initial)
			if err != nil {
				t.Fatalf("NewLifecycleMachine(%q) error = %v", initial, err)
			}
			if got := machine.Initial(); got != initial {
				t.Errorf("Initial() = %q, want %q", got, initial)
			}
			if got := machine.Current(); got != initial {
				t.Errorf("Current() = %q, want %q", got, initial)
			}
			if machine.RestartUsed() {
				t.Error("RestartUsed() = true, want false")
			}
		})
	}

	unstable := []protocol.StateStatus{
		protocol.StatePreparingUV,
		protocol.StateSyncingRepository,
		protocol.StatePreparingPython,
		protocol.StateSyncingEnvironment,
		protocol.StateStartingBackend,
		protocol.StateRunning,
		protocol.StateRestarting,
		protocol.StateStoppingBackend,
		protocol.StateBackendFailed,
		protocol.StateStopped,
		protocol.StateStatus("future_state"),
	}
	for _, initial := range unstable {
		initial := initial
		t.Run("reject_"+string(initial), func(t *testing.T) {
			t.Parallel()

			if machine, err := protocol.NewLifecycleMachine(initial); err == nil {
				t.Fatalf("NewLifecycleMachine(%q) = %#v, nil; want error", initial, machine)
			}
		})
	}
}

func TestLifecycleMachineExecutesEveryRegularTransition(t *testing.T) {
	t.Parallel()

	transitions := documentedLifecycleTransitions(t)
	for _, transition := range transitions {
		transition := transition
		t.Run(fmt.Sprintf("%s_to_%s", transition.From, transition.To), func(t *testing.T) {
			t.Parallel()

			initial, prefix, ok := pathToLifecycleState(transition.From, transitions)
			if !ok {
				t.Fatalf("test specification has no path to %q", transition.From)
			}
			machine, err := protocol.NewLifecycleMachine(initial)
			if err != nil {
				t.Fatal(err)
			}
			for _, next := range prefix {
				if err := machine.Transition(next); err != nil {
					t.Fatalf("Transition(%q -> %q) error = %v", machine.Current(), next, err)
				}
				if got := machine.Current(); got != next {
					t.Fatalf("Current() = %q after transition, want %q", got, next)
				}
				if got := machine.Initial(); got != initial {
					t.Fatalf("Initial() = %q after transition, want %q", got, initial)
				}
			}
			if transition.From == protocol.StateRunning && transition.To == protocol.StateBackendFailed {
				for _, next := range []protocol.StateStatus{
					protocol.StateRestarting,
					protocol.StateRunning,
				} {
					if err := machine.Transition(next); err != nil {
						t.Fatalf("preparing stateful guard with Transition(%q -> %q) error = %v", machine.Current(), next, err)
					}
				}
			}
			if err := machine.Transition(transition.To); err != nil {
				t.Fatalf("Transition(%q -> %q) error = %v", machine.Current(), transition.To, err)
			}
			if got := machine.Current(); got != transition.To {
				t.Fatalf("Current() = %q after transition, want %q", got, transition.To)
			}
			if got := machine.Initial(); got != initial {
				t.Fatalf("Initial() = %q after transition, want %q", got, initial)
			}
		})
	}
}

func TestLifecycleMachineMainPaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		initial protocol.StateStatus
		path    []protocol.StateStatus
	}{
		{
			name:    "bootstrap",
			initial: protocol.StateUninitialized,
			path: []protocol.StateStatus{
				protocol.StatePreparingUV,
				protocol.StateSyncingRepository,
				protocol.StatePreparingPython,
				protocol.StateSyncingEnvironment,
				protocol.StateReadyToStart,
			},
		},
		{
			name:    "repair",
			initial: protocol.StateEnvironmentBroken,
			path: []protocol.StateStatus{
				protocol.StatePreparingUV,
				protocol.StatePreparingPython,
				protocol.StateSyncingEnvironment,
				protocol.StateReadyToStart,
			},
		},
		{
			name:    "dependencies_retry",
			initial: protocol.StateEnvironmentBroken,
			path: []protocol.StateStatus{
				protocol.StateSyncingEnvironment,
				protocol.StateReadyToStart,
			},
		},
		{
			name:    "idempotent_bootstrap",
			initial: protocol.StateReadyToStart,
			path: []protocol.StateStatus{
				protocol.StatePreparingUV,
				protocol.StateSyncingRepository,
				protocol.StatePreparingPython,
				protocol.StateSyncingEnvironment,
				protocol.StateReadyToStart,
			},
		},
		{
			name:    "normal_backend_shutdown",
			initial: protocol.StateReadyToStart,
			path: []protocol.StateStatus{
				protocol.StateStartingBackend,
				protocol.StateRunning,
				protocol.StateStoppingBackend,
				protocol.StateStopped,
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			machine, err := protocol.NewLifecycleMachine(test.initial)
			if err != nil {
				t.Fatal(err)
			}
			for _, next := range test.path {
				if err := machine.Transition(next); err != nil {
					t.Fatalf("Transition(%q -> %q) error = %v", machine.Current(), next, err)
				}
				if got := machine.Current(); got != next {
					t.Fatalf("Current() = %q, want %q", got, next)
				}
				if got := machine.Initial(); got != test.initial {
					t.Fatalf("Initial() = %q, want %q", got, test.initial)
				}
			}
		})
	}
}

func TestLifecycleMachine_StandaloneWorkspaceSync(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		initial    protocol.StateStatus
		actualSwap bool
		want       protocol.StateStatus
	}{
		{
			name:    "uninitialized_no_op",
			initial: protocol.StateUninitialized,
			want:    protocol.StateUninitialized,
		},
		{
			name:       "uninitialized_actual_swap",
			initial:    protocol.StateUninitialized,
			actualSwap: true,
			want:       protocol.StateEnvironmentBroken,
		},
		{
			name:    "environment_broken_no_op",
			initial: protocol.StateEnvironmentBroken,
			want:    protocol.StateEnvironmentBroken,
		},
		{
			name:       "environment_broken_actual_swap",
			initial:    protocol.StateEnvironmentBroken,
			actualSwap: true,
			want:       protocol.StateEnvironmentBroken,
		},
		{
			name:    "ready_to_start_no_op",
			initial: protocol.StateReadyToStart,
			want:    protocol.StateReadyToStart,
		},
		{
			name:       "ready_to_start_actual_swap",
			initial:    protocol.StateReadyToStart,
			actualSwap: true,
			want:       protocol.StateEnvironmentBroken,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			machine := newLifecycleMachine(t, test.initial)
			if err := machine.Transition(protocol.StateSyncingRepository); err != nil {
				t.Fatalf("Transition(%q -> %q) error = %v", test.initial, protocol.StateSyncingRepository, err)
			}
			if got := machine.Current(); got != protocol.StateSyncingRepository {
				t.Fatalf("Current() = %q after entering standalone sync, want %q", got, protocol.StateSyncingRepository)
			}

			if test.actualSwap {
				if err := machine.Transition(protocol.StateEnvironmentBroken); err != nil {
					t.Fatalf("Transition(%q -> %q) error = %v", protocol.StateSyncingRepository, protocol.StateEnvironmentBroken, err)
				}
			} else if err := machine.RollbackPreparation(); err != nil {
				t.Fatalf("RollbackPreparation() error = %v", err)
			}

			if got := machine.Current(); got != test.want {
				t.Fatalf("Current() = %q after standalone sync, want %q", got, test.want)
			}
			if got := machine.Initial(); got != test.initial {
				t.Fatalf("Initial() = %q after standalone sync, want %q", got, test.initial)
			}
			if machine.RestartUsed() {
				t.Fatal("RestartUsed() = true after standalone sync, want false")
			}
		})
	}
}

func TestLifecycleMachineRejectsUnknownSelfLoopAndIllegalTransitions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		machine func(t *testing.T) *protocol.LifecycleMachine
		next    protocol.StateStatus
	}{
		{
			name: "unknown_from",
			machine: func(*testing.T) *protocol.LifecycleMachine {
				return &protocol.LifecycleMachine{}
			},
			next: protocol.StatePreparingUV,
		},
		{
			name: "unknown_to",
			machine: func(t *testing.T) *protocol.LifecycleMachine {
				return newLifecycleMachine(t, protocol.StateReadyToStart)
			},
			next: protocol.StateStatus("future_state"),
		},
		{
			name: "self_loop",
			machine: func(t *testing.T) *protocol.LifecycleMachine {
				return newLifecycleMachine(t, protocol.StateReadyToStart)
			},
			next: protocol.StateReadyToStart,
		},
		{
			name: "uninitialized_to_ready_to_start",
			machine: func(t *testing.T) *protocol.LifecycleMachine {
				return newLifecycleMachine(t, protocol.StateUninitialized)
			},
			next: protocol.StateReadyToStart,
		},
		{
			name: "ready_to_start_to_running",
			machine: func(t *testing.T) *protocol.LifecycleMachine {
				return newLifecycleMachine(t, protocol.StateReadyToStart)
			},
			next: protocol.StateRunning,
		},
		{
			name: "environment_broken_to_starting_backend",
			machine: func(t *testing.T) *protocol.LifecycleMachine {
				return newLifecycleMachine(t, protocol.StateEnvironmentBroken)
			},
			next: protocol.StateStartingBackend,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			machine := test.machine(t)
			before := machine.Current()
			initial := machine.Initial()
			restartUsed := machine.RestartUsed()
			err := machine.Transition(test.next)
			if err == nil {
				t.Fatalf("Transition(%q -> %q) error = nil, want error", before, test.next)
			}
			for _, detail := range []string{
				fmt.Sprintf("from=%q", before),
				fmt.Sprintf("to=%q", test.next),
			} {
				if !strings.Contains(err.Error(), detail) {
					t.Errorf("Transition error %q does not contain %q", err, detail)
				}
			}
			if got := machine.Current(); got != before {
				t.Errorf("Current() = %q after rejected transition, want unchanged %q", got, before)
			}
			if got := machine.Initial(); got != initial {
				t.Errorf("Initial() = %q after rejected transition, want unchanged %q", got, initial)
			}
			if got := machine.RestartUsed(); got != restartUsed {
				t.Errorf("RestartUsed() = %t after rejected transition, want unchanged %t", got, restartUsed)
			}
		})
	}
}

func TestLifecycleMachineRollbackPreparation(t *testing.T) {
	t.Parallel()

	stable := []protocol.StateStatus{
		protocol.StateUninitialized,
		protocol.StateEnvironmentBroken,
		protocol.StateReadyToStart,
	}
	for _, current := range []protocol.StateStatus{
		protocol.StatePreparingUV,
		protocol.StateSyncingRepository,
	} {
		current := current
		for _, initial := range stable {
			initial := initial
			t.Run(fmt.Sprintf("%s_from_%s", current, initial), func(t *testing.T) {
				t.Parallel()

				machine := newLifecycleMachine(t, initial)
				if err := machine.Transition(protocol.StatePreparingUV); err != nil {
					t.Fatal(err)
				}
				if current == protocol.StateSyncingRepository {
					if err := machine.Transition(protocol.StateSyncingRepository); err != nil {
						t.Fatal(err)
					}
				}

				if err := machine.RollbackPreparation(); err != nil {
					t.Fatalf("RollbackPreparation() error = %v", err)
				}
				if got := machine.Current(); got != initial {
					t.Fatalf("Current() = %q after rollback, want initial %q", got, initial)
				}
				if got := machine.Initial(); got != initial {
					t.Fatalf("Initial() = %q after rollback, want %q", got, initial)
				}
				if machine.RestartUsed() {
					t.Fatal("RestartUsed() = true after preparation rollback, want false")
				}
			})
		}
	}
}

func TestLifecycleMachineRollbackPreparationRejectsOtherStates(t *testing.T) {
	t.Parallel()

	for _, current := range []protocol.StateStatus{
		protocol.StateUninitialized,
		protocol.StatePreparingPython,
		protocol.StateSyncingEnvironment,
		protocol.StateReadyToStart,
		protocol.StateStartingBackend,
		protocol.StateRunning,
		protocol.StateRestarting,
		protocol.StateStoppingBackend,
		protocol.StateEnvironmentBroken,
		protocol.StateBackendFailed,
		protocol.StateStopped,
	} {
		current := current
		t.Run(string(current), func(t *testing.T) {
			t.Parallel()

			machine := newLifecycleMachineAt(t, current)
			before := machine.Current()
			initial := machine.Initial()
			restartUsed := machine.RestartUsed()

			if err := machine.RollbackPreparation(); err == nil {
				t.Fatalf("RollbackPreparation() from %q error = nil, want error", current)
			}
			if got := machine.Current(); got != before {
				t.Errorf("Current() = %q after rejected rollback, want unchanged %q", got, before)
			}
			if got := machine.Initial(); got != initial {
				t.Errorf("Initial() = %q after rejected rollback, want unchanged %q", got, initial)
			}
			if got := machine.RestartUsed(); got != restartUsed {
				t.Errorf("RestartUsed() = %t after rejected rollback, want unchanged %t", got, restartUsed)
			}
		})
	}
}

func TestLifecycleMachineRestart(t *testing.T) {
	t.Parallel()

	machine := newLifecycleMachineAt(t, protocol.StateRunning)
	if err := machine.Transition(protocol.StateRestarting); err != nil {
		t.Fatalf("first Transition(running -> restarting) error = %v", err)
	}
	if got := machine.Current(); got != protocol.StateRestarting {
		t.Fatalf("Current() = %q, want %q", got, protocol.StateRestarting)
	}
	if !machine.RestartUsed() {
		t.Fatal("RestartUsed() = false after entering restarting, want true")
	}
	if err := machine.Transition(protocol.StateRunning); err != nil {
		t.Fatalf("Transition(restarting -> running) error = %v", err)
	}

	before := machine.Current()
	initial := machine.Initial()
	restartUsed := machine.RestartUsed()
	err := machine.Transition(protocol.StateRestarting)
	if err == nil {
		t.Fatal("second Transition(running -> restarting) error = nil, want error")
	}
	for _, detail := range []string{
		fmt.Sprintf("from=%q", before),
		fmt.Sprintf("to=%q", protocol.StateRestarting),
	} {
		if !strings.Contains(err.Error(), detail) {
			t.Errorf("Transition error %q does not contain %q", err, detail)
		}
	}
	if got := machine.Current(); got != before {
		t.Errorf("Current() = %q after rejected second restart, want unchanged %q", got, before)
	}
	if got := machine.Initial(); got != initial {
		t.Errorf("Initial() = %q after rejected second restart, want unchanged %q", got, initial)
	}
	if got := machine.RestartUsed(); got != restartUsed {
		t.Errorf("RestartUsed() = %t after rejected second restart, want unchanged %t", got, restartUsed)
	}
}

func TestLifecycleMachineRunningFailureRequiresRestart(t *testing.T) {
	t.Parallel()

	if !protocol.IsKnownLifecycleTransition(protocol.StateRunning, protocol.StateBackendFailed) {
		t.Fatal("static transition running -> backend_failed is missing")
	}

	machine := newLifecycleMachineAt(t, protocol.StateRunning)
	before := machine.Current()
	initial := machine.Initial()
	restartUsed := machine.RestartUsed()
	err := machine.Transition(protocol.StateBackendFailed)
	if err == nil {
		t.Fatal("Transition(running -> backend_failed) before restart error = nil, want error")
	}
	for _, detail := range []string{
		fmt.Sprintf("from=%q", before),
		fmt.Sprintf("to=%q", protocol.StateBackendFailed),
	} {
		if !strings.Contains(err.Error(), detail) {
			t.Errorf("Transition error %q does not contain %q", err, detail)
		}
	}
	if got := machine.Current(); got != before {
		t.Errorf("Current() = %q after premature backend failure, want unchanged %q", got, before)
	}
	if got := machine.Initial(); got != initial {
		t.Errorf("Initial() = %q after premature backend failure, want unchanged %q", got, initial)
	}
	if got := machine.RestartUsed(); got != restartUsed {
		t.Errorf("RestartUsed() = %t after premature backend failure, want unchanged %t", got, restartUsed)
	}

	for _, next := range []protocol.StateStatus{
		protocol.StateRestarting,
		protocol.StateRunning,
		protocol.StateBackendFailed,
	} {
		if err := machine.Transition(next); err != nil {
			t.Fatalf("Transition(%q -> %q) after restart error = %v", machine.Current(), next, err)
		}
	}
	if got := machine.Current(); got != protocol.StateBackendFailed {
		t.Errorf("Current() = %q after post-restart failure, want %q", got, protocol.StateBackendFailed)
	}
}

func TestLifecycleMachineTerminalStates(t *testing.T) {
	t.Parallel()

	for _, terminal := range []protocol.StateStatus{
		protocol.StateBackendFailed,
		protocol.StateStopped,
	} {
		terminal := terminal
		t.Run(string(terminal), func(t *testing.T) {
			t.Parallel()

			machine := newLifecycleMachineAt(t, terminal)
			initial := machine.Initial()
			restartUsed := machine.RestartUsed()
			for _, next := range append(protocol.AllStateStatuses(), protocol.StateStatus("future_state")) {
				if err := machine.Transition(next); err == nil {
					t.Errorf("Transition(%q -> %q) error = nil, want terminal-state error", terminal, next)
				}
				if got := machine.Current(); got != terminal {
					t.Fatalf("Current() = %q after rejected terminal transition, want unchanged %q", got, terminal)
				}
			}
			if err := machine.RollbackPreparation(); err == nil {
				t.Errorf("RollbackPreparation() from terminal %q error = nil, want error", terminal)
			}
			if got := machine.Current(); got != terminal {
				t.Errorf("Current() = %q after rejected terminal rollback, want unchanged %q", got, terminal)
			}
			if got := machine.Initial(); got != initial {
				t.Errorf("Initial() = %q after terminal operations, want unchanged %q", got, initial)
			}
			if got := machine.RestartUsed(); got != restartUsed {
				t.Errorf("RestartUsed() = %t after terminal operations, want unchanged %t", got, restartUsed)
			}
		})
	}
}

// 这些并发读写只为 race detector 覆盖同一 LifecycleMachine 的锁域，
// 不把 goroutine 的实际调度顺序当作确定性交错证明。
func TestLifecycleMachineConcurrentReaders(t *testing.T) {
	machine := newLifecycleMachine(t, protocol.StateReadyToStart)
	start := make(chan struct{})
	errs := make(chan error, 33)
	var workers sync.WaitGroup

	workers.Add(1)
	go func() {
		defer workers.Done()
		<-start
		for _, next := range []protocol.StateStatus{
			protocol.StateStartingBackend,
			protocol.StateRunning,
			protocol.StateRestarting,
			protocol.StateRunning,
			protocol.StateBackendFailed,
		} {
			if err := machine.Transition(next); err != nil {
				errs <- fmt.Errorf("writer Transition(%q): %w", next, err)
				return
			}
		}
	}()

	const readers = 32
	for range readers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for range 1000 {
				if got := machine.Current(); !protocol.IsKnownStateStatus(got) {
					errs <- fmt.Errorf("Current() = unknown state %q", got)
					return
				}
				if got := machine.Initial(); got != protocol.StateReadyToStart {
					errs <- fmt.Errorf("Initial() = %q, want %q", got, protocol.StateReadyToStart)
					return
				}
				_ = machine.RestartUsed()
			}
		}()
	}

	close(start)
	workers.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// 并发 restart 尝试覆盖 restartUsed 与 current 的联合锁域；精确调度仍由锁域审查保证。
func TestLifecycleMachineConcurrentRestartGuard(t *testing.T) {
	machine := newLifecycleMachineAt(t, protocol.StateRunning)
	start := make(chan struct{})
	results := make(chan error, 32)
	var workers sync.WaitGroup

	const attempts = 32
	for range attempts {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			results <- machine.Transition(protocol.StateRestarting)
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Errorf("concurrent first restart successes = %d, want exactly 1", successes)
	}
	if got := machine.Current(); got != protocol.StateRestarting {
		t.Errorf("Current() = %q after concurrent restart attempts, want %q", got, protocol.StateRestarting)
	}
	if !machine.RestartUsed() {
		t.Error("RestartUsed() = false after successful concurrent restart, want true")
	}
}

func newLifecycleMachine(t *testing.T, initial protocol.StateStatus) *protocol.LifecycleMachine {
	t.Helper()

	machine, err := protocol.NewLifecycleMachine(initial)
	if err != nil {
		t.Fatalf("NewLifecycleMachine(%q) error = %v", initial, err)
	}
	return machine
}

func newLifecycleMachineAt(t *testing.T, target protocol.StateStatus) *protocol.LifecycleMachine {
	t.Helper()

	initial, path, ok := pathToLifecycleState(target, protocol.AllLifecycleTransitions())
	if !ok {
		t.Fatalf("test specification has no path to %q", target)
	}
	machine := newLifecycleMachine(t, initial)
	for _, next := range path {
		if err := machine.Transition(next); err != nil {
			t.Fatalf("Transition(%q -> %q) while constructing test machine error = %v", machine.Current(), next, err)
		}
	}
	return machine
}

func pathToLifecycleState(
	target protocol.StateStatus,
	transitions []protocol.LifecycleTransition,
) (protocol.StateStatus, []protocol.StateStatus, bool) {
	stable := []protocol.StateStatus{
		protocol.StateUninitialized,
		protocol.StateEnvironmentBroken,
		protocol.StateReadyToStart,
	}
	type route struct {
		initial protocol.StateStatus
		current protocol.StateStatus
		path    []protocol.StateStatus
	}
	queue := make([]route, 0, len(stable))
	visited := make(map[protocol.StateStatus]bool)
	for _, initial := range stable {
		queue = append(queue, route{initial: initial, current: initial})
		visited[initial] = true
	}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current.current == target {
			return current.initial, current.path, true
		}
		for _, transition := range transitions {
			if transition.From != current.current || visited[transition.To] {
				continue
			}
			visited[transition.To] = true
			path := append(append([]protocol.StateStatus(nil), current.path...), transition.To)
			queue = append(queue, route{
				initial: current.initial,
				current: transition.To,
				path:    path,
			})
		}
	}

	return "", nil, false
}
