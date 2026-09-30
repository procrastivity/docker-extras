#!/bin/sh
# Removes only files listed in the checksum-verified installer ownership record.
set -eu

say() { printf 'docker-extras-uninstall: %s\n' "$*" >&2; }
die() { printf 'docker-extras-uninstall: error: %s\n' "$*" >&2; exit 1; }

STATE_DIR="${XDG_STATE_HOME:-$HOME/.local/state}/docker-extras"
STATE_FILE="$STATE_DIR/install.tsv"

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
check_directory_chain "$STATE_DIR"

if [ ! -f "$STATE_FILE" ] || [ -L "$STATE_FILE" ]; then
  die "no regular install record found at $STATE_FILE; refusing to guess what to remove"
fi
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
trap 'rm -f "$files"' EXIT
trap 'rm -f "$files"; exit 1' HUP INT TERM

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

install_dir=$(awk -F '|' '$1 == "install_dir" { print $2 }' "$STATE_FILE")
plugin_name=$(awk -F '|' '$1 == "plugin_name" { print $2 }' "$STATE_FILE")
plugin_path=$(awk -F '|' '$1 == "plugin_path" { print $2 }' "$STATE_FILE")
case "$install_dir" in
  /*) ;;
  *) die "install record has a non-absolute tool directory" ;;
esac
[ "$install_dir" != / ] || die "install record has an unsafe tool directory"
case "/$install_dir/" in
  *"/../"*|*"/./"*) die "install record has a non-normalized tool directory" ;;
esac
check_directory_chain "$install_dir"
check_directory_chain "$HOME/.docker/cli-plugins"
case "$plugin_name" in
  [a-z]*) ;;
  *) die "install record has an invalid plugin name" ;;
esac
case "$plugin_name" in
  *[!a-z0-9]*) die "install record has an invalid plugin name" ;;
esac
if [ -n "$plugin_path" ] &&
   [ "$plugin_path" != "$HOME/.docker/cli-plugins/docker-$plugin_name" ]; then
  die "install record has an out-of-scope plugin path"
fi

awk -F '|' '$1 == "file" { print $2 "|" $3 }' "$STATE_FILE" > "$files"
count=0
while IFS='|' read -r path expected; do
  count=$((count + 1))
  case "$path" in
    "$install_dir"/docker-extras-*)
      base=${path##*/}
      if [ "$path" != "$install_dir/$base" ] || [ "$base" = docker-extras- ]; then
        die "refusing path outside the recorded tool directory: $path"
      fi
      ;;
    "$plugin_path") [ -n "$plugin_path" ] || die "malformed plugin entry in $STATE_FILE" ;;
    *) die "refusing path outside the recorded installation: $path" ;;
  esac
  if [ ! -f "$path" ] || [ -L "$path" ]; then
    die "refusing missing, non-regular, or symlinked file: $path"
  fi
  actual=$(hash_file "$path") || die "could not hash installed file: $path"
  [ "$actual" = "$expected" ] || die "refusing modified file: $path"
done < "$files"
[ "$count" -gt 0 ] || die "install record contains no files"
if [ -n "$plugin_path" ] &&
   ! awk -F '|' -v path="$plugin_path" '$1 == "file" && $2 == path { found++ } END { exit found != 1 }' "$STATE_FILE"; then
  die "plugin path is not fully recorded in $STATE_FILE"
fi

# Verify every owned file before making the first removal.
while IFS='|' read -r path expected; do
  rm -f "$path" || die "could not remove verified file: $path"
done < "$files"
rm -f "$STATE_FILE" || die "could not remove install record $STATE_FILE"
rmdir "$STATE_DIR" 2>/dev/null || true

say "removed verified installation files"
