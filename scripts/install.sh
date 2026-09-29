#!/bin/sh
# Plugin-only installer. Published as docker-extras-install.sh.
set -eu

say() { printf 'docker-extras-install: %s\n' "$*" >&2; }
die() { printf 'docker-extras-install: error: %s\n' "$*" >&2; exit 1; }

if [ "${DOCKER_EXTRAS_NO_PLUGIN+x}" = x ]; then
  die "DOCKER_EXTRAS_NO_PLUGIN is unsupported; this release installs only the Docker CLI plugin"
fi
if [ "${DOCKER_EXTRAS_INSTALL_DIR+x}" = x ]; then
  die "DOCKER_EXTRAS_INSTALL_DIR is unsupported; this release installs only into ~/.docker/cli-plugins"
fi

BASE_URL="${DOCKER_EXTRAS_BASE_URL:-https://github.com/procrastivity/docker-extras}"
VERSION="${DOCKER_EXTRAS_VERSION:-}"
PLUGIN_NAME="${DOCKER_EXTRAS_PLUGIN_NAME:-extras}"
PLUGIN_DIR="$HOME/.docker/cli-plugins"
PLUGIN_PATH="$PLUGIN_DIR/docker-$PLUGIN_NAME"
STATE_DIR="${XDG_STATE_HOME:-$HOME/.local/state}/docker-extras"
STATE_FILE="$STATE_DIR/install.tsv"

case "$PLUGIN_NAME" in
  [a-z]*) ;;
  *) die "invalid plugin name '$PLUGIN_NAME' (must start with a lowercase letter)" ;;
esac
case "$PLUGIN_NAME" in
  *[!a-z0-9]*) die "invalid plugin name '$PLUGIN_NAME' (use lowercase letters and digits only)" ;;
esac
case "$PLUGIN_DIR:$STATE_DIR" in
  *'|'*) die "install paths cannot contain the state-file delimiter '|'" ;;
esac

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
check_directory_chain "$PLUGIN_DIR"
check_directory_chain "$STATE_DIR"

if [ -n "$VERSION" ]; then
  dl="$BASE_URL/releases/download/$VERSION"
else
  dl="$BASE_URL/releases/latest/download"
fi

tmp=$(mktemp -d)
state_tmp=""
staged_plugin=""
cleanup() {
  rm -rf "$tmp"
  [ -z "$state_tmp" ] || rm -f "$state_tmp"
  [ -z "$staged_plugin" ] || rm -f "$staged_plugin"
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

tar -tzf "$tmp/$asset" > "$tmp/members" || die "could not list $asset"
awk '
  $0 == "plugin/docker-extras" { plugin++ ; next }
  $0 == "LICENSE" { license++ ; next }
  { exit 1 }
  END { if (plugin != 1 || license != 1) exit 1 }
' "$tmp/members" || die "$asset must contain only plugin/docker-extras and LICENSE"
tar -xzf "$tmp/$asset" -C "$tmp" plugin/docker-extras || die "could not extract plugin from $asset"
[ -f "$tmp/plugin/docker-extras" ] && [ ! -L "$tmp/plugin/docker-extras" ] ||
  die "$asset has no regular plugin/docker-extras executable"

# Audit the complete v1 Bash install record before any destination or state
# write. In particular, a changed legacy launcher blocks migration as a whole.
: > "$tmp/old-files"
old_install_dir=""
old_plugin_name=""
old_plugin_path=""
have_old_state=0
if [ -e "$STATE_FILE" ] || [ -L "$STATE_FILE" ]; then
  [ -f "$STATE_FILE" ] && [ ! -L "$STATE_FILE" ] ||
    die "refusing malformed or symlinked install record $STATE_FILE"
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
        [ "$old_path" = "$old_install_dir/$old_base" ] && [ "$old_base" != docker-extras- ] ||
          die "legacy file path is outside the recorded tool directory: $old_path"
        ;;
      "$old_plugin_path")
        [ -n "$old_plugin_path" ] || die "legacy record has an invalid plugin file entry"
        ;;
      *) die "legacy file path is outside the recorded installation: $old_path" ;;
    esac
    [ -f "$old_path" ] && [ ! -L "$old_path" ] ||
      die "refusing missing, non-regular, or symlinked legacy file: $old_path"
    actual=$(hash_file "$old_path") || die "could not hash legacy file: $old_path"
    [ "$actual" = "$expected" ] || die "refusing modified legacy file: $old_path"
  done < "$tmp/old-files"
  if [ -n "$old_plugin_path" ] &&
     ! awk -F '|' -v path="$old_plugin_path" '$1 == "file" && $2 == path { found++ } END { exit found != 1 }' "$STATE_FILE"; then
    die "legacy plugin path is not fully recorded in $STATE_FILE"
  fi
  have_old_state=1
fi

# A destination can be replaced only when the full old state audit proved it
# is this installer's existing plugin. Every other file or symlink is foreign.
if [ -e "$PLUGIN_PATH" ] || [ -L "$PLUGIN_PATH" ]; then
  if [ "$have_old_state" -ne 1 ] || [ "$old_plugin_path" != "$PLUGIN_PATH" ]; then
    die "refusing to overwrite existing Docker CLI plugin $PLUGIN_PATH"
  fi
fi

# Stage beside the destination so the final rename is atomic. The old state is
# not replaced until every obsolete, verified v1 file has been removed.
mkdir -p "$PLUGIN_DIR" "$STATE_DIR"
staged_plugin=$(mktemp "$PLUGIN_DIR/.docker-$PLUGIN_NAME.XXXXXX") || die "could not stage plugin in $PLUGIN_DIR"
cp "$tmp/plugin/docker-extras" "$staged_plugin" || die "could not stage plugin"
chmod 0755 "$staged_plugin" || die "could not set plugin permissions"
plugin_hash=$(hash_file "$staged_plugin") || die "could not hash staged plugin"
state_tmp=$(mktemp "$STATE_DIR/.install.XXXXXX") || die "could not stage install record"
{
  printf '%s\n' 'version|1'
  printf '%s\n' "install_dir|$PLUGIN_DIR"
  printf '%s\n' "plugin_name|$PLUGIN_NAME"
  printf '%s\n' "plugin_path|$PLUGIN_PATH"
  printf '%s\n' "file|$PLUGIN_PATH|$plugin_hash"
} > "$state_tmp" || die "could not write install record"
chmod 0600 "$state_tmp" || die "could not secure install record"

mv -f "$staged_plugin" "$PLUGIN_PATH" || die "could not install plugin at $PLUGIN_PATH"
staged_plugin=""
if [ "$have_old_state" -eq 1 ]; then
  while IFS='|' read -r old_path expected; do
    if [ "$old_path" != "$PLUGIN_PATH" ]; then
      rm -f "$old_path" || die "could not remove verified obsolete file $old_path"
    fi
  done < "$tmp/old-files"
fi
mv -f "$state_tmp" "$STATE_FILE" || die "could not replace install record $STATE_FILE"
state_tmp=""

say "installed docker $PLUGIN_NAME from $asset to $PLUGIN_PATH"
say "the legacy Bash files were removed only after their complete ownership and checksum audit"
