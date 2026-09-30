#!/usr/bin/env bash
# Tests platform selection and v1 Bash migration using a local file:// release.
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

say "all four uname targets select only their matching verified archive"
for target in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64; do
  case "$target" in
    linux-amd64) system=Linux; machine=x86_64 ;;
    linux-arm64) system=Linux; machine=aarch64 ;;
    darwin-amd64) system=Darwin; machine=x86_64 ;;
    darwin-arm64) system=Darwin; machine=arm64 ;;
  esac
  home="$work/home-$target"
  state="$work/state-$target"
  env HOME="$home" XDG_STATE_HOME="$state" DOCKER_EXTRAS_BASE_URL="$base_url" \
    DOCKER_EXTRAS_PLUGIN_NAME=tools TEST_UNAME_SYSTEM="$system" TEST_UNAME_MACHINE="$machine" \
    PATH="$mockbin:$PATH" sh "$install_script" >/dev/null
  plugin="$home/.docker/cli-plugins/docker-tools"
  archive="$release/docker-extras-$target.tar.gz"
  members=$(tar -tzf "$archive" | sort)
  [ "$members" = $'LICENSE\nplugin/docker-extras' ] || fail "$target archive has unexpected members: $members"
  expected_hash=$(tar -xOf "$archive" plugin/docker-extras | sha_stdin | awk '{print $1}')
  require_file "$plugin"
  [ -x "$plugin" ] || fail "$target plugin is not executable"
  [ "$(sha "$plugin")" = "$expected_hash" ] || fail "$target installed the wrong archive payload"
  build_info=$(go version -m "$plugin") || fail "$target has no readable Go build metadata"
  os=${target%-*}
  arch=${target#*-}
  printf '%s\n' "$build_info" | grep -Fqx $'\tbuild\tGOOS='"$os" || fail "$target contains a binary for the wrong GOOS"
  printf '%s\n' "$build_info" | grep -Fqx $'\tbuild\tGOARCH='"$arch" || fail "$target contains a binary for the wrong GOARCH"
  grep -Fqx "plugin_path|$plugin" "$state/docker-extras/install.tsv" || fail "$target path was not recorded"
  require_absent "$home/.local/bin/docker-extras-volume-seed"
  env HOME="$home" XDG_STATE_HOME="$state" sh "$uninstall_script" >/dev/null
  require_absent "$plugin"
  require_absent "$state/docker-extras/install.tsv"
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
require_absent "$work/home-bad-checksum/.docker/cli-plugins/docker-extras"
require_absent "$work/state-bad-checksum/docker-extras/install.tsv"

say "unsupported targets and removed opt-outs fail before installation writes"
home="$work/home-unsupported"
state="$work/state-unsupported"
if env HOME="$home" XDG_STATE_HOME="$state" DOCKER_EXTRAS_BASE_URL="$base_url" \
   TEST_UNAME_SYSTEM=FreeBSD TEST_UNAME_MACHINE=x86_64 PATH="$mockbin:$PATH" \
   sh "$install_script" >"$work/unsupported.log" 2>&1; then
  fail "installer accepted unsupported FreeBSD target"
fi
grep -Fq 'unsupported platform FreeBSD/x86_64' "$work/unsupported.log" || fail "unsupported target refusal was unclear"
require_absent "$home/.docker/cli-plugins/docker-extras"
require_absent "$state/docker-extras/install.tsv"
for removed_env in DOCKER_EXTRAS_NO_PLUGIN DOCKER_EXTRAS_NO_PLUGIN_EMPTY DOCKER_EXTRAS_INSTALL_DIR; do
  case "$removed_env" in
    DOCKER_EXTRAS_NO_PLUGIN) removed_value=1 ;;
    DOCKER_EXTRAS_NO_PLUGIN_EMPTY) removed_env=DOCKER_EXTRAS_NO_PLUGIN; removed_value="" ;;
    DOCKER_EXTRAS_INSTALL_DIR) removed_value="" ;;
  esac
  if env HOME="$work/home-$removed_env" XDG_STATE_HOME="$work/state-$removed_env" \
     DOCKER_EXTRAS_BASE_URL="$base_url" "$removed_env=$removed_value" sh "$install_script" >/dev/null 2>&1; then
    fail "installer accepted removed option $removed_env"
  fi
  require_absent "$work/home-$removed_env/.docker/cli-plugins/docker-extras"
  require_absent "$work/state-$removed_env/docker-extras/install.tsv"
done
if env HOME="$work/home-invalid-name" XDG_STATE_HOME="$work/state-invalid-name" \
   DOCKER_EXTRAS_BASE_URL="$base_url" DOCKER_EXTRAS_PLUGIN_NAME=bad-name \
   sh "$install_script" >/dev/null 2>&1; then
  fail "installer accepted a hyphenated plugin name"
fi
require_absent "$work/home-invalid-name/.docker/cli-plugins/docker-bad-name"
require_absent "$work/state-invalid-name/docker-extras/install.tsv"

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

say "clean v1 migration removes verified Bash files and records only the new plugin"
create_legacy_install "$work/legacy-success"
env HOME="$LEGACY_HOME" XDG_STATE_HOME="$LEGACY_STATE" DOCKER_EXTRAS_BASE_URL="$base_url" \
  DOCKER_EXTRAS_PLUGIN_NAME=tools sh "$install_script" >/dev/null
new_plugin="$LEGACY_HOME/.docker/cli-plugins/docker-tools"
require_file "$new_plugin"
require_absent "$LEGACY_BIN/docker-extras-volume-seed"
require_absent "$LEGACY_HOME/.docker/cli-plugins/docker-extras"
grep -Fqx 'plugin_name|tools' "$LEGACY_STATE_FILE" || fail "migration retained old plugin name"
grep -Fqx "plugin_path|$new_plugin" "$LEGACY_STATE_FILE" || fail "migration did not record new plugin path"
[ "$(awk -F '|' '$1 == "file" { count++ } END { print count+0 }' "$LEGACY_STATE_FILE")" -eq 1 ] ||
  fail "plugin-only state still lists old standalone files"
env HOME="$LEGACY_HOME" XDG_STATE_HOME="$LEGACY_STATE" sh "$uninstall_script" >/dev/null
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
  if env HOME="$LEGACY_HOME" XDG_STATE_HOME="$LEGACY_STATE" DOCKER_EXTRAS_BASE_URL="$base_url" \
     DOCKER_EXTRAS_PLUGIN_NAME=tools sh "$install_script" >"$work/modified-$modified.log" 2>&1; then
    fail "migration accepted modified old $modified"
  fi
  cmp -s "$LEGACY_BIN/docker-extras-volume-seed" "$work/old-tool.snapshot" || fail "old tool changed on refusal"
  cmp -s "$LEGACY_PLUGIN" "$work/old-plugin.snapshot" || fail "old plugin changed on refusal"
  cmp -s "$LEGACY_STATE_FILE" "$work/old-state.snapshot" || fail "old state changed on refusal"
  require_absent "$LEGACY_HOME/.docker/cli-plugins/docker-tools"
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
  if env HOME="$LEGACY_HOME" XDG_STATE_HOME="$LEGACY_STATE" DOCKER_EXTRAS_BASE_URL="$base_url" \
     DOCKER_EXTRAS_PLUGIN_NAME=tools sh "$install_script" >"$work/invalid-$invalid.log" 2>&1; then
    fail "migration accepted invalid $invalid legacy entry"
  fi
  cmp -s "$LEGACY_STATE_FILE" "$work/invalid-state.snapshot" || fail "$invalid state bytes changed on refusal"
  cmp -s "$LEGACY_PLUGIN" "$work/invalid-plugin.snapshot" || fail "$invalid legacy plugin changed on refusal"
  require_absent "$LEGACY_HOME/.docker/cli-plugins/docker-tools"
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
if env HOME="$LEGACY_HOME" XDG_STATE_HOME="$LEGACY_STATE" DOCKER_EXTRAS_BASE_URL="$base_url" \
   DOCKER_EXTRAS_PLUGIN_NAME=tools sh "$install_script" >"$work/legacy-link-install.log" 2>&1; then
  fail "migration followed a symlinked legacy tool directory"
fi
if env HOME="$LEGACY_HOME" XDG_STATE_HOME="$LEGACY_STATE" \
   sh "$uninstall_script" >"$work/legacy-link-uninstall.log" 2>&1; then
  fail "uninstall followed a symlinked legacy tool directory"
fi
grep -Fq 'symlinked installation directory' "$work/legacy-link-install.log" || fail "migration did not report symlinked parent"
grep -Fq 'symlinked installation directory' "$work/legacy-link-uninstall.log" || fail "uninstall did not report symlinked parent"
cmp -s "$foreign_bin/docker-extras-volume-seed" "$work/foreign-tool.snapshot" || fail "foreign tool changed"
cmp -s "$LEGACY_PLUGIN" "$work/legacy-link-plugin.snapshot" || fail "legacy plugin changed"
cmp -s "$LEGACY_STATE_FILE" "$work/legacy-link-state.snapshot" || fail "legacy state changed"
require_absent "$LEGACY_HOME/.docker/cli-plugins/docker-tools"

say "symlinked fresh plugin or state ancestors refuse before destination writes"
home="$work/home-link-plugin"
state="$work/state-link-plugin"
foreign_plugin_dir="$work/foreign-plugin-dir"
mkdir -p "$home" "$foreign_plugin_dir/cli-plugins"
printf 'foreign marker\n' > "$foreign_plugin_dir/marker"
cp "$foreign_plugin_dir/marker" "$work/foreign-marker.snapshot"
ln -s "$foreign_plugin_dir" "$home/.docker"
if env HOME="$home" XDG_STATE_HOME="$state" DOCKER_EXTRAS_BASE_URL="$base_url" \
   sh "$install_script" >"$work/link-plugin-install.log" 2>&1; then
  fail "installer followed symlinked Docker plugin parent"
fi
grep -Fq 'symlinked installation directory' "$work/link-plugin-install.log" || fail "plugin parent refusal was unclear"
cmp -s "$foreign_plugin_dir/marker" "$work/foreign-marker.snapshot" || fail "foreign plugin directory changed"
require_absent "$foreign_plugin_dir/cli-plugins/docker-extras"
require_absent "$state/docker-extras/install.tsv"

home="$work/home-link-state"
state="$work/state-link-state"
foreign_state="$work/foreign-state-dir"
mkdir -p "$foreign_state" "$(dirname "$state")"
printf 'foreign marker\n' > "$foreign_state/marker"
cp "$foreign_state/marker" "$work/foreign-state.snapshot"
ln -s "$foreign_state" "$state"
if env HOME="$home" XDG_STATE_HOME="$state" DOCKER_EXTRAS_BASE_URL="$base_url" \
   sh "$install_script" >"$work/link-state-install.log" 2>&1; then
  fail "installer followed symlinked state parent"
fi
grep -Fq 'symlinked installation directory' "$work/link-state-install.log" || fail "state parent refusal was unclear"
cmp -s "$foreign_state/marker" "$work/foreign-state.snapshot" || fail "foreign state directory changed"
require_absent "$foreign_state/docker-extras/install.tsv"
require_absent "$home/.docker/cli-plugins/docker-extras"

say "unowned destination collisions are preserved"
home="$work/home-collision"
state="$work/state-collision"
plugin="$home/.docker/cli-plugins/docker-tools"
mkdir -p "$(dirname "$plugin")"
printf 'user-owned plugin\n' > "$plugin"
cp "$plugin" "$work/collision.snapshot"
if env HOME="$home" XDG_STATE_HOME="$state" DOCKER_EXTRAS_BASE_URL="$base_url" \
   DOCKER_EXTRAS_PLUGIN_NAME=tools sh "$install_script" >/dev/null 2>&1; then
  fail "installer overwrote an unowned plugin collision"
fi
cmp -s "$plugin" "$work/collision.snapshot" || fail "collision bytes changed"
require_absent "$state/docker-extras/install.tsv"

say "custom plugin metadata, help, dispatch, and completion work through actual Docker"
case "$(uname -s):$(uname -m)" in
  Linux:x86_64|Linux:aarch64|Darwin:x86_64|Darwin:arm64|Darwin:aarch64) ;;
  *) fail "test runner uses unsupported platform $(uname -s)/$(uname -m)" ;;
esac
if ! command -v docker >/dev/null 2>&1; then
  if [ "${REQUIRE_DOCKER:-0}" = 1 ]; then fail "Docker CLI is required by package gate"; fi
  say "Docker CLI absent; skipping actual Docker command-dispatch check"
else
  home="$work/home-docker-custom"
  state="$work/state-docker-custom"
  env HOME="$home" XDG_STATE_HOME="$state" DOCKER_EXTRAS_BASE_URL="$base_url" \
    DOCKER_EXTRAS_PLUGIN_NAME=tools sh "$install_script" >/dev/null
  plugin="$home/.docker/cli-plugins/docker-tools"
  metadata=$("$plugin" docker-cli-plugin-metadata)
  [[ "$metadata" == *'"SchemaVersion":"0.1.0"'* ]] || fail "custom metadata handshake failed: $metadata"
  expected_version=$(git describe --tags --match 'v[0-9]*' --always --dirty)
  expected_version=${expected_version#v}
  [[ "$metadata" == *"\"Version\":\"$expected_version\""* ]] || fail "plugin build version metadata is wrong: $metadata"
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
  env HOME="$home" XDG_STATE_HOME="$state" sh "$uninstall_script" >/dev/null
  require_absent "$plugin"
  require_absent "$state/docker-extras/install.tsv"
fi

home="$work/home-modified-installed"
state="$work/state-modified-installed"
env HOME="$home" XDG_STATE_HOME="$state" DOCKER_EXTRAS_BASE_URL="$base_url" \
  DOCKER_EXTRAS_PLUGIN_NAME=tools sh "$install_script" >/dev/null
plugin="$home/.docker/cli-plugins/docker-tools"
state_file="$state/docker-extras/install.tsv"
printf 'user edit\n' >> "$plugin"
cp "$plugin" "$work/new-plugin.snapshot"
cp "$state_file" "$work/new-state.snapshot"
if env HOME="$home" XDG_STATE_HOME="$state" sh "$uninstall_script" >/dev/null 2>&1; then
  fail "uninstaller removed a modified plugin"
fi
cmp -s "$plugin" "$work/new-plugin.snapshot" || fail "modified plugin changed on uninstall refusal"
cmp -s "$state_file" "$work/new-state.snapshot" || fail "state changed on uninstall refusal"

say "uninstall refuses symlinked state ancestors without removing an installed plugin"
home="$work/home-uninstall-link-state"
state="$work/state-uninstall-link-state"
env HOME="$home" XDG_STATE_HOME="$state" DOCKER_EXTRAS_BASE_URL="$base_url" \
  sh "$install_script" >/dev/null
plugin="$home/.docker/cli-plugins/docker-extras"
foreign_state="$work/foreign-uninstall-state"
mv "$state" "$foreign_state"
ln -s "$foreign_state" "$state"
cp "$plugin" "$work/uninstall-link-plugin.snapshot"
cp "$foreign_state/docker-extras/install.tsv" "$work/uninstall-link-state.snapshot"
if env HOME="$home" XDG_STATE_HOME="$state" sh "$uninstall_script" >"$work/link-state-uninstall.log" 2>&1; then
  fail "uninstaller followed symlinked state parent"
fi
grep -Fq 'symlinked installation directory' "$work/link-state-uninstall.log" || fail "state parent uninstall refusal was unclear"
cmp -s "$plugin" "$work/uninstall-link-plugin.snapshot" || fail "plugin changed on state parent refusal"
cmp -s "$foreign_state/docker-extras/install.tsv" "$work/uninstall-link-state.snapshot" || fail "foreign state changed on refusal"

say "all platform, migration, custom dispatch, and uninstall checks passed"
