#!/bin/sh
# docker-extras uninstaller. Published on every release as
# docker-extras-uninstall.sh, so:
#
#   curl -fsSL https://github.com/procrastivity/docker-extras/releases/latest/download/docker-extras-uninstall.sh | sh
#
# The installer writes an ownership/checksum record to
# ${XDG_STATE_HOME:-~/.local/state}/docker-extras/install.tsv. This script
# removes only the files in that record, and refuses to remove anything that
# is missing, modified, symlinked, or outside the recorded tool/plugin paths.
set -eu

say() { printf 'docker-extras-uninstall: %s\n' "$*" >&2; }
die() { printf 'docker-extras-uninstall: error: %s\n' "$*" >&2; exit 1; }

STATE_DIR="${XDG_STATE_HOME:-$HOME/.local/state}/docker-extras"
STATE_FILE="$STATE_DIR/install.tsv"

[ -f "$STATE_FILE" ] ||
  die "no install record found at $STATE_FILE; refusing to guess what to remove"
command -v awk >/dev/null 2>&1 || die "awk is required"
command -v mktemp >/dev/null 2>&1 || die "mktemp is required"

if command -v sha256sum >/dev/null 2>&1; then
  hash_file() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  hash_file() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  die "sha256sum or shasum is required"
fi

files=$(mktemp)
trap 'rm -f "$files"' EXIT INT TERM
version=""
install_dir=""
plugin_name=""
plugin_path=""

while IFS='|' read -r kind first second; do
  case "$kind" in
    version) version="$first" ;;
    install_dir) install_dir="$first" ;;
    plugin_name) plugin_name="$first" ;;
    plugin_path) plugin_path="$first" ;;
    file)
      [ -n "$first" ] && [ -n "$second" ] || die "malformed file entry in $STATE_FILE"
      printf '%s\n' "$first|$second" >> "$files"
      ;;
    "") ;;
    *) die "malformed entry in $STATE_FILE" ;;
  esac
done < "$STATE_FILE"

[ "$version" = 1 ] || die "unsupported install record version${version:+ $version}"
[ -n "$install_dir" ] || die "install record has no tool directory"
[ -n "$plugin_name" ] || die "install record has no plugin name"

count=0
while IFS='|' read -r path expected; do
  count=$((count + 1))
  case "$path" in
    "$install_dir"/docker-extras-*) ;;
    "$plugin_path") [ -n "$plugin_path" ] || die "malformed plugin entry in $STATE_FILE" ;;
    *) die "refusing to remove path outside the recorded installation: $path" ;;
  esac
  [ -f "$path" ] && [ ! -L "$path" ] ||
    die "refusing to remove missing or symlinked file: $path"
  actual=$(hash_file "$path")
  [ "$actual" = "$expected" ] ||
    die "refusing to remove modified file: $path"
done < "$files"
[ "$count" -gt 0 ] || die "install record contains no files"

# Every file has been verified before the first removal, so a hand edit
# cannot leave a partially-uninstalled set of files behind.
while IFS='|' read -r path _expected; do
  rm -f "$path"
done < "$files"
rm -f "$STATE_FILE"
rmdir "$STATE_DIR" 2>/dev/null || true

say "removed the docker-extras tools from $install_dir"
if [ -n "$plugin_path" ]; then
  say "removed the Docker CLI plugin '$plugin_name'"
else
  say "no Docker CLI plugin was installed"
fi
