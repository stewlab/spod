# spod

An Apache-2.0, daemonless OCI container engine and embeddable Go library.
Images come directly from registries through `go-containerregistry`; Linux
containers run through `runc/libcontainer`. No Docker, Podman, containerd,
root service, or separately installed runtime binary is required.

## Build

```sh
GOWORK=off CGO_ENABLED=1 go build -trimpath -tags 'netgo osusergo' \
  -ldflags '-linkmode external -extldflags "-static"' -o bin/spod ./cmd/spod
GOWORK=off go test ./...
```

`GOWORK=off` also makes these commands independent of any enclosing development
workspace. The Linux binary is statically linked and needs a C compiler and
static libc development files at build time, not at runtime. libcontainer's
namespace constructor runs before Go starts; it requires CGO for correct
namespace initialization and exec. Image operations are portable;
container execution requires Linux with unprivileged user namespaces enabled.
Non-Linux and Linux `CGO_ENABLED=0` builds return an explicit unsupported-runtime
error for execution. Cross-building the full Linux runtime requires an
appropriate target C compiler and static libc.

The included `Containerfile` builds the same static binary inside a container
and ships it in a minimal Alpine image:

```sh
podman build -t spod .
podman run --rm --userns=host --privileged spod run --rm alpine:3.22 id
```

Running containers from inside that image additionally requires the outer
engine to permit unprivileged user namespaces, as shown above.

## Commands

```sh
spod pull docker.io/library/alpine:3.22
spod run --rm -it -v "$PWD:/workspace" -w /workspace alpine:3.22 /bin/sh
spod run -d --name demo alpine:3.22 sleep infinity
spod exec -it demo /bin/sh
spod inspect --format '{{.Status}}|{{.Image}}' demo
spod ps
spod stop demo
spod start demo
spod logs --tail 20 demo
spod rm demo
spod images
spod rmi alpine:3.22
spod export demo -o demo-rootfs.tar
spod import probe/base:latest demo-rootfs.tar
```

`export NAME` writes a container's root filesystem as a plain tar stream,
including changes made while the container ran; `-o` selects the destination
file, otherwise the archive goes to stdout. `import REFERENCE [FILE]` turns an
archive into a stored image that `run` and other containers can use as their
base image, which makes captured filesystems reproducible starting points.
The archive becomes the image's single layer with a default `PATH`; re-importing
a reference replaces its stored image, and a failed import leaves any existing
image untouched.

`attach NAME` streams a detached container's live output and, when created with
`run -di`, forwards stdin to its initial process. Detached TTY allocation is
not supported; use `exec -it` for an interactive terminal. A detached container
has its own short-lived monitor process to reap its initial process, provide
attach streams, and collect logs. There is no shared or persistent service.

Default storage is `$XDG_DATA_HOME/spod/storage`, falling back to
`~/.local/share/spod/storage`. Override it with `SPOD_ROOT` or `--root`; an
override must be an absolute path, so a store is never created relative to the
working directory by accident.
Image manifests/configuration, cached root filesystems, writable container
filesystems, runtime state, named volumes, and logs live in separate directories.
Removed resources move into the store's `trash/` directory, rather than being
permanently deleted; this retains disk usage until explicitly cleaned up.

## Embedding

Import `github.com/stewlab/spod/pkg/engine` for image and lifecycle operations,
or add `cli.NewCommand()` from `github.com/stewlab/spod/pkg/cli` to a Cobra CLI.
**Every executable embedding the runtime must call `engine.Init()` as its first
operation in `main`**, before argument parsing or application initialization.
This handles libcontainer's self-executing initialization path.

```go
func main() {
    engine.Init()
    // Initialize the embedding application, then run its commands.
}
```

## Boundaries

- Host networking only; isolated network modes are rejected, not silently
  mapped to host networking.
- No Containerfile build, overlayfs, privileged mode, cgroup resource limits,
  SELinux relabeling, or NVIDIA CDI resolution. Use another runtime when these
  are required. `:z`/`:Z` mount suffixes are accepted without relabeling.
- Bind mounts can expose host files, devices, and Wayland/audio sockets.
  A container given these resources can access them with the invoking user's
  privileges. This is not a virtual machine or a sandbox for untrusted code.
- Image ownership is normalized to the invoking user for rootless extraction.
  Single-user mapping supports container UID/GID 0; arbitrary image ownership
  and non-root image users may require subordinate mappings.
- Image removal refuses references held by containers; volume removal refuses
  volumes held by containers.

The original implementation does not incorporate Lilipod source. Upstream
dependencies retain their own licenses and notices.
