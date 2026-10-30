# Containerfile for building the spod binary and a minimal runtime image.
#
# Emit the binary as a build artifact, no image involved:
#   podman build --target binary --output type=local,dest=bin .
#
# Build the distribution image:
#   podman build -t spod .
#   docker build -t spod .
#
# The binary is fully static (CGO enabled against musl). The Linux runtime
# needs CGO for the runc/nsenter constructor that runs before the Go runtime
# starts; a CGO_ENABLED=0 build compiles but refuses to run containers.
# Cross-compilation needs a C toolchain for the target architecture, so this
# file builds for the builder platform only.
#
# The image runs spod as an unprivileged user. Running containers from inside
# a container additionally requires user namespace support on the outer
# engine, e.g. for docker:
#   docker run --rm --userns=host --privileged spod run --rm alpine:3.22 id

FROM golang:1.25-alpine AS build

# Module, registry, and TLS dependencies for the build itself, plus the C
# toolchain and kernel headers required by CGO_ENABLED=1.
RUN apk add --no-cache git ca-certificates gcc musl-dev linux-headers

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY pkg/ pkg/

RUN CGO_ENABLED=1 \
    go build -trimpath -tags 'netgo osusergo' \
    -ldflags '-s -w -linkmode external -extldflags "-static"' \
    -o /out/spod ./cmd/spod

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
