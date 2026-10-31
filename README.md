# spod

A daemonless, rootless OCI container engine and embeddable Go library,
Apache-2.0 licensed. Images pull straight from registries via
`go-containerregistry`; Linux containers run through `runc/libcontainer`.
No daemon, no root service, no separately installed runtime.

## Quick Start

Install the latest release binary and run a container — no toolchain
needed on the machine (the release page carries one file per
architecture):

```sh
curl -fsSL https://github.com/stewlab/spod/releases/latest/download/spod-linux-amd64.tar.gz | tar -xz -C /tmp
install -m 755 /tmp/spod ~/.local/bin/spod
spod --version

spod pull docker.io/library/alpine:3.22
spod run --rm -it alpine:3.22 /bin/sh
```

Containers run on Linux — the engine needs the kernel (user namespaces,
runc/libcontainer). The darwin, windows, and freebsd builds manage images
and the store (pull, import, export, prune) and report an error for the
container lifecycle. Archives are `spod-<os>-<arch>.tar.gz` (`.zip` for
windows) plus a `checksums.txt`; curl-installed binaries are not
quarantined by macOS Gatekeeper. Building from source, including
cross-compiling, is below.

## Building from Source

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

The binary is fully static (CGO against musl), `-trimpath`, stripped
with `-s -w`. `--target binary` exports just the binary into `bin/`; the
default build finishes at the minimal Alpine image packaging the engine
for distribution. `--target test` runs the suite in-container; a failing
test fails the build.

Once one spod binary exists, it can build the next one inside its own
container:

```sh
spod run --rm -v "$PWD:/src" -w /src golang:1.25-alpine \
  sh -c 'apk add --no-cache git gcc musl-dev linux-headers &&
         GOWORK=off CGO_ENABLED=1 go build -trimpath -tags netgo,osusergo \
           -ldflags "-s -w -linkmode external -extldflags -static" -o bin/spod ./cmd/spod'
install -m 755 bin/spod ~/.local/bin/spod
```

`spod --version` (one line) and `spod version` (full build table) name the build: the tag when built from one (a
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

### Cross-compiling

The Linux targets cross-compile in the container: `GOARCH` selects amd64
(default), arm64, riscv64, 386, arm — zig cc carries musl, no per-target
toolchain, no emulation; Alpine's package pin holds the Zig version.

```sh
podman build --build-arg GOARCH=arm64 --target binary --output type=local,dest=bin .
podman build --build-arg GOARCH=riscv64 --target binary --output type=local,dest=bin .
```

The darwin, windows, and freebsd targets build with plain Go and CGO off
— no Apple SDK needed: image pulls, store management, and import/export
work, while the container lifecycle reports `ErrUnsupported`. The
release workflow ships all of them.

### Testing the full lifecycle

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
spod run --rm --userns keep-id -u "$(id -u):$(id -g)" -v "$PWD:/w" alpine:3.22 touch /w/f
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
- `run --userns keep-id` maps your UID/GID to the same IDs inside the
  container, like podman's keep-id: files you create in bind mounts as that
  user are yours on the host, while container root and every other ID map
  to your subordinate range, so an entrypoint can start as root, add a user
  with your UID, and drop to it. It needs `newuidmap`/`newgidmap` and a
  `/etc/subuid` and `/etc/subgid` range larger than your UID; the rootfs is
  re-owned once at creation.
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
bootstrap containers. `cli.StyleHelp(cmd)` applies the shared help styling
to any command tree.

```go
func main() {
    engine.Init()
    // ...application setup and commands
}
```

## Contributing & Releases

Development happens on short-lived feature branches off `main`, merged
through pull requests gated by CI. The conventions live in
CONTRIBUTING.md; the maintainer release runbook — how tags become
releases and how versions are stamped — is RELEASING.md.

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
  Under `--userns keep-id`, container root is a subordinate ID and cannot
  write host files owned by you; run as your UID for that.
- Images and volumes held by containers refuse removal.
- The container lifecycle requires Linux with CGO; on other platforms —
  and on Linux builds without CGO — image and store operations work and
  lifecycle methods report `ErrUnsupported`.
