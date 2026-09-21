#!/usr/bin/env bash
# Installer/uninstaller regression harness. It uses a local file:// release
# fixture, so it exercises download, checksum, custom plugin naming, state
# recording, safe refusal, and removal without network access or Docker.

set -euo pipefail

say() { printf 'install-uninstall-test: %s\n' "$*" >&2; }
fail() { say "FAIL: $*"; exit 1; }
require_file() { [ -f "$1" ] || fail "missing file: $1"; }
require_absent() { [ ! -e "$1" ] || fail "still present: $1"; }

repo_root="$(git rev-parse --show-toplevel)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

release="$work/releases/latest/download"
stage="$work/stage"
mkdir -p "$release" "$stage/bin" "$stage/plugin"
cp "$repo_root/bin/docker-extras-volume-seed" "$stage/bin/"
cp "$repo_root/plugin/docker-extras" "$stage/plugin/"
tar -czf "$release/docker-extras.tar.gz" -C "$stage" .
(cd "$release" && sha256sum docker-extras.tar.gz > SHA256SUMS)

install_script="$repo_root/scripts/install.sh"
uninstall_script="$repo_root/scripts/uninstall.sh"
base_url="file://$work"

home="$work/home custom"
state="$work/state custom"
install_dir="$work/bin custom"
mkdir -p "$home" "$state" "$install_dir"

say "custom plugin name is recorded and removed"
HOME="$home" XDG_STATE_HOME="$state" \
DOCKER_EXTRAS_BASE_URL="$base_url" \
DOCKER_EXTRAS_INSTALL_DIR="$install_dir" \
DOCKER_EXTRAS_PLUGIN_NAME=tools \
sh "$install_script" >/dev/null

tool="$install_dir/docker-extras-volume-seed"
plugin="$home/.docker/cli-plugins/docker-tools"
state_file="$state/docker-extras/install.tsv"
require_file "$tool"
require_file "$plugin"
[ -x "$tool" ] || fail "tool is not executable"
[ -x "$plugin" ] || fail "plugin is not executable"
grep -Fqx 'plugin_name|tools' "$state_file" || fail "custom plugin name was not recorded"

plugin_output=$(HOME="$home" PATH="$install_dir:$PATH" "$plugin" list)
grep -Fqx 'docker-tools dev — tools:' <<<"$plugin_output" ||
  fail "plugin did not derive its command name from the installed path"

HOME="$home" XDG_STATE_HOME="$state" sh "$uninstall_script" >/dev/null
require_absent "$tool"
require_absent "$plugin"
require_absent "$state_file"

say "no-plugin installs are recorded and removed"
home="$work/home-no-plugin"
state="$work/state-no-plugin"
install_dir="$work/bin-no-plugin"
mkdir -p "$home" "$state" "$install_dir"
HOME="$home" XDG_STATE_HOME="$state" \
DOCKER_EXTRAS_BASE_URL="$base_url" \
DOCKER_EXTRAS_INSTALL_DIR="$install_dir" \
DOCKER_EXTRAS_NO_PLUGIN=1 \
sh "$install_script" >/dev/null

tool="$install_dir/docker-extras-volume-seed"
plugin="$home/.docker/cli-plugins/docker-extras"
state_file="$state/docker-extras/install.tsv"
require_file "$tool"
require_absent "$plugin"
grep -Fqx 'plugin_path|' "$state_file" || fail "no-plugin install recorded a plugin path"
HOME="$home" XDG_STATE_HOME="$state" sh "$uninstall_script" >/dev/null
require_absent "$tool"
require_absent "$state_file"

say "an existing plugin path is protected before tools are copied"
home="$work/home-conflict"
state="$work/state-conflict"
install_dir="$work/bin-conflict"
mkdir -p "$home/.docker/cli-plugins" "$state" "$install_dir"
plugin="$home/.docker/cli-plugins/docker-tools"
printf 'user-owned plugin\n' > "$plugin"
if HOME="$home" XDG_STATE_HOME="$state" \
   DOCKER_EXTRAS_BASE_URL="$base_url" \
   DOCKER_EXTRAS_INSTALL_DIR="$install_dir" \
   DOCKER_EXTRAS_PLUGIN_NAME=tools \
   sh "$install_script" >/dev/null 2>&1; then
  fail "installer overwrote an existing plugin"
fi
require_absent "$install_dir/docker-extras-volume-seed"
grep -Fqx 'user-owned plugin' "$plugin" || fail "existing plugin was changed"

say "default plugin name refuses modified files and then uninstalls"
home="$work/home-default"
state="$work/state-default"
install_dir="$work/bin-default"
mkdir -p "$home" "$state" "$install_dir"
HOME="$home" XDG_STATE_HOME="$state" \
DOCKER_EXTRAS_BASE_URL="$base_url" \
DOCKER_EXTRAS_INSTALL_DIR="$install_dir" \
sh "$install_script" >/dev/null

tool="$install_dir/docker-extras-volume-seed"
plugin="$home/.docker/cli-plugins/docker-extras"
state_file="$state/docker-extras/install.tsv"
printf '# modified after install\n' >> "$tool"
if HOME="$home" XDG_STATE_HOME="$state" sh "$uninstall_script" >/dev/null 2>&1; then
  fail "uninstaller removed a modified file"
fi
require_file "$tool"
require_file "$plugin"
require_file "$state_file"

cp "$stage/bin/docker-extras-volume-seed" "$tool"
HOME="$home" XDG_STATE_HOME="$state" sh "$uninstall_script" >/dev/null
require_absent "$tool"
require_absent "$plugin"
require_absent "$state_file"

say "invalid plugin names are rejected"
home="$work/home-invalid"
state="$work/state-invalid"
install_dir="$work/bin-invalid"
mkdir -p "$home" "$state" "$install_dir"
if HOME="$home" XDG_STATE_HOME="$state" \
   DOCKER_EXTRAS_BASE_URL="$base_url" \
   DOCKER_EXTRAS_INSTALL_DIR="$install_dir" \
   DOCKER_EXTRAS_PLUGIN_NAME=bad-name \
   sh "$install_script" >/dev/null 2>&1; then
  fail "installer accepted a hyphenated plugin name"
fi

say "all installer/uninstaller checks passed"
