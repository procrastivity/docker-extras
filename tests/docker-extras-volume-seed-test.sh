#!/usr/bin/env bash
# Regression harness for docker-extras-volume-seed. Its headline case is C1: a seed
# truncated to a 512-byte boundary must be REFUSED before the target volume
# changes, not silently accepted (a block-aligned truncation is exactly the
# case a `tar -tf` header walk misses — see the sha256/bytes= gate in
# docker-extras-volume-seed.sh).
#
# Run it by hand:
#   make test          (or: bash tests/docker-extras-volume-seed-test.sh)
#
# It needs a reachable docker daemon and skips (exit 0) without one, so a
# daemonless machine is not a failure. CI runs it on a GitHub-hosted runner,
# which provides a daemon.
#
# Safety: every docker object this harness creates is named
# dvsprobe-<purpose>-$$, so it can never collide with a real volume. Each
# created name is recorded the moment it is reserved, and only those exact
# names are removed at the end — this never runs `docker volume ls` or sweeps
# anything it did not create, and it never runs `docker compose`. The seed
# directory is its own mktemp -d. Cleanup runs from an EXIT trap, so a failed
# assertion still removes everything.
#
# It tests the WORKING TREE copy of docker-extras-volume-seed, not whatever sits on
# PATH, so a local fix is graded before it is installed anywhere.

set -euo pipefail

say() { printf 'docker-extras-volume-seed-test: %s\n' "$*" >&2; }
ok()  { printf 'docker-extras-volume-seed-test: ok: %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf 'docker-extras-volume-seed-test: FAIL: %s\n' "$1"; fail=$((fail + 1)); }

pass=0
fail=0

# No docker, or no daemon behind it: skip rather than fail. A machine with no
# daemon is not a lint failure, and this harness has no other way to run.
if ! command -v docker >/dev/null 2>&1 \
   || ! docker version --format '{{.Server.Version}}' >/dev/null 2>&1; then
  say "no docker daemon reachable — skipping (not a failure)"
  exit 0
fi

repo_root="$(git rev-parse --show-toplevel)"
dvs="$repo_root/bin/docker-extras-volume-seed"
[ -f "$dvs" ] || { say "error: $dvs not found"; exit 1; }

D="$(mktemp -d)"
vols=()

cleanup() {
  local v
  for v in "${vols[@]}"; do
    docker volume rm -f "$v" >/dev/null 2>&1 || true
  done
  rm -rf "$D"
}
trap cleanup EXIT

# Reserve dvsprobe-PURPOSE-$$ and record it for cleanup, into $new_vol — a
# global instead of a return-by-echo, because the vols+=() append has to land
# in THIS shell, not a command-substitution subshell.
new_vol=""
mkvol() {
  new_vol="dvsprobe-$1-$$"
  vols+=("$new_vol")
  docker volume create "$new_vol" >/dev/null
}
# Same, but does not create the volume yet — for the --allow-create cases,
# where docker-extras-volume-seed itself is the thing that creates it.
namevol() {
  new_vol="dvsprobe-$1-$$"
  vols+=("$new_vol")
}

meta_value() { sed -n "s/^$1=//p" "$2" 2>/dev/null | head -n1; }

# One "sha256  path" line per regular file in VOL, sorted — the round-trip and
# post-restore comparisons both reduce to a text diff of two of these.
tree_digest() {
  docker run --rm -v "$1":/v:ro alpine \
    sh -c 'cd /v && find . -type f -exec sha256sum {} \;' | sort
}

plant_marker() {  # plant_marker VOL — a file a wrongly-cleared volume can't keep
  docker run --rm -v "$1":/v alpine sh -c '
    set -e
    mkdir -p /v/precious
    printf "DO-NOT-LOSE-ME\n" >/v/marker.txt
    printf "precious-data\n"  >/v/precious/db.ibd
  ' >/dev/null
}

marker_of() {  # marker_of VOL — empty if missing or the volume was cleared
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
bash "$dvs" capture --from-volume "$src" --name probe --data-dir "$D" \
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
bash "$dvs" restore --to-volume "$dst" --name probe --data-dir "$D" \
  --allow-create --expect-image alpine:latest --yes >/dev/null 2>&1 || rc=$?
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
bash "$dvs" restore --to-volume "$keep1" --name probe2 --data-dir "$D" \
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
bash "$dvs" restore --to-volume "$keep2" --name probe3 --data-dir "$D" \
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
bash "$dvs" restore --to-volume "$nv" --name probe3 --data-dir "$D" \
  --allow-create --no-verify --expect-image alpine:latest --yes \
  >/dev/null 2>&1 || rc=$?
if [ "$rc" -ne 1 ]; then
  ok "--no-verify proceeds past the integrity gate (rc=$rc, not the exit-1 refusal)"
else
  bad "--no-verify still refused with exit 1 — the gate is not actually skippable"
fi

# ---------------------------------------------------------------------------
# h. cleanup — asserted explicitly; the EXIT trap is the failure-path backstop

say "step h: cleanup"
for v in "${vols[@]}"; do
  docker volume rm -f "$v" >/dev/null 2>&1 || true
  if docker volume inspect "$v" >/dev/null 2>&1; then
    bad "volume $v was not removed"
  else
    ok "volume $v removed"
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
