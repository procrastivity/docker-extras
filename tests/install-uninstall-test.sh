#!/usr/bin/env bash
# Tests platform selection, install options, v1 and v2 record migration, and
# uninstall safety using a local file:// release.
set -euo pipefail

say() { printf 'install-uninstall-test: %s\n' "$*" >&2; }
fail() { say "FAIL: $*"; exit 1; }
require_file() { [ -f "$1" ] || fail "missing file: $1"; }
require_absent() {
  if [ -e "$1" ] || [ -L "$1" ]; then
    fail "still present: $1"
  fi
}
if command -v sha256sum >/dev/null 2>&1; then
  sha() { sha256sum "$1" | awk '{print $1}'; }
  sha_stdin() { sha256sum; }
  verify_manifest() { (cd "$dist_dir" && sha256sum -c SHA256SUMS); }
elif command -v shasum >/dev/null 2>&1; then
  sha() { shasum -a 256 "$1" | awk '{print $1}'; }
  sha_stdin() { shasum -a 256; }
  verify_manifest() { (cd "$dist_dir" && shasum -a 256 -c SHA256SUMS); }
else
  fail "sha256sum or shasum is required"
fi
repo_root=$(git rev-parse --show-toplevel)
dist_dir=${1:-$repo_root/dist}
case "$dist_dir" in /*) ;; *) dist_dir="$repo_root/$dist_dir" ;; esac
install_script="$repo_root/scripts/install.sh"
uninstall_script="$repo_root/scripts/uninstall.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

release="$work/releases/latest/download"
mkdir -p "$release"
assets=(
  docker-extras-linux-amd64.tar.gz
  docker-extras-linux-arm64.tar.gz
  docker-extras-darwin-amd64.tar.gz
  docker-extras-darwin-arm64.tar.gz
  docker-extras-install.sh
  docker-extras-uninstall.sh
)
for asset in "${assets[@]}"; do
  require_file "$dist_dir/$asset"
  cp "$dist_dir/$asset" "$release/$asset"
done
cp "$dist_dir/SHA256SUMS" "$release/SHA256SUMS"
verify_manifest
expected_assets=$(printf '%s\n' "${assets[@]}" | sort)
actual_assets=$(awk '{print $2}' "$dist_dir/SHA256SUMS" | sort)
[ "$actual_assets" = "$expected_assets" ] || fail "SHA256SUMS does not list exactly four archives and both installer scripts"

base_url="file://$work"
mockbin="$work/mockbin"
mkdir -p "$mockbin"
cat > "$mockbin/uname" <<'EOF'
#!/bin/sh
case "$1" in
  -s) printf '%s\n' "$TEST_UNAME_SYSTEM" ;;
  -m) printf '%s\n' "$TEST_UNAME_MACHINE" ;;
  *) exit 2 ;;
esac
EOF
chmod 0755 "$mockbin/uname"


run_install() {
  local home=$1 state=$2
  shift 2
  env HOME="$home" XDG_STATE_HOME="$state" DOCKER_EXTRAS_BASE_URL="$base_url" "$@" sh "$install_script"
}
run_uninstall() {
  local home=$1 state=$2
  shift 2
  env HOME="$home" XDG_STATE_HOME="$state" "$@" sh "$uninstall_script"
}
require_link() {
  [ -L "$1" ] || fail "not a symlink: $1"
  [ "$(readlink "$1")" = "$2" ] || fail "$1 points at $(readlink "$1"), want $2"
}
entry_count() { awk -F '|' -v k="$1" '$1 == k { n++ } END { print n+0 }' "$2"; }

say "all four uname targets install the matching verified binary on PATH and link the plugin"
for target in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64; do
  case "$target" in
    linux-amd64) system=Linux; machine=x86_64 ;;
    linux-arm64) system=Linux; machine=aarch64 ;;
    darwin-amd64) system=Darwin; machine=x86_64 ;;
    darwin-arm64) system=Darwin; machine=arm64 ;;
  esac
  home="$work/home-$target"
  state="$work/state-$target"
  run_install "$home" "$state" DOCKER_EXTRAS_PLUGIN_NAME=tools TEST_UNAME_SYSTEM="$system" \
    TEST_UNAME_MACHINE="$machine" PATH="$mockbin:$PATH" >/dev/null
  bin="$home/.local/bin/docker-extras"
  plugin="$home/.docker/cli-plugins/docker-tools"
  state_file="$state/docker-extras/install.tsv"
  archive="$release/docker-extras-$target.tar.gz"
  members=$(tar -tzf "$archive" | sort)
  [ "$members" = $'LICENSE\nplugin/docker-extras' ] || fail "$target archive has unexpected members: $members"
  expected_hash=$(tar -xOf "$archive" plugin/docker-extras | sha_stdin | awk '{print $1}')
  require_file "$bin"
  [ ! -L "$bin" ] || fail "$target binary is a symlink"
  [ -x "$bin" ] || fail "$target binary is not executable"
  [ "$(sha "$bin")" = "$expected_hash" ] || fail "$target installed the wrong archive payload"
  require_link "$plugin" "$bin"
  build_info=$(go version -m "$bin") || fail "$target has no readable Go build metadata"
  os=${target%-*}
  arch=${target#*-}
  printf '%s\n' "$build_info" | grep -Fqx $'\tbuild\tGOOS='"$os" || fail "$target contains a binary for the wrong GOOS"
  printf '%s\n' "$build_info" | grep -Fqx $'\tbuild\tGOARCH='"$arch" || fail "$target contains a binary for the wrong GOARCH"
  grep -Fqx 'version|2' "$state_file" || fail "$target record is not version 2"
  grep -Fqx "bin_path|$bin" "$state_file" || fail "$target binary path was not recorded"
  grep -Fqx "plugin_path|$plugin" "$state_file" || fail "$target plugin path was not recorded"
  grep -Fqx "file|$bin|$expected_hash" "$state_file" || fail "$target binary checksum was not recorded"
  grep -Fqx "link|$plugin|$bin" "$state_file" || fail "$target plugin link was not recorded"
  require_absent "$home/.local/bin/docker-extras-volume-seed"
  require_absent "$home/.local/bin/docker-tools"
  run_uninstall "$home" "$state" >/dev/null
  require_absent "$bin"
  require_absent "$plugin"
  require_absent "$state_file"
done

say "a downloaded archive with a checksum mismatch is rejected before writes"
badbase="$work/bad-release"
badrelease="$badbase/releases/latest/download"
mkdir -p "$badrelease"
cp "$release/docker-extras-linux-amd64.tar.gz" "$badrelease/docker-extras-linux-amd64.tar.gz"
cp "$release/SHA256SUMS" "$badrelease/SHA256SUMS"
printf 'tamper' >> "$badrelease/docker-extras-linux-amd64.tar.gz"
if env HOME="$work/home-bad-checksum" XDG_STATE_HOME="$work/state-bad-checksum" \
   DOCKER_EXTRAS_BASE_URL="file://$badbase" TEST_UNAME_SYSTEM=Linux TEST_UNAME_MACHINE=x86_64 \
   PATH="$mockbin:$PATH" sh "$install_script" >"$work/bad-checksum.log" 2>&1; then
  fail "installer accepted a checksum-mismatched archive"
fi
grep -Fq 'checksum verification failed' "$work/bad-checksum.log" || fail "checksum refusal was unclear"
require_absent "$work/home-bad-checksum/.local/bin/docker-extras"
require_absent "$work/home-bad-checksum/.docker/cli-plugins/docker-extras"
require_absent "$work/state-bad-checksum/docker-extras/install.tsv"

say "unsupported targets, invalid names, and unsafe install directories fail before writes"
home="$work/home-unsupported"
state="$work/state-unsupported"
if run_install "$home" "$state" TEST_UNAME_SYSTEM=FreeBSD TEST_UNAME_MACHINE=x86_64 PATH="$mockbin:$PATH" \
   >"$work/unsupported.log" 2>&1; then
  fail "installer accepted unsupported FreeBSD target"
fi
grep -Fq 'unsupported platform FreeBSD/x86_64' "$work/unsupported.log" || fail "unsupported target refusal was unclear"
require_absent "$home/.local/bin/docker-extras"
require_absent "$home/.docker/cli-plugins/docker-extras"
require_absent "$state/docker-extras/install.tsv"
if run_install "$work/home-invalid-name" "$work/state-invalid-name" DOCKER_EXTRAS_PLUGIN_NAME=bad-name \
   >/dev/null 2>&1; then
  fail "installer accepted a hyphenated plugin name"
fi
require_absent "$work/home-invalid-name/.local/bin/docker-extras"
require_absent "$work/home-invalid-name/.docker/cli-plugins/docker-bad-name"
require_absent "$work/state-invalid-name/docker-extras/install.tsv"
mkdir -p "$work/real-bin-parent/bin"
ln -s "$work/real-bin-parent" "$work/linked-bin-parent"
for case_name in relative dotdot trailing delimiter root symlink plugindir; do
  home="$work/home-dir-$case_name"
  state="$work/state-dir-$case_name"
  case "$case_name" in
    relative) dir="relative/bin" ;;
    dotdot) dir="$home/x/../bin" ;;
    trailing) dir="$home/bin/" ;;
    delimiter) dir="$home/b|in" ;;
    root) dir="/" ;;
    symlink) dir="$work/linked-bin-parent/bin" ;;
    plugindir) dir="$home/.docker/cli-plugins" ;;
  esac
  if run_install "$home" "$state" DOCKER_EXTRAS_INSTALL_DIR="$dir" >"$work/dir-$case_name.log" 2>&1; then
    fail "installer accepted unsafe install directory ($case_name): $dir"
  fi
  require_absent "$home/.docker/cli-plugins/docker-extras"
  require_absent "$state/docker-extras/install.tsv"
done
require_absent "$work/real-bin-parent/bin/docker-extras"
grep -Fq 'symlinked installation directory' "$work/dir-symlink.log" || fail "symlinked install directory refusal was unclear"

say "DOCKER_EXTRAS_NO_PLUGIN installs only the PATH binary; an empty value still links the plugin"
home="$work/home-no-plugin"
state="$work/state-no-plugin"
run_install "$home" "$state" DOCKER_EXTRAS_NO_PLUGIN=1 >/dev/null
bin="$home/.local/bin/docker-extras"
state_file="$state/docker-extras/install.tsv"
require_file "$bin"
require_absent "$home/.docker/cli-plugins/docker-extras"
grep -Fqx 'plugin_path|' "$state_file" || fail "no-plugin record has a plugin path"
[ "$(entry_count link "$state_file")" -eq 0 ] || fail "no-plugin record lists a link"
run_uninstall "$home" "$state" >/dev/null
require_absent "$bin"
require_absent "$state_file"
home="$work/home-empty-no-plugin"
state="$work/state-empty-no-plugin"
run_install "$home" "$state" DOCKER_EXTRAS_NO_PLUGIN= >/dev/null
require_link "$home/.docker/cli-plugins/docker-extras" "$home/.local/bin/docker-extras"
run_uninstall "$home" "$state" >/dev/null

say "a custom install directory is honored and warned about when not on PATH"
home="$work/home-custom-dir"
state="$work/state-custom-dir"
custom="$work/custom tool bin"
run_install "$home" "$state" DOCKER_EXTRAS_INSTALL_DIR="$custom" >/dev/null 2>"$work/custom-dir.log"
require_file "$custom/docker-extras"
require_link "$home/.docker/cli-plugins/docker-extras" "$custom/docker-extras"
require_absent "$home/.local/bin/docker-extras"
grep -Fq "warning: $custom is not on PATH" "$work/custom-dir.log" || fail "installer did not warn about PATH"
run_uninstall "$home" "$state" >/dev/null
require_absent "$custom/docker-extras"
require_absent "$home/.docker/cli-plugins/docker-extras"

create_legacy_install() {
  local root=$1 old_name=${2:-extras}
  LEGACY_HOME="$root/home"
  LEGACY_STATE="$root/state"
  LEGACY_BIN="$root/old local bin"
  LEGACY_PLUGIN="$LEGACY_HOME/.docker/cli-plugins/docker-$old_name"
  LEGACY_STATE_FILE="$LEGACY_STATE/docker-extras/install.tsv"
  mkdir -p "$LEGACY_BIN" "$(dirname "$LEGACY_PLUGIN")" "$(dirname "$LEGACY_STATE_FILE")"
  printf '#!/bin/sh\necho installer-owned-old-tool\n' > "$LEGACY_BIN/docker-extras-volume-seed"
  printf '#!/bin/sh\necho installer-owned-old-plugin\n' > "$LEGACY_PLUGIN"
  chmod 0755 "$LEGACY_BIN/docker-extras-volume-seed" "$LEGACY_PLUGIN"
  {
    printf '%s\n' 'version|1'
    printf 'install_dir|%s\n' "$LEGACY_BIN"
    printf 'plugin_name|%s\n' "$old_name"
    printf 'plugin_path|%s\n' "$LEGACY_PLUGIN"
    printf 'file|%s|%s\n' "$LEGACY_BIN/docker-extras-volume-seed" "$(sha "$LEGACY_BIN/docker-extras-volume-seed")"
    printf 'file|%s|%s\n' "$LEGACY_PLUGIN" "$(sha "$LEGACY_PLUGIN")"
  } > "$LEGACY_STATE_FILE"
}

say "clean v1 Bash migration removes verified Bash files and records the PATH binary and link"
create_legacy_install "$work/legacy-success"
run_install "$LEGACY_HOME" "$LEGACY_STATE" DOCKER_EXTRAS_PLUGIN_NAME=tools >/dev/null
new_bin="$LEGACY_HOME/.local/bin/docker-extras"
new_plugin="$LEGACY_HOME/.docker/cli-plugins/docker-tools"
require_file "$new_bin"
require_link "$new_plugin" "$new_bin"
require_absent "$LEGACY_BIN/docker-extras-volume-seed"
require_absent "$LEGACY_HOME/.docker/cli-plugins/docker-extras"
grep -Fqx 'version|2' "$LEGACY_STATE_FILE" || fail "migration did not write a version 2 record"
grep -Fqx 'plugin_name|tools' "$LEGACY_STATE_FILE" || fail "migration retained old plugin name"
grep -Fqx "plugin_path|$new_plugin" "$LEGACY_STATE_FILE" || fail "migration did not record new plugin path"
[ "$(entry_count file "$LEGACY_STATE_FILE")" -eq 1 ] || fail "migrated record still lists old Bash files"
[ "$(entry_count link "$LEGACY_STATE_FILE")" -eq 1 ] || fail "migrated record does not list the plugin link"
run_uninstall "$LEGACY_HOME" "$LEGACY_STATE" >/dev/null
require_absent "$new_bin"
require_absent "$new_plugin"
require_absent "$LEGACY_STATE_FILE"

say "modified old standalone or plugin blocks all migration writes"
for modified in tool plugin; do
  create_legacy_install "$work/legacy-modified-$modified"
  if [ "$modified" = tool ]; then file="$LEGACY_BIN/docker-extras-volume-seed"; else file="$LEGACY_PLUGIN"; fi
  printf 'user edit\n' >> "$file"
  cp "$LEGACY_BIN/docker-extras-volume-seed" "$work/old-tool.snapshot"
  cp "$LEGACY_PLUGIN" "$work/old-plugin.snapshot"
  cp "$LEGACY_STATE_FILE" "$work/old-state.snapshot"
  if run_install "$LEGACY_HOME" "$LEGACY_STATE" DOCKER_EXTRAS_PLUGIN_NAME=tools \
     >"$work/modified-$modified.log" 2>&1; then
    fail "migration accepted modified old $modified"
  fi
  cmp -s "$LEGACY_BIN/docker-extras-volume-seed" "$work/old-tool.snapshot" || fail "old tool changed on refusal"
  cmp -s "$LEGACY_PLUGIN" "$work/old-plugin.snapshot" || fail "old plugin changed on refusal"
  cmp -s "$LEGACY_STATE_FILE" "$work/old-state.snapshot" || fail "old state changed on refusal"
  require_absent "$LEGACY_HOME/.docker/cli-plugins/docker-tools"
  require_absent "$LEGACY_HOME/.local/bin/docker-extras"
done

say "symlinked and out-of-scope v1 entries fail the complete audit before writes"
for invalid in symlink scope; do
  create_legacy_install "$work/legacy-invalid-$invalid"
  if [ "$invalid" = symlink ]; then
    target="$work/legacy-invalid-$invalid/target-copy"
    cp "$LEGACY_BIN/docker-extras-volume-seed" "$target"
    rm "$LEGACY_BIN/docker-extras-volume-seed"
    ln -s "$target" "$LEGACY_BIN/docker-extras-volume-seed"
    link_before=$(readlink "$LEGACY_BIN/docker-extras-volume-seed")
  else
    external="$work/legacy-invalid-$invalid/outside-owned-file"
    printf 'not in install scope\n' > "$external"
    {
      printf '%s\n' 'version|1'
      printf 'install_dir|%s\n' "$LEGACY_BIN"
      printf '%s\n' 'plugin_name|extras'
      printf 'plugin_path|%s\n' "$LEGACY_PLUGIN"
      printf 'file|%s|%s\n' "$external" "$(sha "$external")"
      printf 'file|%s|%s\n' "$LEGACY_PLUGIN" "$(sha "$LEGACY_PLUGIN")"
    } > "$LEGACY_STATE_FILE"
    cp "$external" "$work/outside.snapshot"
  fi
  cp "$LEGACY_STATE_FILE" "$work/invalid-state.snapshot"
  cp "$LEGACY_PLUGIN" "$work/invalid-plugin.snapshot"
  if run_install "$LEGACY_HOME" "$LEGACY_STATE" DOCKER_EXTRAS_PLUGIN_NAME=tools \
     >"$work/invalid-$invalid.log" 2>&1; then
    fail "migration accepted invalid $invalid legacy entry"
  fi
  cmp -s "$LEGACY_STATE_FILE" "$work/invalid-state.snapshot" || fail "$invalid state bytes changed on refusal"
  cmp -s "$LEGACY_PLUGIN" "$work/invalid-plugin.snapshot" || fail "$invalid legacy plugin changed on refusal"
  require_absent "$LEGACY_HOME/.docker/cli-plugins/docker-tools"
  require_absent "$LEGACY_HOME/.local/bin/docker-extras"
  if [ "$invalid" = symlink ]; then
    [ "$(readlink "$LEGACY_BIN/docker-extras-volume-seed")" = "$link_before" ] || fail "legacy symlink changed on refusal"
  else
    cmp -s "$external" "$work/outside.snapshot" || fail "out-of-scope file changed on refusal"
  fi
done

say "symlinked legacy directory refuses migration and uninstall without touching foreign files"
create_legacy_install "$work/legacy-link-parent"
foreign_bin="$work/foreign-legacy-bin"
mv "$LEGACY_BIN" "$foreign_bin"
ln -s "$foreign_bin" "$LEGACY_BIN"
cp "$foreign_bin/docker-extras-volume-seed" "$work/foreign-tool.snapshot"
cp "$LEGACY_PLUGIN" "$work/legacy-link-plugin.snapshot"
cp "$LEGACY_STATE_FILE" "$work/legacy-link-state.snapshot"
if run_install "$LEGACY_HOME" "$LEGACY_STATE" DOCKER_EXTRAS_PLUGIN_NAME=tools \
   >"$work/legacy-link-install.log" 2>&1; then
  fail "migration followed a symlinked legacy tool directory"
fi
if run_uninstall "$LEGACY_HOME" "$LEGACY_STATE" >"$work/legacy-link-uninstall.log" 2>&1; then
  fail "uninstall followed a symlinked legacy tool directory"
fi
grep -Fq 'symlinked installation directory' "$work/legacy-link-install.log" || fail "migration did not report symlinked parent"
grep -Fq 'symlinked installation directory' "$work/legacy-link-uninstall.log" || fail "uninstall did not report symlinked parent"
cmp -s "$foreign_bin/docker-extras-volume-seed" "$work/foreign-tool.snapshot" || fail "foreign tool changed"
cmp -s "$LEGACY_PLUGIN" "$work/legacy-link-plugin.snapshot" || fail "legacy plugin changed"
cmp -s "$LEGACY_STATE_FILE" "$work/legacy-link-state.snapshot" || fail "legacy state changed"
require_absent "$LEGACY_HOME/.docker/cli-plugins/docker-tools"
require_absent "$LEGACY_HOME/.local/bin/docker-extras"

# The plugin-only Go release reused the v1 record layout with install_dir set to
# the plugin directory and one regular plugin file.
create_plugin_only_install() {
  local root=$1 old_name=${2:-extras}
  GO1_HOME="$root/home"
  GO1_STATE="$root/state"
  GO1_PLUGIN_DIR="$GO1_HOME/.docker/cli-plugins"
  GO1_PLUGIN="$GO1_PLUGIN_DIR/docker-$old_name"
  GO1_STATE_FILE="$GO1_STATE/docker-extras/install.tsv"
  mkdir -p "$GO1_PLUGIN_DIR" "$(dirname "$GO1_STATE_FILE")"
  printf '#!/bin/sh\necho installer-owned-go-plugin\n' > "$GO1_PLUGIN"
  chmod 0755 "$GO1_PLUGIN"
  {
    printf '%s\n' 'version|1'
    printf 'install_dir|%s\n' "$GO1_PLUGIN_DIR"
    printf 'plugin_name|%s\n' "$old_name"
    printf 'plugin_path|%s\n' "$GO1_PLUGIN"
    printf 'file|%s|%s\n' "$GO1_PLUGIN" "$(sha "$GO1_PLUGIN")"
  } > "$GO1_STATE_FILE"
}

say "plugin-only Go migration replaces the regular plugin file with a link to the PATH binary"
create_plugin_only_install "$work/go1-same-name"
run_install "$GO1_HOME" "$GO1_STATE" >/dev/null
require_file "$GO1_HOME/.local/bin/docker-extras"
require_link "$GO1_PLUGIN" "$GO1_HOME/.local/bin/docker-extras"
grep -Fqx 'version|2' "$GO1_STATE_FILE" || fail "plugin-only migration did not write a version 2 record"
run_uninstall "$GO1_HOME" "$GO1_STATE" >/dev/null
require_absent "$GO1_PLUGIN"
require_absent "$GO1_HOME/.local/bin/docker-extras"

create_plugin_only_install "$work/go1-no-plugin"
run_install "$GO1_HOME" "$GO1_STATE" DOCKER_EXTRAS_NO_PLUGIN=1 >/dev/null
require_file "$GO1_HOME/.local/bin/docker-extras"
require_absent "$GO1_PLUGIN"

create_plugin_only_install "$work/go1-modified"
printf 'user edit\n' >> "$GO1_PLUGIN"
cp "$GO1_PLUGIN" "$work/go1-plugin.snapshot"
if run_install "$GO1_HOME" "$GO1_STATE" >/dev/null 2>&1; then
  fail "plugin-only migration accepted a modified plugin"
fi
cmp -s "$GO1_PLUGIN" "$work/go1-plugin.snapshot" || fail "modified plugin-only file changed on refusal"
require_absent "$GO1_HOME/.local/bin/docker-extras"

say "re-install is idempotent; renames and moves across upgrades remove only the old owned entries"
home="$work/home-upgrade"
state="$work/state-upgrade"
state_file="$state/docker-extras/install.tsv"
run_install "$home" "$state" >/dev/null
cp "$state_file" "$work/upgrade-first.snapshot"
run_install "$home" "$state" >/dev/null
cmp -s "$state_file" "$work/upgrade-first.snapshot" || fail "re-install changed the record"
require_link "$home/.docker/cli-plugins/docker-extras" "$home/.local/bin/docker-extras"
run_install "$home" "$state" DOCKER_EXTRAS_PLUGIN_NAME=tools >/dev/null
require_absent "$home/.docker/cli-plugins/docker-extras"
require_link "$home/.docker/cli-plugins/docker-tools" "$home/.local/bin/docker-extras"
moved="$work/moved bin"
run_install "$home" "$state" DOCKER_EXTRAS_PLUGIN_NAME=tools DOCKER_EXTRAS_INSTALL_DIR="$moved" >/dev/null 2>&1
require_absent "$home/.local/bin/docker-extras"
require_file "$moved/docker-extras"
require_link "$home/.docker/cli-plugins/docker-tools" "$moved/docker-extras"
run_install "$home" "$state" DOCKER_EXTRAS_INSTALL_DIR="$moved" DOCKER_EXTRAS_NO_PLUGIN=1 >/dev/null 2>&1
require_absent "$home/.docker/cli-plugins/docker-tools"
require_file "$moved/docker-extras"
run_uninstall "$home" "$state" >/dev/null
require_absent "$moved/docker-extras"
require_absent "$state_file"

say "symlinked fresh plugin or state ancestors refuse before destination writes"
home="$work/home-link-plugin"
state="$work/state-link-plugin"
foreign_plugin_dir="$work/foreign-plugin-dir"
mkdir -p "$home" "$foreign_plugin_dir/cli-plugins"
printf 'foreign marker\n' > "$foreign_plugin_dir/marker"
cp "$foreign_plugin_dir/marker" "$work/foreign-marker.snapshot"
ln -s "$foreign_plugin_dir" "$home/.docker"
if run_install "$home" "$state" >"$work/link-plugin-install.log" 2>&1; then
  fail "installer followed symlinked Docker plugin parent"
fi
grep -Fq 'symlinked installation directory' "$work/link-plugin-install.log" || fail "plugin parent refusal was unclear"
cmp -s "$foreign_plugin_dir/marker" "$work/foreign-marker.snapshot" || fail "foreign plugin directory changed"
require_absent "$foreign_plugin_dir/cli-plugins/docker-extras"
require_absent "$home/.local/bin/docker-extras"
require_absent "$state/docker-extras/install.tsv"

home="$work/home-link-state"
state="$work/state-link-state"
foreign_state="$work/foreign-state-dir"
mkdir -p "$foreign_state" "$(dirname "$state")"
printf 'foreign marker\n' > "$foreign_state/marker"
cp "$foreign_state/marker" "$work/foreign-state.snapshot"
ln -s "$foreign_state" "$state"
if run_install "$home" "$state" >"$work/link-state-install.log" 2>&1; then
  fail "installer followed symlinked state parent"
fi
grep -Fq 'symlinked installation directory' "$work/link-state-install.log" || fail "state parent refusal was unclear"
cmp -s "$foreign_state/marker" "$work/foreign-state.snapshot" || fail "foreign state directory changed"
require_absent "$foreign_state/docker-extras/install.tsv"
require_absent "$home/.local/bin/docker-extras"
require_absent "$home/.docker/cli-plugins/docker-extras"

say "unowned destination collisions are preserved"
for collision in plugin-file plugin-link bin-file; do
  home="$work/home-collision-$collision"
  state="$work/state-collision-$collision"
  plugin="$home/.docker/cli-plugins/docker-tools"
  bin="$home/.local/bin/docker-extras"
  mkdir -p "$(dirname "$plugin")" "$(dirname "$bin")"
  case "$collision" in
    plugin-file) printf 'user-owned plugin\n' > "$plugin"; dest="$plugin" ;;
    plugin-link) printf 'user-owned target\n' > "$work/user-target"; ln -s "$work/user-target" "$plugin"; dest="$plugin" ;;
    bin-file) printf 'user-owned binary\n' > "$bin"; dest="$bin" ;;
  esac
  [ -L "$dest" ] && before=$(readlink "$dest") || before=$(sha "$dest")
  if run_install "$home" "$state" DOCKER_EXTRAS_PLUGIN_NAME=tools >/dev/null 2>&1; then
    fail "installer overwrote an unowned $collision collision"
  fi
  [ -L "$dest" ] && after=$(readlink "$dest") || after=$(sha "$dest")
  [ "$before" = "$after" ] || fail "$collision collision changed"
  require_absent "$state/docker-extras/install.tsv"
  [ "$collision" = bin-file ] || require_absent "$bin"
  [ "$collision" != bin-file ] || require_absent "$plugin"
done

say "plugin metadata, help, dispatch, and completion work through the link and on PATH"
case "$(uname -s):$(uname -m)" in
  Linux:x86_64|Linux:aarch64|Darwin:x86_64|Darwin:arm64|Darwin:aarch64) ;;
  *) fail "test runner uses unsupported platform $(uname -s)/$(uname -m)" ;;
esac
home="$work/home-docker-custom"
state="$work/state-docker-custom"
run_install "$home" "$state" DOCKER_EXTRAS_PLUGIN_NAME=tools >/dev/null
bin="$home/.local/bin/docker-extras"
plugin="$home/.docker/cli-plugins/docker-tools"
require_link "$plugin" "$bin"
metadata=$("$plugin" docker-cli-plugin-metadata)
[[ "$metadata" == *'"SchemaVersion":"0.1.0"'* ]] || fail "custom metadata handshake failed: $metadata"
expected_version=$(git describe --tags --match 'v[0-9]*' --always --dirty)
expected_version=${expected_version#v}
[[ "$metadata" == *"\"Version\":\"$expected_version\""* ]] || fail "plugin build version metadata is wrong: $metadata"
direct_help=$(PATH="$(dirname "$bin"):$PATH" docker-extras volume seed capture --help) ||
  fail "docker-extras on PATH did not run"
[[ "$direct_help" == *"docker-extras volume seed capture"* ]] || fail "PATH help shows the wrong command: $direct_help"
if output=$(PATH="$(dirname "$bin"):$PATH" docker-extras volume seed capture 2>&1); then
  fail "PATH dispatch accepted capture without --from-volume"
fi
[[ "$output" == *"capture requires --from-volume"* ]] || fail "PATH dispatch returned unexpected output: $output"
if ! command -v docker >/dev/null 2>&1; then
  if [ "${REQUIRE_DOCKER:-0}" = 1 ]; then fail "Docker CLI is required by package gate"; fi
  say "Docker CLI absent; skipping actual Docker command-dispatch check"
else
  (
    export HOME="$home"
    unset DOCKER_CONFIG DOCKER_CONTEXT DOCKER_HOST
    docker tools --help | grep -Fq 'volume' || fail "Docker did not load custom plugin help"
    if output=$(docker tools volume seed capture 2>&1); then
      fail "custom Docker dispatch accepted capture without --from-volume"
    fi
    [[ "$output" == *"capture requires --from-volume"* ]] || fail "custom dispatch returned unexpected output: $output"
    root_completion=$(docker __complete tools "")
    printf '%s\n' "$root_completion" | awk -F '\t' '$1 == "volume" { found=1 } END { exit !found }' ||
      fail "root completion omitted volume: $root_completion"
    printf '%s\n' "$root_completion" | grep -Fxq ':4' || fail "root completion omitted :4"
    leaf_completion=$(docker __complete tools volume seed "")
    for candidate in capture restore; do
      printf '%s\n' "$leaf_completion" | awk -F '\t' -v c="$candidate" '$1 == c { found=1 } END { exit !found }' ||
        fail "leaf completion omitted $candidate: $leaf_completion"
    done
    printf '%s\n' "$leaf_completion" | grep -Fxq ':4' || fail "leaf completion omitted :4"
  )
fi
run_uninstall "$home" "$state" >/dev/null
require_absent "$bin"
require_absent "$plugin"
require_absent "$state/docker-extras/install.tsv"

say "uninstall refuses a modified binary or a retargeted link and removes nothing"
for tamper in binary link; do
  home="$work/home-tamper-$tamper"
  state="$work/state-tamper-$tamper"
  run_install "$home" "$state" DOCKER_EXTRAS_PLUGIN_NAME=tools >/dev/null
  bin="$home/.local/bin/docker-extras"
  plugin="$home/.docker/cli-plugins/docker-tools"
  state_file="$state/docker-extras/install.tsv"
  if [ "$tamper" = binary ]; then
    printf 'user edit\n' >> "$bin"
  else
    printf 'user-owned target\n' > "$work/retarget"
    ln -sfn "$work/retarget" "$plugin"
  fi
  bin_before=$(sha "$bin")
  link_before=$(readlink "$plugin")
  cp "$state_file" "$work/tamper-state.snapshot"
  if run_uninstall "$home" "$state" >/dev/null 2>&1; then
    fail "uninstaller accepted a tampered $tamper"
  fi
  if run_install "$home" "$state" DOCKER_EXTRAS_PLUGIN_NAME=tools >/dev/null 2>&1; then
    fail "installer upgraded over a tampered $tamper"
  fi
  [ "$(sha "$bin")" = "$bin_before" ] || fail "binary changed on $tamper refusal"
  [ "$(readlink "$plugin")" = "$link_before" ] || fail "link changed on $tamper refusal"
  cmp -s "$state_file" "$work/tamper-state.snapshot" || fail "state changed on $tamper refusal"
done

say "uninstall refuses symlinked state ancestors without removing an installed binary or link"
home="$work/home-uninstall-link-state"
state="$work/state-uninstall-link-state"
run_install "$home" "$state" >/dev/null
bin="$home/.local/bin/docker-extras"
plugin="$home/.docker/cli-plugins/docker-extras"
foreign_state="$work/foreign-uninstall-state"
mv "$state" "$foreign_state"
ln -s "$foreign_state" "$state"
cp "$bin" "$work/uninstall-link-bin.snapshot"
cp "$foreign_state/docker-extras/install.tsv" "$work/uninstall-link-state.snapshot"
if run_uninstall "$home" "$state" >"$work/link-state-uninstall.log" 2>&1; then
  fail "uninstaller followed symlinked state parent"
fi
grep -Fq 'symlinked installation directory' "$work/link-state-uninstall.log" || fail "state parent uninstall refusal was unclear"
cmp -s "$bin" "$work/uninstall-link-bin.snapshot" || fail "binary changed on state parent refusal"
require_link "$plugin" "$bin"
cmp -s "$foreign_state/docker-extras/install.tsv" "$work/uninstall-link-state.snapshot" || fail "foreign state changed on refusal"

say "all platform, option, migration, upgrade, dispatch, and uninstall checks passed"
