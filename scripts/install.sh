#!/bin/sh
# docker-extras installer. Published on every release as
# docker-extras-install.sh, so:
#
#   curl -fsSL https://github.com/procrastivity/docker-extras/releases/latest/download/docker-extras-install.sh | sh
#
# Configuration is environment variables, not flags (flags are awkward to
# pass through a curl | sh pipe):
#   DOCKER_EXTRAS_BASE_URL     repo web URL (default: the procrastivity repo)
#   DOCKER_EXTRAS_VERSION      vX.Y.Z to pin; empty means the latest release
#   DOCKER_EXTRAS_INSTALL_DIR  where the tools land (default: ~/.local/bin)
#   DOCKER_EXTRAS_NO_PLUGIN    non-empty skips the docker CLI plugin
#
# The tools land in DOCKER_EXTRAS_INSTALL_DIR; the umbrella plugin is copied
# to ~/.docker/cli-plugins/docker-extras so `docker extras TOOL` works (the
# dispatcher falls back to PATH to find the tools).
set -eu

say() { printf 'docker-extras-install: %s\n' "$*" >&2; }
die() { printf 'docker-extras-install: error: %s\n' "$*" >&2; exit 1; }

BASE_URL="${DOCKER_EXTRAS_BASE_URL:-https://github.com/procrastivity/docker-extras}"
VERSION="${DOCKER_EXTRAS_VERSION:-}"
INSTALL_DIR="${DOCKER_EXTRAS_INSTALL_DIR:-$HOME/.local/bin}"
PLUGIN_DIR="$HOME/.docker/cli-plugins"

command -v curl >/dev/null 2>&1 || die "curl is required"
command -v tar >/dev/null 2>&1 || die "tar is required"

# GitHub's URL shapes: 'latest/download' resolves the newest release;
# 'download/vX.Y.Z' pins one (note: 'download', singular).
if [ -n "$VERSION" ]; then
	dl="$BASE_URL/releases/download/$VERSION"
else
	dl="$BASE_URL/releases/latest/download"
fi

asset="docker-extras.tar.gz"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "downloading $dl/$asset"
curl -fsSL "$dl/$asset" -o "$tmp/$asset" || die "download failed: $dl/$asset"
curl -fsSL "$dl/SHA256SUMS" -o "$tmp/SHA256SUMS" || die "download failed: $dl/SHA256SUMS"

if command -v sha256sum >/dev/null 2>&1; then
	sum_cmd="sha256sum"
else
	# macOS ships shasum, not GNU sha256sum.
	sum_cmd="shasum -a 256"
fi
# grep-then-check selects the one line — portable, unlike GNU-only
# --ignore-missing.
(cd "$tmp" && grep " $asset\$" SHA256SUMS | $sum_cmd -c -) ||
	die "checksum verification failed for $asset"

tar -xzf "$tmp/$asset" -C "$tmp" || die "could not extract $asset"
[ -d "$tmp/bin" ] || die "the archive holds no bin/ directory"

mkdir -p "$INSTALL_DIR"
installed=""
for f in "$tmp"/bin/docker-extras-*; do
	[ -f "$f" ] || die "the archive holds no docker-extras-* tools"
	name=$(basename "$f")
	if command -v install >/dev/null 2>&1; then
		install -m 0755 "$f" "$INSTALL_DIR/$name"
	else
		cp "$f" "$INSTALL_DIR/$name" && chmod 0755 "$INSTALL_DIR/$name"
	fi
	installed="$installed $name"
done
say "installed$installed to $INSTALL_DIR"

if [ -z "${DOCKER_EXTRAS_NO_PLUGIN:-}" ]; then
	if [ -f "$tmp/plugin/docker-extras" ]; then
		mkdir -p "$PLUGIN_DIR"
		if command -v install >/dev/null 2>&1; then
			install -m 0755 "$tmp/plugin/docker-extras" "$PLUGIN_DIR/docker-extras"
		else
			cp "$tmp/plugin/docker-extras" "$PLUGIN_DIR/docker-extras" && chmod 0755 "$PLUGIN_DIR/docker-extras"
		fi
		say "installed the CLI plugin to $PLUGIN_DIR/docker-extras (try: docker extras)"
	else
		say "warning: the archive holds no plugin/docker-extras; skipping the plugin"
	fi
else
	say "skipping the CLI plugin (DOCKER_EXTRAS_NO_PLUGIN is set)"
fi

case ":$PATH:" in
*":$INSTALL_DIR:"*) ;;
*) say "warning: $INSTALL_DIR is not on PATH" ;;
esac
