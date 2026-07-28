# AUTO-MAS Runtime

AUTO-MAS Runtime is the Windows runtime manager for AUTO-MAS. It will provide a
single `auto-mas-runtime.exe` that prepares the managed Go/Python environment,
synchronizes the backend workspace, and supervises the backend process.

The project is under active implementation. The architecture and task list are
the source of truth:

- [Architecture](doc/架构设计.md)
- [Implementation tasks](doc/任务拆分.md)

Contributors and AI agents must read [AGENTS.md](AGENTS.md) before making
changes. It covers the toolchain, workflow, coding standards, and hard
constraints of this repository.

## Responsibilities

Runtime owns local environment setup, managed backend repository updates,
dependency synchronization, diagnostics, repair, cleanup, and the backend
process lifecycle. It does not proxy AUTO-MAS business HTTP or WebSocket
traffic, manage Python plugins, or update its own executable.

## Prerequisites

- Windows 10 or Windows 11
- [PowerShell 7](https://learn.microsoft.com/powershell/) (`pwsh`)
- Go 1.26 or newer
- golangci-lint 2.12.2 or newer

The commands below use PowerShell 7 syntax.

Install the pinned lint tool when it is not already available:

```powershell
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
```

## Build and run

```powershell
go build ./...
go run ./cmd/auto-mas-runtime
```

The current scaffold prints a development version placeholder:

```text
auto-mas-runtime dev
```

## Test and lint

```powershell
go test ./...
go vet ./...
golangci-lint run
```

Check formatting without changing files:

```powershell
$goFiles = Get-ChildItem -LiteralPath . -Recurse -Filter "*.go" -File
gofmt -d $goFiles.FullName
```

Format the project:

```powershell
$goFiles = Get-ChildItem -LiteralPath . -Recurse -Filter "*.go" -File
gofmt -w $goFiles.FullName
```

## Repository layout

```text
cmd/auto-mas-runtime/  Executable entry point
internal/cli/          CLI parsing and application dispatch
internal/protocol/     NDJSON protocol contracts
internal/config/       Configuration and managed directory layout
internal/gitrepo/      Managed backend repository synchronization
internal/uv/           uv, Python, and project environment management
internal/backend/      Backend lifecycle orchestration
internal/process/      Windows process and Job Object integration
internal/health/       Backend health and identity checks
internal/state/        Persistent runtime operation state
internal/lock/         Cross-process coordination
internal/mirror/       Download source selection and rotation
internal/filesystem/   Guarded filesystem operations
internal/logging/      Runtime diagnostics and operation logs
testdata/              Shared component and end-to-end fixtures
```
