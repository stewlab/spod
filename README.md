# spod

A daemonless, rootless OCI container engine and embeddable Go library,
Apache-2.0 licensed. Images pull straight from registries via
`go-containerregistry`; Linux containers run through `runc/libcontainer`.
No daemon, no root service, no separately installed runtime.

## Build

```sh
GOWORK=off CGO_ENABLED=1 go build -trimpath -tags 'netgo osusergo' \
  -ldflags '-linkmode external -extldflags "-static"' -o bin/spod ./cmd/spod
GOWORK=off go test ./...
```

- Container execution needs Linux, CGO, a C compiler at build time, and
  unprivileged user namespaces at runtime; the resulting binary is static.
- Image operations (pull, export, inspect) work everywhere, including
  `CGO_ENABLED=0` and non-Linux builds; execution there returns an explicit
  error instead of failing obscurely.
- Tests that run real containers are opt-in:
  `SPOD_REAL_LIFECYCLE_TEST=1 go test ./pkg/engine/ -run TestRealLifecycleAlpine`
  exercises the full lifecycle against `docker.io/library/alpine:3.22`;
  plain tests stay offline.

The included `Containerfile` ships the same static binary in a minimal
Alpine image:

```sh
podman build -t spod .
podman run --rm --userns=host --privileged spod run --rm alpine:3.22 id
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
spod export demo -o demo-rootfs.tar
spod import probe/base:latest demo-rootfs.tar
```

- `export` snapshots a container's root filesystem, changes included;
  `import` turns an archive into a stored image, making captured filesystems
  reproducible starting points.
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
