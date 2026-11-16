#!/usr/bin/env bash
set -euo pipefail
export GOTOOLCHAIN=go1.26.11
export GOCACHE="${TMPDIR:-/tmp}/auto-mas-runtime-verify"
export AUTO_MAS_TELEMETRY=disabled
go_files=()
while IFS= read -r -d '' go_file; do
  go_files+=("$go_file")
done < <(git ls-files -z -- '*.go')
if ((${#go_files[@]} == 0)); then
  echo "no Go files found" >&2
  exit 1
fi
gofmt -w "${go_files[@]}"
git diff --exit-code -- '*.go'
go version
go vet ./...
go build -buildvcs=false ./...
smoke_binary="$(mktemp "${TMPDIR:-/tmp}/auto-mas-smoke.XXXXXX")"
trap 'rm -f "$smoke_binary"' EXIT
go build -buildvcs=false -o "$smoke_binary" ./cmd/auto-mas-runtime
go run ./scripts/release-smoke "$smoke_binary" dev
if [[ "$(go env GOOS)" == "darwin" ]]; then
  go test ./internal/config ./internal/protocol ./internal/protocol/contracttest ./internal/release ./internal/version -count=1
  go test ./internal/lock -run '^TestDarwinLock_' -count=1
  go test -race ./internal/lock -run '^TestDarwinLock_' -count=1
else
  go test ./... -count=1
  if [[ -n "${AUTO_MAS_UV_INTEGRATION_ARCHIVE:-}" ]]; then
    go test -tags=integration ./internal/uv -run '^TestIntegration_OfficialUVArtifact$' -v -count=1
  fi
  go test -race ./... -count=1
  go test ./internal/protocol ./internal/lock ./internal/state ./internal/process -count=100
fi
git diff --check
