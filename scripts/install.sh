#!/bin/sh
# docker-extras installer. Published on every release as
# docker-extras-install.sh, so:
#
#   curl -fsSL https://github.com/procrastivity/docker-extras/releases/latest/download/docker-extras-install.sh | sh
#
# Configuration is environment variables, not flags (flags are awkward to
# pass through a curl | sh pipe):
#   DOCKER_EXTRAS_BASE_URL       repo web URL (default: the procrastivity repo)
#   DOCKER_EXTRAS_VERSION        vX.Y.Z to pin; empty means the latest release
#   DOCKER_EXTRAS_INSTALL_DIR    where the tools land (default: ~/.local/bin)
#   DOCKER_EXTRAS_PLUGIN_NAME    Docker command name (default: extras)
#   DOCKER_EXTRAS_NO_PLUGIN      non-empty skips the docker CLI plugin
#
# The installer records every installed file and its checksum in
# ${XDG_STATE_HOME:-~/.local/state}/docker-extras/install.tsv. The companion
# docker-extras-uninstall.sh uses that record rather than guessing, so an
# overridden plugin name is removed correctly and modified files are refused.
set -eu

say() { printf 'docker-extras-install: %s\n' "$*" >&2; }
die() { printf 'docker-extras-install: error: %s\n' "$*" >&2; exit 1; }

BASE_URL="${DOCKER_EXTRAS_BASE_URL:-https://github.com/procrastivity/docker-extras}"
VERSION="${DOCKER_EXTRAS_VERSION:-}"
INSTALL_DIR="${DOCKER_EXTRAS_INSTALL_DIR:-$HOME/.local/bin}"
PLUGIN_NAME="${DOCKER_EXTRAS_PLUGIN_NAME:-extras}"
PLUGIN_DIR="$HOME/.docker/cli-plugins"
STATE_DIR="${XDG_STATE_HOME:-$HOME/.local/state}/docker-extras"
STATE_FILE="$STATE_DIR/install.tsv"

case "$PLUGIN_NAME" in
  [a-z]*) ;;
  *) die "invalid plugin name '$PLUGIN_NAME' (must start with a lowercase letter)" ;;
esac
case "$PLUGIN_NAME" in
  *[!a-z0-9]*) die "invalid plugin name '$PLUGIN_NAME' (use lowercase letters and digits only)" ;;
esac
case "$INSTALL_DIR:$PLUGIN_DIR:$STATE_DIR" in
  *'|'*) die "install paths cannot contain the state-file delimiter '|'" ;;
esac

command -v curl >/dev/null 2>&1 || die "curl is required"
command -v tar >/dev/null 2>&1 || die "tar is required"
command -v mktemp >/dev/null 2>&1 || die "mktemp is required"
command -v awk >/dev/null 2>&1 || die "awk is required"

if command -v sha256sum >/dev/null 2>&1; then
  hash_file() { sha256sum "$1" | awk '{print $1}'; }
  verify_checksum() { sha256sum -c -; }
elif command -v shasum >/dev/null 2>&1; then
  hash_file() { shasum -a 256 "$1" | awk '{print $1}'; }
  verify_checksum() { shasum -a 256 -c -; }
else
  die "sha256sum or shasum is required"
fi

# GitHub's URL shapes: 'latest/download' resolves the newest release;
# 'download/vX.Y.Z' pins one (note: 'download', singular).
if [ -n "$VERSION" ]; then
  dl="$BASE_URL/releases/download/$VERSION"
else
  dl="$BASE_URL/releases/latest/download"
fi

asset="docker-extras.tar.gz"
tmp=$(mktemp -d)
installed_files="$tmp/installed-files"
trap 'rm -rf "$tmp"' EXIT INT TERM
: > "$installed_files"

# A second install with the same destinations is an upgrade. Changing the
# plugin name or tool directory without uninstalling first would strand the
# previous files, so refuse that ambiguous transition instead.
if [ -f "$STATE_FILE" ]; then
  old_install_dir=$(awk -F '|' '$1 == "install_dir" { print $2; exit }' "$STATE_FILE")
  old_plugin_path=$(awk -F '|' '$1 == "plugin_path" { print $2; exit }' "$STATE_FILE")
  new_plugin_path="$PLUGIN_DIR/docker-$PLUGIN_NAME"
  if [ "$old_install_dir" != "$INSTALL_DIR" ]; then
    die "an installation is already recorded at $old_install_dir; run docker-extras-uninstall.sh first"
  fi
  if [ "$old_plugin_path" != "$new_plugin_path" ] && [ -n "$old_plugin_path" ]; then
    die "an installation is already using $old_plugin_path; run docker-extras-uninstall.sh first"
  fi
fi

say "downloading $dl/$asset"
curl -fsSL "$dl/$asset" -o "$tmp/$asset" || die "download failed: $dl/$asset"
curl -fsSL "$dl/SHA256SUMS" -o "$tmp/SHA256SUMS" || die "download failed: $dl/SHA256SUMS"

# grep-then-check selects the one line — portable, unlike GNU-only
# --ignore-missing.
(cd "$tmp" && grep " $asset\$" SHA256SUMS | verify_checksum) ||
  die "checksum verification failed for $asset"

tar -xzf "$tmp/$asset" -C "$tmp" || die "could not extract $asset"
[ -d "$tmp/bin" ] || die "the archive holds no bin/ directory"

plugin_path=""
if [ -z "${DOCKER_EXTRAS_NO_PLUGIN:-}" ] && [ -f "$tmp/plugin/docker-extras" ]; then
  plugin_path="$PLUGIN_DIR/docker-$PLUGIN_NAME"
  if [ ! -f "$STATE_FILE" ] && { [ -e "$plugin_path" ] || [ -L "$plugin_path" ]; }; then
    die "refusing to overwrite existing Docker CLI plugin $plugin_path"
  fi
fi

mkdir -p "$INSTALL_DIR"
installed=""
for f in "$tmp"/bin/docker-extras-*; do
  [ -f "$f" ] || die "the archive holds no docker-extras-* tools"
  name=$(basename "$f")
  dest="$INSTALL_DIR/$name"
  if command -v install >/dev/null 2>&1; then
    install -m 0755 "$f" "$dest"
  else
    cp "$f" "$dest" && chmod 0755 "$dest"
  fi
  printf '%s\n' "$dest" >> "$installed_files"
  installed="$installed $name"
done
say "installed$installed to $INSTALL_DIR"

if [ -z "${DOCKER_EXTRAS_NO_PLUGIN:-}" ]; then
  if [ -f "$tmp/plugin/docker-extras" ]; then
    mkdir -p "$PLUGIN_DIR"
    if command -v install >/dev/null 2>&1; then
      install -m 0755 "$tmp/plugin/docker-extras" "$plugin_path"
    else
      cp "$tmp/plugin/docker-extras" "$plugin_path" && chmod 0755 "$plugin_path"
    fi
    printf '%s\n' "$plugin_path" >> "$installed_files"
    say "installed the CLI plugin to $plugin_path (try: docker $PLUGIN_NAME)"
  else
    say "warning: the archive holds no plugin/docker-extras; skipping the plugin"
  fi
else
  say "skipping the CLI plugin (DOCKER_EXTRAS_NO_PLUGIN is set)"
fi

# Write ownership state only after every requested file is in place. The
# state file itself is not part of the removable file list, and is replaced
# atomically so an interrupted write cannot create a plausible partial record.
mkdir -p "$STATE_DIR"
state_tmp=$(mktemp "$STATE_DIR/.install.XXXXXX")
trap 'rm -rf "$tmp" "$state_tmp"' EXIT INT TERM
{
  printf '%s\n' 'version|1'
  printf '%s\n' "install_dir|$INSTALL_DIR"
  printf '%s\n' "plugin_name|$PLUGIN_NAME"
  printf '%s\n' "plugin_path|$plugin_path"
  while IFS= read -r path; do
    printf '%s\n' "file|$path|$(hash_file "$path")"
  done < "$installed_files"
} > "$state_tmp"
chmod 0600 "$state_tmp"
mv "$state_tmp" "$STATE_FILE"

case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) say "warning: $INSTALL_DIR is not on PATH" ;;
esac
