#!/bin/sh
# docker-extras installer. Published on every release as
# docker-extras-install.sh, so:
#
#   curl -fsSL https://github.com/procrastivity/docker-extras/releases/latest/download/docker-extras-install.sh | sh
#
# Installs one binary, docker-extras, into a directory meant for PATH, and by
# default links it into ~/.docker/cli-plugins so `docker extras ...` works too.
# Docker discovers plugins only in its plugin directories, never on PATH, and
# runs a plugin through the path it found, so the symlink name selects the
# Docker command name while the PATH name stays docker-extras.
#
# Configuration is environment variables, not flags (flags are awkward to
# pass through a curl | sh pipe):
#   DOCKER_EXTRAS_BASE_URL       repo web URL (default: the procrastivity repo)
#   DOCKER_EXTRAS_VERSION        vX.Y.Z to pin; empty means the latest release
#   DOCKER_EXTRAS_INSTALL_DIR    where docker-extras lands (default: ~/.local/bin)
#   DOCKER_EXTRAS_PLUGIN_NAME    Docker command name (default: extras)
#   DOCKER_EXTRAS_NO_PLUGIN      non-empty skips the Docker CLI plugin link
#
# The installer records every installed file and link in
# ${XDG_STATE_HOME:-~/.local/state}/docker-extras/install.tsv. The companion
# docker-extras-uninstall.sh uses that record rather than guessing.
set -eu

say() { printf 'docker-extras-install: %s\n' "$*" >&2; }
die() { printf 'docker-extras-install: error: %s\n' "$*" >&2; exit 1; }

BASE_URL="${DOCKER_EXTRAS_BASE_URL:-https://github.com/procrastivity/docker-extras}"
VERSION="${DOCKER_EXTRAS_VERSION:-}"
INSTALL_DIR="${DOCKER_EXTRAS_INSTALL_DIR:-$HOME/.local/bin}"
BIN_PATH="$INSTALL_DIR/docker-extras"
PLUGIN_NAME="${DOCKER_EXTRAS_PLUGIN_NAME:-extras}"
PLUGIN_DIR="$HOME/.docker/cli-plugins"
PLUGIN_PATH="$PLUGIN_DIR/docker-$PLUGIN_NAME"
[ -z "${DOCKER_EXTRAS_NO_PLUGIN:-}" ] || PLUGIN_PATH=""
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
case "$INSTALL_DIR" in
  /*) ;;
  *) die "non-absolute installation directory: $INSTALL_DIR" ;;
esac
[ "$INSTALL_DIR" != / ] || die "unsafe installation directory: $INSTALL_DIR"
# The plugin directory holds the link, never the binary: Docker would treat a
# second docker-* file there as another plugin.
[ "$INSTALL_DIR" != "$PLUGIN_DIR" ] ||
  die "DOCKER_EXTRAS_INSTALL_DIR cannot be the Docker plugin directory $PLUGIN_DIR"

os=$(uname -s) || die "could not determine operating system"
arch=$(uname -m) || die "could not determine processor architecture"
case "$os:$arch" in
  Linux:x86_64) os=linux; arch=amd64 ;;
  Linux:aarch64) os=linux; arch=arm64 ;;
  Darwin:x86_64) os=darwin; arch=amd64 ;;
  Darwin:arm64|Darwin:aarch64) os=darwin; arch=arm64 ;;
  *) die "unsupported platform $os/$arch; supported targets are linux/amd64, linux/arm64, darwin/amd64, and darwin/arm64" ;;
esac
asset="docker-extras-$os-$arch.tar.gz"

command -v curl >/dev/null 2>&1 || die "curl is required"
command -v tar >/dev/null 2>&1 || die "tar is required"
command -v mktemp >/dev/null 2>&1 || die "mktemp is required"
command -v awk >/dev/null 2>&1 || die "awk is required"
command -v uname >/dev/null 2>&1 || die "uname is required"
command -v readlink >/dev/null 2>&1 || die "readlink is required"

if command -v sha256sum >/dev/null 2>&1; then
  hash_file() { sha256sum "$1" | awk '{print $1}'; }
  verify_checksum() { sha256sum -c -; }
elif command -v shasum >/dev/null 2>&1; then
  hash_file() { shasum -a 256 "$1" | awk '{print $1}'; }
  verify_checksum() { shasum -a 256 -c -; }
else
  die "sha256sum or shasum is required"
fi

# An audited leaf is not safe to replace if an ancestor redirects it elsewhere.
# Reject non-normalized paths too, so each lexical component is checked.
check_directory_chain() {
  path=$1
  case "$path" in /*) ;; *) die "non-absolute installation directory: $path" ;; esac
  case "$path/" in *'/../'*|*'/./'*|*'//'*) die "non-normalized installation directory: $path" ;; esac
  while [ "$path" != / ]; do
    [ ! -L "$path" ] || die "refusing symlinked installation directory: $path"
    path=${path%/*}
    [ -n "$path" ] || path=/
  done
}
check_directory_chain "$INSTALL_DIR"
[ -z "$PLUGIN_PATH" ] || check_directory_chain "$PLUGIN_DIR"
check_directory_chain "$STATE_DIR"

if [ -n "$VERSION" ]; then
  dl="$BASE_URL/releases/download/$VERSION"
else
  dl="$BASE_URL/releases/latest/download"
fi

tmp=$(mktemp -d)
state_tmp=""
staged_bin=""
staged_link=""
cleanup() {
  rm -rf "$tmp"
  [ -z "$state_tmp" ] || rm -f "$state_tmp"
  [ -z "$staged_bin" ] || rm -f "$staged_bin"
  [ -z "$staged_link" ] || rm -f "$staged_link"
}
trap cleanup EXIT
trap 'cleanup; exit 1' HUP INT TERM

say "downloading $dl/$asset"
curl -fsSL "$dl/$asset" -o "$tmp/$asset" || die "download failed: $dl/$asset"
curl -fsSL "$dl/SHA256SUMS" -o "$tmp/SHA256SUMS" || die "download failed: $dl/SHA256SUMS"

sum_line=$(awk -v asset="$asset" '
  $2 == asset {
    if (NF != 2 || length($1) != 64 || $1 ~ /[^0-9a-f]/) exit 1
    count++
    line=$0
  }
  END { if (count != 1) exit 1; print line }
' "$tmp/SHA256SUMS") || die "SHA256SUMS has no unique valid entry for $asset"
(cd "$tmp" && printf '%s\n' "$sum_line" | verify_checksum >/dev/null) ||
  die "checksum verification failed for $asset"

# The archive layout predates the PATH install and stays fixed, so this
# installer can still install a pinned older release.
tar -tzf "$tmp/$asset" > "$tmp/members" || die "could not list $asset"
awk '
  $0 == "plugin/docker-extras" { plugin++ ; next }
  $0 == "LICENSE" { license++ ; next }
  { exit 1 }
  END { if (plugin != 1 || license != 1) exit 1 }
' "$tmp/members" || die "$asset must contain only plugin/docker-extras and LICENSE"
tar -xzf "$tmp/$asset" -C "$tmp" plugin/docker-extras || die "could not extract binary from $asset"
if [ ! -f "$tmp/plugin/docker-extras" ] || [ -L "$tmp/plugin/docker-extras" ]; then
  die "$asset has no regular plugin/docker-extras executable"
fi

# Audit the complete previous install record before any destination or state
# write. Every recorded entry becomes one "kind|path|value" line in old-entries:
# a file with its checksum, or a link with its target. A changed or foreign
# entry blocks the whole install.
#
# Record versions:
#   1  the v1 Bash tools (+ optional plugin), and the plugin-only Go release,
#      which reused the v1 layout with install_dir set to the plugin directory
#   2  docker-extras on PATH (bin_path) + optional plugin symlink (link entry)
: > "$tmp/old-entries"
have_old_state=0
if [ -e "$STATE_FILE" ] || [ -L "$STATE_FILE" ]; then
  if [ ! -f "$STATE_FILE" ] || [ -L "$STATE_FILE" ]; then
    die "refusing malformed or symlinked install record $STATE_FILE"
  fi
  old_version=$(awk -F '|' '$1 == "version" { print $2; exit }' "$STATE_FILE")
  case "$old_version" in
    1)
      awk -F '|' '
        $1 == "version" { if (NF != 2 || $2 != "1" || version++) exit 1; next }
        $1 == "install_dir" { if (NF != 2 || $2 == "" || install++) exit 1; next }
        $1 == "plugin_name" { if (NF != 2 || $2 == "" || name++) exit 1; next }
        $1 == "plugin_path" { if (NF != 2 || path++) exit 1; next }
        $1 == "file" {
          if (NF != 3 || $2 == "" || length($3) != 64 || $3 ~ /[^0-9a-f]/ || seen[$2]++) exit 1
          files++
          next
        }
        { exit 1 }
        END { if (version != 1 || install != 1 || name != 1 || path != 1 || files < 1) exit 1 }
      ' "$STATE_FILE" || die "malformed or unsupported install record $STATE_FILE"

      old_install_dir=$(awk -F '|' '$1 == "install_dir" { print $2 }' "$STATE_FILE")
      old_plugin_name=$(awk -F '|' '$1 == "plugin_name" { print $2 }' "$STATE_FILE")
      old_plugin_path=$(awk -F '|' '$1 == "plugin_path" { print $2 }' "$STATE_FILE")
      case "$old_install_dir" in
        /*) ;;
        *) die "legacy install record has a non-absolute tool directory" ;;
      esac
      [ "$old_install_dir" != / ] || die "legacy install record has an unsafe tool directory"
      case "/$old_install_dir/" in
        *"/../"*|*"/./"*) die "legacy install record has a non-normalized tool directory" ;;
      esac
      check_directory_chain "$old_install_dir"
      case "$old_plugin_name" in
        [a-z]*) ;;
        *) die "legacy install record has an invalid plugin name" ;;
      esac
      case "$old_plugin_name" in
        *[!a-z0-9]*) die "legacy install record has an invalid plugin name" ;;
      esac
      if [ -n "$old_plugin_path" ] &&
         [ "$old_plugin_path" != "$HOME/.docker/cli-plugins/docker-$old_plugin_name" ]; then
        die "legacy install record has an out-of-scope plugin path"
      fi

      awk -F '|' '$1 == "file" { print $2 "|" $3 }' "$STATE_FILE" > "$tmp/old-files"
      while IFS='|' read -r old_path expected; do
        case "$old_path" in
          "$old_install_dir"/docker-extras-*)
            old_base=${old_path##*/}
            if [ "$old_path" != "$old_install_dir/$old_base" ] || [ "$old_base" = docker-extras- ]; then
              die "legacy file path is outside the recorded tool directory: $old_path"
            fi
            ;;
          "$old_plugin_path")
            [ -n "$old_plugin_path" ] || die "legacy record has an invalid plugin file entry"
            ;;
          *) die "legacy file path is outside the recorded installation: $old_path" ;;
        esac
        printf 'file|%s|%s\n' "$old_path" "$expected" >> "$tmp/old-entries"
      done < "$tmp/old-files"
      if [ -n "$old_plugin_path" ] &&
         ! awk -F '|' -v path="$old_plugin_path" '$1 == "file" && $2 == path { found++ } END { exit found != 1 }' "$STATE_FILE"; then
        die "legacy plugin path is not fully recorded in $STATE_FILE"
      fi
      ;;
    2)
      awk -F '|' '
        $1 == "version" { if (NF != 2 || $2 != "2" || version++) exit 1; next }
        $1 == "bin_path" { if (NF != 2 || $2 == "" || bin++) exit 1; bin_path = $2; next }
        $1 == "plugin_name" { if (NF != 2 || $2 == "" || name++) exit 1; next }
        $1 == "plugin_path" { if (NF != 2 || path++) exit 1; plugin_path = $2; next }
        $1 == "file" {
          if (NF != 3 || $2 == "" || length($3) != 64 || $3 ~ /[^0-9a-f]/ || files++) exit 1
          file_path = $2
          next
        }
        $1 == "link" {
          if (NF != 3 || $2 == "" || $3 == "" || links++) exit 1
          link_path = $2; link_target = $3
          next
        }
        { exit 1 }
        END {
          if (version != 1 || bin != 1 || name != 1 || path != 1 || files != 1) exit 1
          if (file_path != bin_path) exit 1
          if (plugin_path == "" && links != 0) exit 1
          if (plugin_path != "" && (links != 1 || link_path != plugin_path || link_target != bin_path)) exit 1
        }
      ' "$STATE_FILE" || die "malformed or unsupported install record $STATE_FILE"

      old_bin_path=$(awk -F '|' '$1 == "bin_path" { print $2 }' "$STATE_FILE")
      old_plugin_name=$(awk -F '|' '$1 == "plugin_name" { print $2 }' "$STATE_FILE")
      old_plugin_path=$(awk -F '|' '$1 == "plugin_path" { print $2 }' "$STATE_FILE")
      [ "${old_bin_path##*/}" = docker-extras ] || die "install record has an out-of-scope binary path"
      old_bin_dir=${old_bin_path%/*}
      [ -n "$old_bin_dir" ] || die "install record has an unsafe tool directory"
      check_directory_chain "$old_bin_dir"
      case "$old_plugin_name" in
        [a-z]*) ;;
        *) die "install record has an invalid plugin name" ;;
      esac
      case "$old_plugin_name" in
        *[!a-z0-9]*) die "install record has an invalid plugin name" ;;
      esac
      if [ -n "$old_plugin_path" ]; then
        [ "$old_plugin_path" = "$HOME/.docker/cli-plugins/docker-$old_plugin_name" ] ||
          die "install record has an out-of-scope plugin path"
        check_directory_chain "$HOME/.docker/cli-plugins"
      fi
      awk -F '|' '$1 == "file" || $1 == "link"' "$STATE_FILE" >> "$tmp/old-entries"
      ;;
    *) die "malformed or unsupported install record $STATE_FILE" ;;
  esac

  while IFS='|' read -r kind old_path expected; do
    case "$kind" in
      file)
        if [ ! -f "$old_path" ] || [ -L "$old_path" ]; then
          die "refusing missing, non-regular, or symlinked recorded file: $old_path"
        fi
        actual=$(hash_file "$old_path") || die "could not hash recorded file: $old_path"
        [ "$actual" = "$expected" ] || die "refusing modified recorded file: $old_path"
        ;;
      link)
        [ -L "$old_path" ] || die "refusing missing or replaced recorded link: $old_path"
        actual=$(readlink "$old_path") || die "could not read recorded link: $old_path"
        [ "$actual" = "$expected" ] || die "refusing retargeted recorded link: $old_path"
        ;;
      *) die "malformed install record entry in $STATE_FILE" ;;
    esac
  done < "$tmp/old-entries"
  have_old_state=1
fi

# owned PATH: true when the audited record owns PATH as a file or link.
owned() {
  [ "$have_old_state" -eq 1 ] &&
    awk -F '|' -v p="$1" '$2 == p { found = 1 } END { exit !found }' "$tmp/old-entries"
}

# A destination can be replaced only when the full old state audit proved it
# is this installer's. Every other file, directory, or symlink is foreign.
if [ -e "$BIN_PATH" ] || [ -L "$BIN_PATH" ]; then
  owned "$BIN_PATH" || die "refusing to overwrite existing $BIN_PATH"
fi
if [ -n "$PLUGIN_PATH" ] && { [ -e "$PLUGIN_PATH" ] || [ -L "$PLUGIN_PATH" ]; }; then
  owned "$PLUGIN_PATH" || die "refusing to overwrite existing Docker CLI plugin $PLUGIN_PATH"
fi

# Stage beside each destination so the final renames are atomic. The binary
# lands before the link, so the link never dangles. The old state is not
# replaced until every obsolete, verified entry has been removed.
mkdir -p "$INSTALL_DIR" "$STATE_DIR"
staged_bin=$(mktemp "$INSTALL_DIR/.docker-extras.XXXXXX") || die "could not stage binary in $INSTALL_DIR"
cp "$tmp/plugin/docker-extras" "$staged_bin" || die "could not stage binary"
chmod 0755 "$staged_bin" || die "could not set binary permissions"
bin_hash=$(hash_file "$staged_bin") || die "could not hash staged binary"
if [ -n "$PLUGIN_PATH" ]; then
  mkdir -p "$PLUGIN_DIR"
  staged_link=$(mktemp "$PLUGIN_DIR/.docker-$PLUGIN_NAME.XXXXXX") || die "could not stage plugin link in $PLUGIN_DIR"
  rm -f "$staged_link" || die "could not stage plugin link"
  ln -s "$BIN_PATH" "$staged_link" || die "could not stage plugin link"
fi
state_tmp=$(mktemp "$STATE_DIR/.install.XXXXXX") || die "could not stage install record"
{
  printf '%s\n' 'version|2'
  printf '%s\n' "bin_path|$BIN_PATH"
  printf '%s\n' "plugin_name|$PLUGIN_NAME"
  printf '%s\n' "plugin_path|$PLUGIN_PATH"
  printf '%s\n' "file|$BIN_PATH|$bin_hash"
  [ -z "$PLUGIN_PATH" ] || printf '%s\n' "link|$PLUGIN_PATH|$BIN_PATH"
} > "$state_tmp" || die "could not write install record"
chmod 0600 "$state_tmp" || die "could not secure install record"

mv -f "$staged_bin" "$BIN_PATH" || die "could not install binary at $BIN_PATH"
staged_bin=""
if [ -n "$PLUGIN_PATH" ]; then
  mv -f "$staged_link" "$PLUGIN_PATH" || die "could not link Docker CLI plugin at $PLUGIN_PATH"
  staged_link=""
fi
removed=0
while IFS='|' read -r kind old_path expected; do
  if [ "$old_path" != "$BIN_PATH" ] && [ "$old_path" != "$PLUGIN_PATH" ]; then
    rm -f "$old_path" || die "could not remove verified obsolete $kind $old_path"
    removed=$((removed + 1))
  fi
done < "$tmp/old-entries"
mv -f "$state_tmp" "$STATE_FILE" || die "could not replace install record $STATE_FILE"
state_tmp=""

say "installed docker-extras from $asset to $BIN_PATH"
if [ -n "$PLUGIN_PATH" ]; then
  say "linked Docker CLI plugin $PLUGIN_PATH (try: docker $PLUGIN_NAME)"
else
  say "skipped the Docker CLI plugin (DOCKER_EXTRAS_NO_PLUGIN is set)"
fi
[ "$removed" -eq 0 ] || say "removed $removed obsolete file(s) after their ownership and checksum audit"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) say "warning: $INSTALL_DIR is not on PATH" ;;
esac
