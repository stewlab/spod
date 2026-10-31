# Containerfile for building the spod binary and a minimal runtime image.
#
# Emit the binary as a build artifact, no image involved:
#   podman build --target binary --output type=local,dest=bin .
#
# Build the distribution image:
#   podman build --tag spod .
#   docker build --tag spod .
#
# Run the test suite in the container (exit code is the test result):
#   podman build --target test .
#
# The binary is fully static (CGO enabled against musl). The Linux runtime
# needs CGO for the runc/nsenter constructor that runs before the Go runtime
# starts; a CGO_ENABLED=0 build compiles but refuses to run containers.
# Cross-compilation needs no per-target toolchain: the build stage uses
# zig cc (bundled musl) for the builder platform and every target, so a
# foreign binary is one build argument away — the container itself always
# runs on the builder platform, never under emulation:
#   podman build --build-arg GOARCH=arm64 --target binary --output type=local,dest=bin .
#
# The image runs spod as an unprivileged user. Running containers from inside
# a container additionally requires user namespace support on the outer
# engine, e.g. for docker:
#   docker run --rm --userns=host --privileged spod run --rm alpine:3.22 id

FROM golang:1.25-alpine AS build

# Module, registry, and TLS dependencies for the build itself, plus zig —
# the single C compiler for every target — and the kernel headers its
# bundled musl complements for CGO_ENABLED=1.
RUN apk add --no-cache git ca-certificates zig linux-headers

# GOARCH selects the target; the case maps it to the zig triple.
ARG GOARCH=amd64
RUN case "$GOARCH" in \
      amd64) echo x86_64-linux-musl > /tmp/zigtarget ;; \
      arm64) echo aarch64-linux-musl > /tmp/zigtarget ;; \
      riscv64) echo riscv64-linux-musl > /tmp/zigtarget ;; \
      386) echo x86-linux-musl > /tmp/zigtarget ;; \
      arm) echo arm-linux-musleabihf > /tmp/zigtarget ;; \
      *) echo "unsupported GOARCH: $GOARCH" >&2; exit 1 ;; \
    esac

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
# The whole checkout, .git included, so the build can stamp its state:
# a tagged checkout stamps the tag, an untagged clone stamps the Go
# toolchain's vcs.revision (surfaced as dev-<commit>), and sources
# without usable git metadata stay dev. .containerignore keeps build
# outputs out of the context, so a clean checkout reads as clean.
COPY . .

RUN V="$(git describe --tags --dirty 2>/dev/null || true)"; \
    CGO_ENABLED=1 CC="zig cc -target $(cat /tmp/zigtarget)" \
    go build -trimpath -tags 'netgo osusergo' \
    -ldflags "-s -w -linkmode external -extldflags \"-static\" ${V:+-X github.com/stewlab/spod/pkg/cli.version=$V}" \
    -o /out/spod ./cmd/spod

# Run the unit tests; a failing suite fails the build.
FROM build AS test
RUN CGO_ENABLED=1 CC="zig cc -target $(cat /tmp/zigtarget)" go test -ldflags '-linkmode external -extldflags "-static"' ./...

# Artifact target for --output; placed before the runtime stage so a plain
# build still produces the image.
FROM scratch AS binary
COPY --from=build /out/spod /spod

FROM alpine:3.22

# CA certificates for registry access over TLS.
RUN apk add --no-cache ca-certificates && adduser -D -u 1000 spod

COPY --from=build /out/spod /usr/local/bin/spod

USER spod
ENTRYPOINT ["/usr/local/bin/spod"]
