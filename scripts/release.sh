#!/usr/bin/env bash
# Build the release archives for a tag, the way a user downloads gitsynth.
#
#   scripts/release.sh vX.Y.Z
#
# The tag must exist already (git tag -a vX.Y.Z). It is cloned into a
# temporary directory and built there, so the release is exactly the tagged
# commit whatever is checked out or changed here, and with that commit's own
# build script. The tests run first. Then, for each platform:
#
#   dist/vX.Y.Z/gitsynth-vX.Y.Z-OS-ARCH.tar.gz
#     gitsynth-vX.Y.Z-OS-ARCH/gitsynth            the server, with the web UI inside
#     gitsynth-vX.Y.Z-OS-ARCH/deploy/...          the systemd unit and the Caddyfile
#     gitsynth-vX.Y.Z-OS-ARCH/docs/server.md      how to install and run it
#     gitsynth-vX.Y.Z-OS-ARCH/README.md
#     gitsynth-vX.Y.Z-OS-ARCH/LICENSE
#
# and dist/vX.Y.Z/SHA256SUMS, which sha256sum -c checks. Publishing them is a
# separate step. PLATFORMS overrides the default "linux/amd64 linux/arm64".
set -euo pipefail

if [ $# -ne 1 ]; then
	echo "usage: scripts/release.sh vX.Y.Z" >&2
	exit 2
fi
tag=$1
platforms=${PLATFORMS:-linux/amd64 linux/arm64}
root=$(cd "$(dirname "$0")/.." && pwd)
dist=$root/dist/$tag

# go build stamps the binary with the tag only for a semantic version; any
# other name would leave it a pseudo-version.
if ! [[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
	echo "release: $tag is not a version such as v1.2.3 or v1.2.3-rc.1" >&2
	exit 2
fi
if ! git -C "$root" rev-parse -q --verify "refs/tags/$tag^{commit}" >/dev/null; then
	echo "release: there is no tag $tag; make it first: git tag -a $tag -m $tag" >&2
	exit 1
fi
if [ -e "$dist" ]; then
	echo "release: $dist exists; remove it to build $tag again" >&2
	exit 1
fi

sha256() {
	if command -v sha256sum >/dev/null; then
		sha256sum "$@"
	else
		shasum -a 256 "$@"
	fi
}

# The archives hold no owner of this machine and, from macOS, none of its
# extended attributes, which tar on Linux would warn about.
tar_flags=()
if tar --version 2>/dev/null | grep -q 'GNU tar'; then
	tar_flags=(--owner=0 --group=0 --numeric-owner --no-xattrs)
else
	export COPYFILE_DISABLE=1
	tar_flags=(--uid 0 --gid 0 --uname root --gname root --no-xattrs --no-mac-metadata --no-acls)
fi

work=$(mktemp -d "${TMPDIR:-/tmp}/gitsynth-release.XXXXXX")
trap 'rm -rf "$work"' EXIT
src=$work/src
git clone --quiet --no-checkout "$root" "$src"
git -C "$src" -c advice.detachedHead=false checkout --quiet --detach "$tag"
echo "release: $tag is $(git -C "$src" rev-parse HEAD)"

echo "release: running the tests"
(cd "$src" && go test ./...)

mkdir -p "$work/out"
for platform in $platforms; do
	os=${platform%/*}
	arch=${platform#*/}
	name=gitsynth-$tag-$os-$arch
	stage=$work/out/$name
	mkdir -p "$stage/deploy"
	# -trimpath keeps this machine's paths out of the binary.
	GOOS=$os GOARCH=$arch CGO_ENABLED=0 GOFLAGS=-trimpath "$src/scripts/build.sh" "$stage/gitsynth"
	stamped=$(go version -m "$stage/gitsynth" | awk '$1 == "mod" && $2 == "gitsynth" { print $3 }')
	if [ "$stamped" != "$tag" ]; then
		echo "release: $name says it is version ${stamped:-unknown}, not $tag" >&2
		exit 1
	fi
	cp "$src/deploy/gitsynth.service" "$src/deploy/Caddyfile" "$stage/deploy/"
	mkdir -p "$stage/docs"
	cp "$src/docs/server.md" "$stage/docs/"
	cp "$src/README.md" "$src/LICENSE" "$stage/"
	tar "${tar_flags[@]}" -C "$work/out" -czf "$work/out/$name.tar.gz" "$name"
done

mkdir -p "$dist"
mv "$work"/out/*.tar.gz "$dist/"
(cd "$dist" && sha256 *.tar.gz >SHA256SUMS)
echo "release: built $tag into $dist:"
(cd "$dist" && ls -l)
