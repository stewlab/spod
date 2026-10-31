# spod

A daemonless, rootless OCI container engine and embeddable Go library,
Apache-2.0 licensed. Images pull straight from registries via
`go-containerregistry`; Linux containers run through `runc/libcontainer`.
No daemon, no root service, no separately installed runtime.

## Build & Install

Build in a container — no Go toolchain or C compiler needed on the host:

```sh
podman build --target binary --output type=local,dest=bin .   # emits bin/spod
mkdir -p ~/.local/bin && install -m 755 bin/spod ~/.local/bin/spod
spod --version

podman build --tag spod .                                     # builds the distribution image
podman build --target test .                                  # runs the test suite
```

`install` (rather than `cp`) replaces the file by rename, so the install
succeeds even while a previous spod process is still running from the old
binary — `cp` fails with "Text file busy" in that situation.

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
install -m 755 bin/spod ~/.local/bin/spod
```

`spod --version` names its build: the tag when built from one (a
`go install github.com/stewlab/spod/cmd/spod@v0.1.0` binary included),
`dev-<commit>` for checkout builds — `-dirty` when the checkout carries
uncommitted changes — and `dev` when no git metadata is available.

Build on the host instead if you have Go 1.25+, a C compiler, and a
static libc:

```sh
GOWORK=off CGO_ENABLED=1 go build -trimpath -tags 'netgo osusergo' \
  -ldflags '-s -w -linkmode external -extldflags "-static"' -o bin/spod ./cmd/spod
GOWORK=off go test ./...
install -m 755 bin/spod ~/.local/bin/spod
```

### Cross-compiling in the container

The build stage compiles with `zig cc` — one C compiler carrying musl for
every target — so a foreign binary needs no per-target toolchain, no
sysroot, and no emulation: the container runs on the builder platform
while `GOARCH` selects the binary's target.

```sh
podman build --build-arg GOARCH=arm64 --target binary --output type=local,dest=bin .
podman build --build-arg GOARCH=riscv64 --target binary --output type=local,dest=bin .
```

Supported values: amd64 (default), arm64, riscv64, 386, arm. The
host-amd64 Zig build was verified against the real-lifecycle test suite
and the arm64/riscv64 artifacts link statically; Alpine's package pin
holds the Zig version for the container build. macOS targets
additionally need the Apple SDK, which Zig does not bundle.

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
spod rename demo demo2
spod images
spod rmi alpine:3.22
spod volume ls && spod volume rm demo-home
spod prune            # or: spod prune --force
spod export demo -o demo-rootfs.tar
spod import probe/base:latest demo-rootfs.tar
spod import-oci alpine:3.22 alpine-saved.tar
```

- `export` snapshots a container's root filesystem, changes included;
  `import` turns an archive into a stored image, making captured filesystems
  reproducible starting points.
- `import-oci` stores a registry image from an archive — podman or docker
  `save` output, either OCI layout or docker-save format — which is how
  images land in the store from another engine's storage without a
  download.
- `rename` changes a container's name in place, mount definitions
  included; `volume ls` / `volume rm` manage named volumes.
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
