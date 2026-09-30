#!/usr/bin/env bash
# Regression harness for the Go Docker Extras volume seed plugin. Its headline case is C1: a seed
# truncated to a 512-byte boundary must be REFUSED before the target volume
# changes, not silently accepted (a block-aligned truncation is exactly the
# case a `tar -tf` header walk misses — see the sha256/bytes= gate in
# cmd/docker-extras/restore.go).
#
# Run it by hand:
#   make test          (or: bash tests/docker-extras-volume-seed-test.sh)
#
# It needs a reachable docker daemon and skips (exit 0) without one, so a
# daemonless machine is not a failure. CI runs it on a GitHub-hosted runner,
# which provides a daemon.
#
# Safety: every volume gets an unpredictable dvsprobe-<purpose>-<run-token>
# name, is refused if already present, and is removed only when its run-owner
# label matches. This never sweeps other Docker objects or runs compose. The
# seed directory is its own mktemp -d; cleanup also runs on assertion failure.
#
# It builds the working-tree Go CLI plugin into this test's temporary Docker
# config and invokes every seed operation through `docker extras`.

set -euo pipefail

say() { printf 'docker-extras-volume-seed-test: %s\n' "$*" >&2; }
ok()  { printf 'docker-extras-volume-seed-test: ok: %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf 'docker-extras-volume-seed-test: FAIL: %s\n' "$1"; fail=$((fail + 1)); }

pass=0
fail=0

# No docker, or no daemon behind it: skip rather than fail. A machine with no
# daemon is not a lint failure, and this harness has no other way to run.
# REQUIRE_DOCKER=1 turns the skip into a failure — CI sets it, so a runner
# misconfiguration can never pass the gate having tested nothing.
if ! command -v docker >/dev/null 2>&1 \
   || ! docker version --format '{{.Server.Version}}' >/dev/null 2>&1; then
  if [ "${REQUIRE_DOCKER:-0}" = "1" ]; then
    say "no docker daemon reachable and REQUIRE_DOCKER=1 — failing"
    exit 1
  fi
  say "no docker daemon reachable — skipping (not a failure)"
  exit 0
fi

# Resolve the active endpoint before isolating Docker's config for plugin
# discovery. Named contexts live under DOCKER_CONFIG, so retain their context
# store by reference and prove the isolated config reaches the same daemon.
original_docker_config="${DOCKER_CONFIG:-$HOME/.docker}"
original_docker_context="${DOCKER_CONTEXT-}"
original_docker_host="${DOCKER_HOST-}"
if [ -n "${BDS245_TEST_DOCKER_HOST:-}" ]; then
  # Keep the default engine visible to Go's distinct-ID assertions while
  # selecting the disposable alternate engine only for this shell harness.
  original_docker_context=""
  original_docker_host="$BDS245_TEST_DOCKER_HOST"
  selected_daemon_id=$(DOCKER_HOST="$original_docker_host" docker info --format '{{.ID}}')
elif [ -z "$original_docker_context" ] && [ -z "$original_docker_host" ]; then
  original_docker_context="$(docker context show)"
  selected_daemon_id="$(docker info --format '{{.ID}}')"
else
  selected_daemon_id="$(docker info --format '{{.ID}}')"
fi

repo_root="$(git rev-parse --show-toplevel)"
if [ ! -f "$repo_root/go.mod" ] || [ ! -d "$repo_root/cmd/docker-extras" ]; then
  say "error: Go CLI source not found under $repo_root"
  exit 1
fi

D="$(mktemp -d)"
run_token="${D##*/}"
run_token="${run_token#tmp.}"
owner_label="com.procrastivity.docker-extras.test-run=$run_token"
vols=()

owned_volume() {
  [ "$(docker volume inspect --format '{{ index .Labels "com.procrastivity.docker-extras.test-run" }}' "$1" 2>/dev/null)" = "$run_token" ]
}

cleanup() {
  local v
  for v in "${vols[@]}"; do
    if owned_volume "$v"; then
      docker volume rm -f "$v" >/dev/null 2>&1 || true
    fi
  done
  rm -rf "$D"
}
trap cleanup EXIT

# Docker discovers plugins inside DOCKER_CONFIG. Keep the plugin installation
# isolated to this fixture and leave the user's Docker CLI config untouched.
export DOCKER_CONFIG="$D/docker-config"
mkdir -p "$DOCKER_CONFIG/cli-plugins"
if [ -n "$original_docker_context" ]; then
  if [ "$original_docker_context" != default ]; then
    context_store="$original_docker_config/contexts"
    [ -d "$context_store" ] || {
      say "error: selected context '$original_docker_context' is missing from $context_store"
      exit 1
    }
    ln -s "$context_store" "$DOCKER_CONFIG/contexts"
  fi
  export DOCKER_CONTEXT="$original_docker_context"
  unset DOCKER_HOST
else
  unset DOCKER_CONTEXT
  export DOCKER_HOST="$original_docker_host"
fi
isolated_daemon_id="$(docker info --format '{{.ID}}')"
if [ "$isolated_daemon_id" != "$selected_daemon_id" ]; then
  say "error: isolated plugin config changed Docker daemon (before=$selected_daemon_id after=$isolated_daemon_id)"
  exit 1
fi
say "isolated plugin config preserved selected Docker daemon $selected_daemon_id"
plugin="$DOCKER_CONFIG/cli-plugins/docker-extras"
go -C "$repo_root" build -o "$plugin" ./cmd/docker-extras
run_seed() { docker extras volume seed "$@"; }

# Reserve an absent name before any operation can create it. Cleanup checks
# the owner label even for an attempted --allow-create that did not succeed.
# $new_vol is global so vols+=() stays in this shell.
new_vol=""
reserve_vol() {
  new_vol="dvsprobe-$1-$run_token"
  if docker volume inspect "$new_vol" >/dev/null 2>&1; then
    say "refusing pre-existing volume $new_vol"
    exit 1
  fi
  vols+=("$new_vol")
}
mkvol() {
  reserve_vol "$1"
  docker volume create --label "$owner_label" "$new_vol" >/dev/null
  owned_volume "$new_vol" || { say "created volume $new_vol is not owned by this run"; exit 1; }
}
# Same, but does not create the volume yet — for the --allow-create cases,
# where the plugin itself is the thing that creates it.
namevol() {
  reserve_vol "$1"
}

meta_value() { sed -n "s/^$1=//p" "$2" 2>/dev/null | head -n1; }

# One "sha256  path" line per regular file in VOL, sorted — the round-trip and
# post-restore comparisons both reduce to a text diff of two of these.
tree_digest() {
  owned_volume "$1" || { say "refusing to mount missing or unowned volume $1"; return 1; }
  docker run --rm -v "$1":/v:ro alpine \
    sh -c 'cd /v && find . -type f -exec sha256sum {} \;' | sort
}

plant_marker() {  # plant_marker VOL — a file a wrongly-cleared volume can't keep
  owned_volume "$1" || { say "refusing to mount missing or unowned volume $1"; return 1; }
  docker run --rm -v "$1":/v alpine sh -c '
    set -e
    mkdir -p /v/precious
    printf "DO-NOT-LOSE-ME\n" >/v/marker.txt
    printf "precious-data\n"  >/v/precious/db.ibd
  ' >/dev/null
}

marker_of() {  # marker_of VOL — empty if cleared; refuses missing/unowned volumes
  owned_volume "$1" || { say "refusing to mount missing or unowned volume $1"; return 1; }
  docker run --rm -v "$1":/v:ro alpine cat /v/marker.txt 2>/dev/null || true
}

# ---------------------------------------------------------------------------
# a. a populated source volume

say "step a: populate a source volume"
mkvol src; src="$new_vol"
docker run --rm -v "$src":/v alpine sh -c '
  set -e
  mkdir -p /v/subdir
  printf "alpha-content\n" >/v/top.txt
  printf "beta-content\n"  >/v/subdir/nested.txt
' >/dev/null
if docker volume inspect "$src" >/dev/null 2>&1; then
  ok "source volume $src exists and is populated"
else
  bad "source volume $src was not created"
fi

# ---------------------------------------------------------------------------
# b. capture a good seed

say "step b: capture a good seed"
rc=0
run_seed capture --from-volume "$src" --name probe --data-dir "$D" \
  --image alpine:latest --yes >/dev/null 2>&1 || rc=$?
if [ "$rc" -eq 0 ] && [ -f "$D/probe.tar" ] && [ -f "$D/probe.meta" ]; then
  ok "capture exited 0 and wrote probe.tar + probe.meta"
else
  bad "capture did not produce a usable seed (rc=$rc)"
fi

bytes_meta="$(meta_value bytes "$D/probe.meta")"
bytes_real="$(stat -c %s "$D/probe.tar" 2>/dev/null || echo '')"
if [ -n "$bytes_meta" ] && [ "$bytes_meta" = "$bytes_real" ]; then
  ok "sidecar bytes=$bytes_meta matches the archive's real size"
else
  bad "sidecar bytes=$bytes_meta does not match the archive size ($bytes_real)"
fi

sha_meta="$(meta_value sha256 "$D/probe.meta")"
if [[ "$sha_meta" =~ ^[0-9a-f]{64}$ ]]; then
  ok "sidecar sha256= is a 64-character hex digest"
else
  bad "sidecar sha256= is not a valid digest (got: $sha_meta)"
fi

# ---------------------------------------------------------------------------
# c. restore the good seed into a fresh volume, d. compare

say "step c: restore the good seed into a fresh volume"
namevol dst; dst="$new_vol"
rc=0
run_seed restore --to-volume "$dst" --name probe --data-dir "$D" \
  --allow-create --label "$owner_label" --expect-image alpine:latest --yes >/dev/null 2>&1 || rc=$?
if [ "$rc" -eq 0 ]; then
  ok "restore into a fresh volume exited 0"
else
  bad "restore into a fresh volume failed (rc=$rc)"
fi

say "step d: compare source and restored contents"
src_list="$(tree_digest "$src")"
dst_list="$(tree_digest "$dst")"
if [ -n "$src_list" ] && [ "$src_list" = "$dst_list" ]; then
  ok "restored volume is byte-for-byte identical to the source"
else
  bad "restored volume differs from the source"
fi

# ---------------------------------------------------------------------------
# e. C1 — a seed truncated to a 512-byte boundary must be refused pre-clear

say "step e: a seed truncated to a 512-byte boundary must be refused"
cp "$D/probe.tar" "$D/probe2.tar"
cp "$D/probe.meta" "$D/probe2.meta"
truncate -s 512 "$D/probe2.tar"

mkvol keep1; keep1="$new_vol"
plant_marker "$keep1"

rc=0
run_seed restore --to-volume "$keep1" --name probe2 --data-dir "$D" \
  --expect-image alpine:latest --yes >/dev/null 2>&1 || rc=$?
if [ "$rc" -eq 1 ]; then
  ok "truncated seed is refused with exit 1 (pre-clear), not silently accepted"
else
  bad "truncated seed returned rc=$rc, expected exactly 1"
fi

if [ "$(marker_of "$keep1")" = "DO-NOT-LOSE-ME" ]; then
  ok "the target's prior data survived the refused restore"
else
  bad "the target's marker file is gone — the refusal did not leave data intact"
fi

# ---------------------------------------------------------------------------
# f. one flipped byte, same size — only the sha256 check catches it

say "step f: one flipped byte, same size — only the sha256 check catches it"
cp "$D/probe.tar" "$D/probe3.tar"
cp "$D/probe.meta" "$D/probe3.meta"
# Offset 20 sits inside the first tar header's 100-byte name field: changing
# any one byte there moves the header away from its own recorded checksum, so
# this corrupts the archive without changing its size — a byte-count check
# alone (step e) would pass this file straight through.
printf '\377' | dd of="$D/probe3.tar" bs=1 seek=20 count=1 conv=notrunc status=none

mkvol keep2; keep2="$new_vol"
plant_marker "$keep2"

rc=0
run_seed restore --to-volume "$keep2" --name probe3 --data-dir "$D" \
  --expect-image alpine:latest --yes >/dev/null 2>&1 || rc=$?
if [ "$rc" -eq 1 ]; then
  ok "a same-size bit flip is refused with exit 1 — the sha256 check caught it, not a size check"
else
  bad "bit-flipped seed returned rc=$rc, expected exactly 1"
fi

if [ "$(marker_of "$keep2")" = "DO-NOT-LOSE-ME" ]; then
  ok "the target's prior data survived the refused restore"
else
  bad "the target's marker file is gone — the refusal did not leave data intact"
fi

# ---------------------------------------------------------------------------
# g. --no-verify is the escape hatch — proves (e) and (f) came from the gate

say "step g: --no-verify lets the same corrupt seed through the gate"
namevol nv; nv="$new_vol"
rc=0
output="$(run_seed restore --to-volume "$nv" --name probe3 --data-dir "$D" \
  --allow-create --label "$owner_label" --no-verify --expect-image alpine:latest --yes 2>&1)" || rc=$?
if [ "$rc" -eq 2 ] && [[ "$output" == *"tar extract"* ]] && owned_volume "$nv"; then
  ok "--no-verify reached tar extract, created the target, and returned exit 2"
else
  bad "--no-verify did not reach the expected extract failure and owned target (rc=$rc): $output"
fi

# ---------------------------------------------------------------------------
# h. image identity is independent of integrity verification

say "step h: unknown and mismatched image locks preserve the target"
mkvol image-lock; image_lock="$new_vol"
plant_marker "$image_lock"

rc=0
output="$(run_seed restore --to-volume "$image_lock" --name probe --data-dir "$D" \
  --yes 2>&1)" || rc=$?
if [ "$rc" -eq 1 ] && [[ "$output" == *"cannot determine which image"* ]]; then
  ok "an unknown target image is refused before replacement"
else
  bad "unknown target image returned rc=$rc without the expected refusal: $output"
fi
if [ "$(marker_of "$image_lock")" = "DO-NOT-LOSE-ME" ]; then
  ok "the target's prior data survived the unknown-image refusal"
else
  bad "unknown-image refusal changed the target"
fi

rc=0
output="$(run_seed restore --to-volume "$image_lock" --name probe --data-dir "$D" \
  --expect-image alpine:3.22 --no-verify --yes 2>&1)" || rc=$?
if [ "$rc" -eq 1 ] && [[ "$output" == *"seed was captured under image"* ]]; then
  ok "--no-verify does not bypass a mismatched image lock"
else
  bad "mismatched image with --no-verify returned rc=$rc without the expected refusal: $output"
fi
if [ "$(marker_of "$image_lock")" = "DO-NOT-LOSE-ME" ]; then
  ok "the target's prior data survived the mismatched-image refusal"
else
  bad "mismatched-image refusal changed the target"
fi

rc=0
run_seed restore --to-volume "$image_lock" --name probe --data-dir "$D" \
  --expect-image alpine:3.22 --force --no-verify --yes >/dev/null 2>&1 || rc=$?
if [ "$rc" -eq 0 ] && [ "$(tree_digest "$src")" = "$(tree_digest "$image_lock")" ]; then
  ok "--force bypassed only the mismatched image lock and restored the expected bytes"
else
  bad "forced mismatched-image restore failed or produced different bytes (rc=$rc)"
fi

# ---------------------------------------------------------------------------
# i. creation is delayed until gates pass; failure after clear is exit 2

say "step i: delayed creation and post-clear failure classification"
namevol delayed; delayed="$new_vol"
rc=0
output="$(run_seed restore --to-volume "$delayed" --name probe --data-dir "$D" \
  --allow-create --label "$owner_label" --expect-image alpine:3.22 --yes 2>&1)" || rc=$?
if [ "$rc" -eq 1 ] && [[ "$output" == *"seed was captured under image"* ]] \
  && ! docker volume inspect "$delayed" >/dev/null 2>&1; then
  ok "image refusal left the delayed --allow-create target absent"
else
  bad "delayed create gate returned rc=$rc or created its target: $output"
fi
if tree_digest "$delayed" >/dev/null 2>&1 || marker_of "$delayed" >/dev/null 2>&1 \
  || docker volume inspect "$delayed" >/dev/null 2>&1; then
  bad "read helpers mounted or created the reserved target after pre-create refusal"
else
  ok "read helpers refused the absent target without creating a volume"
fi

printf 'not a tar archive\n' >"$D/partial.tar"
printf 'image=alpine:latest\n' >"$D/partial.meta"
mkvol clearfail; clearfail="$new_vol"
plant_marker "$clearfail"
rc=0
output="$(run_seed restore --to-volume "$clearfail" --name partial --data-dir "$D" \
  --expect-image alpine:latest --no-verify --yes 2>&1)" || rc=$?
if [ "$rc" -eq 2 ]; then
  ok "an extract failure after target clear returned exit 2"
else
  bad "post-clear extract failure returned rc=$rc instead of 2: $output"
fi
if [ -z "$(marker_of "$clearfail")" ]; then
  ok "post-clear failure did not falsely claim the old target was intact"
else
  bad "post-clear failure left the old marker, contrary to the simulated clear"
fi

# ---------------------------------------------------------------------------
# ---------------------------------------------------------------------------
# j. cleanup — asserted explicitly; the EXIT trap is the failure-path backstop

say "step j: cleanup"
for v in "${vols[@]}"; do
  if owned_volume "$v"; then
    docker volume rm -f "$v" >/dev/null 2>&1 || true
  fi
  if docker volume inspect "$v" >/dev/null 2>&1; then
    bad "volume $v remains (not owned or removal failed)"
  else
    ok "reserved volume $v is absent"
  fi
done
rm -rf "$D"
if [ -d "$D" ]; then
  bad "seed directory $D was not removed"
else
  ok "seed directory $D removed"
fi
vols=()  # cleared, so the EXIT trap's loop above is a guaranteed no-op

echo >&2
say "passed=$pass failed=$fail"
[ "$fail" -eq 0 ]
