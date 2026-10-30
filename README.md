# spod

A daemonless, rootless OCI container engine and embeddable Go library,
Apache-2.0 licensed. Images pull straight from registries via
`go-containerregistry`; Linux containers run through `runc/libcontainer`.
No daemon, no root service, no separately installed runtime.

## Build & Install

Build in a container — no Go toolchain or C compiler needed on the host:

```sh
podman build --target binary --output type=local,dest=bin .   # emits bin/spod
podman build --tag spod .                                     # builds the distribution image
podman build --target test .                                  # runs the test suite
```

The first stage compiles the production binary: fully static (CGO
against musl), `-trimpath`, and stripped with `-s -w`. The
`--target binary` run exports just that binary into `bin/`; the second
build finishes at the minimal Alpine image that packages the engine for
distribution, for example as a CI job image. The test target runs the
same suite inside the container; a failing test fails the build.

Once one spod binary exists, it can build the next one inside its own
container:

```sh
spod run --rm -v "$PWD:/src" -w /src golang:1.25-alpine \
  sh -c 'apk add --no-cache git gcc musl-dev linux-headers &&
         GOWORK=off CGO_ENABLED=1 go build -trimpath -tags netgo,osusergo \
           -ldflags "-s -w -linkmode external -extldflags -static" -o bin/spod ./cmd/spod'
```

Build on the host instead if you have Go 1.25+, a C compiler, and a
static libc:

```sh
GOWORK=off CGO_ENABLED=1 go build -trimpath -tags 'netgo osusergo' \
  -ldflags '-s -w -linkmode external -extldflags "-static"' -o bin/spod ./cmd/spod
GOWORK=off go test ./...
```

The two environment settings are requirements, not preferences:

- `GOWORK=off` builds this checkout against its own `go.mod`. An enclosing
  `go.work` (a monorepo superproject, for example) would otherwise redirect
  the build to a different spod checkout.
- `CGO_ENABLED=1` links libcontainer's cgo constructor, which sets up the
  Linux user namespace before the Go runtime starts. Without CGO the binary
  still compiles everywhere and keeps image operations (pull, images,
  prune, inspect); container commands return an explicit error instead of
  failing obscurely.

Install the binary to `~/.local/bin` (ensure it is on your `PATH`):

```sh
mkdir -p ~/.local/bin
cp bin/spod ~/.local/bin/spod
spod --version
```

Then run it directly — no other container tooling involved:

```sh
spod run --rm -it -v "$PWD:/workspace" -w /workspace alpine:3.22 /bin/sh
```

Tests that run real containers are opt-in; this exercises the full
lifecycle against `docker.io/library/alpine:3.22`, while plain tests stay
offline:

```sh
SPOD_REAL_LIFECYCLE_TEST=1 GOWORK=off go test ./pkg/engine/ -run TestRealLifecycleAlpine
```

## Commands

```sh
spod pull docker.io/library/alpine:3.22
spod run --rm -it -v "$PWD:/workspace" -w /workspace alpine:3.22 /bin/sh
spod run -d --name demo alpine:3.22 sleep infinity
spod exec -it demo /bin/sh
spod attach demo
spod inspect --format '{{.Status}}|{{.Image}}' demo
spod ps
spod stop demo && spod start demo
spod logs --tail 20 demo
spod rm demo
spod images
spod rmi alpine:3.22
spod prune            # or: spod prune --force
spod export demo -o demo-rootfs.tar
spod import probe/base:latest demo-rootfs.tar
```

- `export` snapshots a container's root filesystem, changes included;
  `import` turns an archive into a stored image, making captured filesystems
  reproducible starting points.
- `prune` removes stored images no container uses. It lists what would go
  and asks first (default No); scripts pass `--force`. Images still in use
  are never offered, and removals land in the store's trash either way.
- `attach` streams a detached container's live output and, when it was
  created with `run -di`, forwards stdin. Detached TTY is not supported;
  use `exec -it` for a terminal.
- Storage follows the platform data directory (`~/.local/share/spod/storage`
  on Linux). Override with `SPOD_ROOT` or `--root`; overrides must be
  absolute. Removed resources move to the store's `trash/` directory rather
  than being deleted outright.

## Embedding

`pkg/engine` exposes image and lifecycle operations; `pkg/cli` provides a
Cobra command tree. **Every embedding executable must call `engine.Init()`
as its first operation in `main`** — libcontainer re-executes the binary to
bootstrap containers.

```go
func main() {
    engine.Init()
    // ...application setup and commands
}
```

## Boundaries

- Host networking only; isolated network modes are rejected, not silently
  mapped to host. The host's `/etc/resolv.conf` and `/etc/hosts` are bind-
  mounted read-only so name resolution works; a container mounting either
  path itself overrides that.
- No image builds, overlayfs, privileged mode, cgroup limits, SELinux
  relabeling, or NVIDIA CDI. `:z`/`:Z` mount suffixes are accepted without
  relabeling.
- Bind mounts hand containers host files, devices, and sockets with the
  invoking user's privileges; this is not a sandbox for untrusted code.
- Image ownership is normalized to the invoking user. Single-UID mapping
  covers container UID/GID 0; other image users need subordinate mappings.
- Images and volumes held by containers refuse removal.
