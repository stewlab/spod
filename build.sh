#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

usage() {
  cat <<'EOF'
Usage: build.sh [command] [options]

Commands:
  build      Compile the spod binary to bin/spod (default)
  test       Run the unit tests (offline)
  test-real  Run the gated lifecycle test against alpine:3.22 (network)
  install    Build, then install to ~/.local/bin/spod
  clean      Remove build artifacts

Options:
  --dynamic  Link against the host libc instead of the default fully
             static binary (useful where no static libc is installed)

Every build runs with GOWORK=off, so an enclosing go.work cannot redirect
the build to a different checkout. On Linux the build keeps CGO enabled:
container execution goes through libcontainer's cgo constructor. A
CGO_ENABLED=0 or non-Linux build compiles everywhere and keeps image
operations; container commands return an explicit error.
EOF
}

CMD=build
STATIC=1
while [[ $# -gt 0 ]]; do
  case "$1" in
    build|test|test-real|install|clean) CMD=$1 ;;
    --dynamic) STATIC=0 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "[spod] Unknown argument: $1" >&2; usage >&2; exit 1 ;;
  esac
  shift
done

# GOWORK=off builds this checkout against its own go.mod. CGO_ENABLED=1 is
# what links libcontainer's namespace constructor; without it the binary
# compiles but container commands refuse with an explicit error.
if [[ "$(go env GOOS)" == "linux" ]]; then
  GO_ENV=(GOWORK=off CGO_ENABLED=1)
else
  GO_ENV=(GOWORK=off CGO_ENABLED=0)
fi

run_build() {
  echo "[spod] Compiling spod binary..."
  mkdir -p bin
  if [[ "$(go env GOOS)" != "linux" ]]; then
    echo "[spod] Non-Linux host: image operations only; container commands return an explicit error"
    env "${GO_ENV[@]}" go build -trimpath -o bin/spod ./cmd/spod
  elif [[ $STATIC == 1 ]]; then
    env "${GO_ENV[@]}" go build -trimpath -tags "netgo osusergo" \
      -ldflags '-linkmode external -extldflags "-static"' \
      -o bin/spod ./cmd/spod
  else
    env "${GO_ENV[@]}" go build -trimpath -tags "netgo osusergo" -o bin/spod ./cmd/spod
  fi
  echo "[spod] Built binary successfully at: $SCRIPT_DIR/bin/spod"
}

run_test() {
  echo "[spod] Running unit tests..."
  env "${GO_ENV[@]}" go test ./...
}

run_test_real() {
  echo "[spod] Running the gated lifecycle test against docker.io/library/alpine:3.22..."
  env "${GO_ENV[@]}" SPOD_REAL_LIFECYCLE_TEST=1 \
    go test -v ./pkg/engine/ -run TestRealLifecycleAlpine -count=1
}

run_install() {
  run_build
  echo "[spod] Installing spod to $HOME/.local/bin/spod..."
  mkdir -p "$HOME/.local/bin" "$HOME/.local/share/licenses/spod"
  cp bin/spod "$HOME/.local/bin/spod.tmp" && mv -f "$HOME/.local/bin/spod.tmp" "$HOME/.local/bin/spod"
  cp LICENSE NOTICE "$HOME/.local/share/licenses/spod/"
  echo "[spod] Installed successfully to $HOME/.local/bin/spod"
}

run_clean() {
  echo "[spod] Cleaning build artifacts..."
  if command -v trash >/dev/null 2>&1; then
    trash bin/spod 2>/dev/null || rm -f bin/spod
  else
    rm -f bin/spod
  fi
  go clean
}

case "$CMD" in
  build) run_build ;;
  test) run_test ;;
  test-real) run_test_real ;;
  install) run_install ;;
  clean) run_clean ;;
esac
