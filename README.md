# spod

A daemonless, rootless OCI container engine and embeddable Go library,
Apache-2.0 licensed. Images pull straight from registries via
`go-containerregistry`; Linux containers run through `runc/libcontainer`.
No daemon, no root service, no separately installed runtime.

## Build

```sh
./build.sh build      # compiles bin/spod
./build.sh test       # offline unit tests
./build.sh test-real  # lifecycle test against alpine:3.22 (needs network)
./build.sh install    # installs to ~/.local/bin/spod
```

`build.sh` wraps the go commands with the two settings every spod build
needs, so you do not have to remember them:

- `GOWORK=off` builds this checkout against its own `go.mod`. An enclosing
  `go.work` (a monorepo superproject, for example) would otherwise redirect
  the build to a different spod checkout.
- `CGO_ENABLED=1` links libcontainer's cgo constructor, which sets up the
  Linux user namespace before the Go runtime starts. Without CGO the binary
  still compiles everywhere and keeps image operations (pull, images,
  prune, inspect); container commands return an explicit error instead of
  failing obscurely.

The Linux build is fully static, so the binary runs on any Linux without
library dependencies (`./build.sh build --dynamic` links the host libc
instead). It needs a C compiler and a static libc at build time, and
unprivileged user namespaces at runtime.

Equivalent manual build:

```sh
GOWORK=off CGO_ENABLED=1 go build -trimpath -tags 'netgo osusergo' \
  -ldflags '-linkmode external -extldflags "-static"' -o bin/spod ./cmd/spod
```

Once built, run it directly — no other container tooling involved:

```sh
bin/spod run --rm -it -v "$PWD:/workspace" -w /workspace alpine:3.22 /bin/sh
```

`Containerfile` packages that same static binary into a minimal Alpine
image for distribution, for example as a CI job image:

```sh
podman build -t spod .
```

The image carries the engine at `/usr/local/bin/spod` and runs as an
unprivileged user. It is a packaging vehicle, not the way to try spod:
running containers from inside a container needs user namespace support
from the outer engine, so on a host the built binary is all you need.
Real-container tests are opt-in (`SPOD_REAL_LIFECYCLE_TEST=1` when using
plain `go test`); plain tests stay offline.

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
  mapped to host.
- No image builds, overlayfs, privileged mode, cgroup limits, SELinux
  relabeling, or NVIDIA CDI. `:z`/`:Z` mount suffixes are accepted without
  relabeling.
- Bind mounts hand containers host files, devices, and sockets with the
  invoking user's privileges; this is not a sandbox for untrusted code.
- Image ownership is normalized to the invoking user. Single-UID mapping
  covers container UID/GID 0; other image users need subordinate mappings.
- Images and volumes held by containers refuse removal.
