#!/bin/sh
# Build the spod binary in a container.
#
# A podman client without a local engine (macOS, where the engine runs in a
# podman machine) cannot export build artifacts with --output type=local.
# This script exports into bin/ on a local engine and copies the artifact
# out of the built image otherwise, so the same invocation works on both.
#
# The script always builds the binary stage, never the runtime image; use
# `podman build --tag spod .` for the latter.
#
# Usage: scripts/build.sh [podman build arguments] [context]
#   scripts/build.sh .
#   scripts/build.sh --build-arg GOARCH=arm64 .
#   scripts/build.sh --target test .   # suite only, no artifact
#
# The context defaults to ".".
set -eu

cd "$(dirname "$0")/.."

artifact=/spod
output=bin/spod
target=

die() {
	printf '%s\n' "$*" >&2
	exit 2
}

# Scanned in a function so the build call below keeps its arguments intact.
scan() {
	while [ "$#" -gt 0 ]; do
		case $1 in
		--target)
			[ "$#" -ge 2 ] || die "build.sh: --target requires a value"
			target=$2
			shift 2
			;;
		--target=*)
			target=${1#*=}
			shift
			;;
		-t | --tag | --tag=* | --output | --output=* | --iidfile | --iidfile=*)
			die "build.sh: $1 is set by the script"
			;;
		*)
			shift
			;;
		esac
	done
}

scan "$@"

# The runtime image is the default stage; the artifact needs its own.
if [ -z "$target" ]; then
	set -- --target binary "$@"
	target=binary
fi

# Targets other than the artifact stage have nothing to extract.
case $target in
binary) ;;
*) exec podman build "$@" ;;
esac

mkdir -p bin

case $(podman info --format '{{.Host.ServiceIsRemote}}') in
false)
	exec podman build --output type=local,dest=bin "$@"
	;;
true) ;;
*)
	die "build.sh: cannot determine whether the podman engine is local"
	;;
esac

# Remote client: build the artifact stage as an image and copy the binary
# out of it. The image and the container are throwaway.
image=spod-build-$$
container=spod-build-$$
cleanup() {
	podman rm -f "$container" >/dev/null 2>&1 || true
	podman rmi -f "$image" >/dev/null 2>&1 || true
}
trap cleanup EXIT
trap 'exit 130' INT TERM HUP

podman build --tag "$image" "$@"
podman create --name "$container" "$image" >/dev/null
podman cp "$container:$artifact" "$output"
