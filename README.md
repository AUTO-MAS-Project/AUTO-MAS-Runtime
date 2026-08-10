# AUTO-MAS Runtime

AUTO-MAS Runtime is the Windows runtime manager for AUTO-MAS. It will provide a
single `auto-mas-runtime.exe` that prepares the managed Go/Python environment,
synchronizes the backend workspace, and supervises the backend process.

The project is under active implementation. Start from the documentation index;
the architecture and task list remain the source of truth:

- [Documentation index](doc/README.md)
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
- MSYS2 UCRT64 GCC/G++ (for CGO and the race detector)
- golangci-lint 2.12.2 or newer

The commands below use PowerShell 7 syntax.

Install the pinned lint tool when it is not already available:

```powershell
& go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
if ($LASTEXITCODE -ne 0) { throw "golangci-lint installation failed" }
```

## Build and run

```powershell
& go build ./...
if ($LASTEXITCODE -ne 0) { throw "build failed" }
& go run ./cmd/auto-mas-runtime
if ($LASTEXITCODE -ne 0) { throw "run failed" }
```

Local builds without release linker flags keep the stable development placeholders:

```text
auto-mas-runtime dev
```

Build a release-style Windows x64 executable with PowerShell 7:

```powershell
$releaseTag = "v0.1.0-beta.1"
$releaseCommit = (& git rev-parse HEAD).Trim()
if ($LASTEXITCODE -ne 0) { throw "unable to resolve release commit" }
$releaseDate = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
$releaseLdflags = "-s -w -X github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/version.Version=$releaseTag -X github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/version.Commit=$releaseCommit -X github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/version.BuildDate=$releaseDate"
$env:GOOS = "windows"
$env:GOARCH = "amd64"
$releaseDist = Join-Path (Get-Location) "dist"
New-Item -ItemType Directory -Path $releaseDist -Force -ErrorAction Stop | Out-Null
$releaseExecutable = Join-Path $releaseDist "auto-mas-runtime.exe"
& go build -trimpath -buildvcs=false -ldflags $releaseLdflags -o $releaseExecutable ./cmd/auto-mas-runtime
if ($LASTEXITCODE -ne 0) { throw "release-style build failed" }
```

The `version --output ndjson` result details report the injected tag, commit, and build date;
the protocol `hello.runtimeVersion` reports the same injected tag. The project is distributed under
AGPL-3.0-or-later; see [LICENSE](LICENSE).

## Test and lint

```powershell
& go test ./...
if ($LASTEXITCODE -ne 0) { throw "tests failed" }
& go vet ./...
if ($LASTEXITCODE -ne 0) { throw "go vet failed" }
& golangci-lint run
if ($LASTEXITCODE -ne 0) { throw "golangci-lint failed" }
```

Before running the race detector, move the directory containing `gcc.exe` to
the front of the current session's `PATH`. This prevents similarly named
MinGW DLLs from other software directories from loading first:

```powershell
$gccBin = Split-Path -Parent (Get-Command gcc -ErrorAction Stop).Source
$env:PATH = "$gccBin;$env:PATH"
& go test -race ./... -count=1
if ($LASTEXITCODE -ne 0) { throw "race tests failed" }
```

Check formatting without changing files:

```powershell
$goFiles = Get-ChildItem -LiteralPath . -Recurse -Filter "*.go" -File
& gofmt -d $goFiles.FullName
if ($LASTEXITCODE -ne 0) { throw "gofmt check failed" }
```

Format the project:

```powershell
$goFiles = Get-ChildItem -LiteralPath . -Recurse -Filter "*.go" -File
& gofmt -w $goFiles.FullName
if ($LASTEXITCODE -ne 0) { throw "gofmt failed" }
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
