# Design spike: a microvm execution driver for spod

Status: spike — analysis only, nothing here is implemented or committed
to. Written 2026-11 after spod v0.3.0 (`build`, `--userns keep-id`) to
capture the non-Linux analysis before any engine work is decided.

## Problem

spod's lifecycle, `build` (its `RUN` steps run containers), and `export`
need Linux namespaces, cgroups, and rootless UID mapping. macOS and
Windows have no kernel equivalents, so those platforms get the pure-Go
subsystems only — registry pulls, the store, `import`/`import-oci`,
`prune` — and honest `ErrUnsupported` for the rest. Every engine that
runs containers on those platforms (Docker Desktop, Podman machine,
OrbStack, Apple's `container`) does it by putting a Linux kernel
underneath. The design question is not *whether* to virtualize off-Linux
but *which* VM and *who ships it*.

## Goals and non-goals

Goals:

- Run the existing OCI bundle semantics (the same libcontainer config
  spod produces today) on macOS — and on Linux hosts where stronger
  isolation than namespaces is wanted — without forking the engine's
  command surface or store format.
- Keep the Linux-native path exactly as it is: direct kernel execution
  remains the default and the differentiator. The VM is a driver, not a
  new default.

Non-goals:

- VM-per-container as the identity of the project (that is Apple
  `container`'s model; it changes semantics — see Deltas).
- Windows support via a spod-shipped VM. WSL2 already provides the Linux
  kernel; spod inside WSL is packaging, not engine work.
- FreeBSD jails. Native and VM-free, but rootless containers on FreeBSD
  remain a documented gap (everything runs as root today), and spod is
  rootless-first. Revisit when rootless jails mature. What exists:
  OCI Runtime Spec v1.3 added FreeBSD (Nov 2025), and `runj`/`ocijail`
  run Podman natively on jails — the pieces are real, the rootless story
  is not.

## Approach evaluated: libkrun

[libkrun](https://github.com/libkrun/libkrun) is a lightweight C VMM
library (Firecracker-class) that boots a Linux guest with minimal setup.
It runs on Linux (KVM) and macOS (Hypervisor.framework) and is Podman's
default machine provider on macOS — so the pattern "OCI container inside
a libkrun microVM" is already proven in production, with crun as the
in-guest runtime. It has no Windows backend; Windows keeps the WSL2
packaging story described under Non-goals.

Why it fits spod:

- C API → embeddable from Go (cgo, or purego/dlopen to keep CGO off the
  darwin plain-Go release builds — note `release.yml` builds darwin with
  `CGO_ENABLED=0`; a dlopen-based binding would preserve that).
- One driver, two platforms: the same guest image serves Linux
  (extra isolation) and macOS (the only path to containers at all).
- The guest is Linux, so spod's OCI config, capability set, and
  keep-id-style ID mapping keep their meaning inside the VM; the driver
  translates host requests into guest executions rather than
  re-inventing container semantics.

Alternatives considered and deferred:

- **Direct Virtualization.framework bindings (Apple `container`-style).**
  Apple's Containerization stack is Swift; spod is Go. Binding the
  ObjC surface via cgo is possible, but the VM-per-container model
  requires re-implementing their entire guest stack (kernel + initramfs
  packaging, virtiofs sharing, vznat networking, Rosetta x86) in Go —
  months of work duplicating a mature stack, and it dilutes spod's
  daemonless identity. Deferred.
- **Embedder-level integration.** An application embedding spod can
  bridge to VM-based runtimes itself (Apple `container`, podman
  machine); that is the embedder's cross-platform strategy, outside the
  engine's scope.
- **QEMU/vfkit VMs.** What Podman used before libkrun: heavier, slower
  start, more memory. No reason to skip libkrun.

## Sketch: the driver

- **Selection.** `spod run --vm` (and `build --vm`) selects the microvm
  driver on any host; the default stays native libcontainer. On
  non-Linux hosts the lifecycle commands could default to the driver
  once it exists, with `--vm=off` preserving today's honest error.
- **Guest.** A single pinned kernel plus a minimal initramfs carrying
  crun (or spod's own tiny init) — stored in the spod store like images,
  versioned like OS templates, never floating tags. The guest agent
  accepts one OCI bundle path and a run/attach/exec verb.
- **Filesystem.** virtiofs (macOS, via libkrun's virtio support) for
  bind mounts and the bundle; rootfs images bind from the store. The
  keep-id ownership model changes meaning under a VM — the guest's IDs
  are the VM's, so bind-mount ownership needs an explicit mapping story
  (likely: run the guest as host-UID-mapped via virtiofs, drop
  keep-id-in-VM until proven).
- **Networking.** libkrun's user-mode networking (slirp-style) maps
  guest services to host ports. Host networking — spod's current
  default — is impossible under a VM; `--network host` under `--vm`
  maps to port publishing or is rejected like other network modes today
  (consistent with the Boundaries section: reject, don't silently map).
- **Store.** Shared: the store stays on the host; the guest reaches the
  store through virtiofs. `build` under `--vm` runs its `RUN` steps as
  in-guest containers and writes the squashed layer back to the host
  store; `export` snapshots in-guest container rootfs the same way.

## Semantics deltas (document if shipped)

- `--network host` (see above) and `localhost` inside containers no
  longer share the host loopback.
- Bind-mount performance drops (virtiofs); huge directory trees will
  feel different.
- /dev, /proc, sysfs inside the guest differ from the host kernel spod
  normally exposes; kernel-version-dependent behavior changes.
- `--userns keep-id` (see Filesystem) needs its own design under the VM
  or a documented "not supported with --vm" answer.

## Milestone sketch

1. **Spike (this doc → prototype).** Boot a pinned kernel + initramfs in
   libkrun from Go on Linux-KVM and macOS-HVF; run `alpine:3.22` from an
   OCI bundle with crun in-guest; prove store-over-virtiofs.
2. **Driver MVP.** `spod run --vm` with attach/exec/logs through the
   guest agent; store integration; README platform table updated.
3. **Build/export under the driver.** `build --vm` (RUN steps in-guest),
   `export` of in-guest containers.
4. **Decide the default story for non-Linux hosts** (driver-on-demand vs
   automatic with explicit opt-out) and the keep-id/VM mapping.

Estimate: milestone 1 is days of prototyping once the guest image
pipeline exists; 2–3 are engine work measured in weeks, gated on the
virtiofs store and networking decisions above.
