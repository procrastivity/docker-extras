# docker-extras-volume-seed

Archive a Docker named volume to a seed file, and restore a seed file into
another named volume. The tool knows only Docker: compose projects are
discovered from the labels Docker Compose stamps on volumes and containers,
never from compose files or application layout.

```
docker extras volume seed capture --from-volume VOL [--name NAME] [--data-dir DIR]
                                  [--image IMG] [--yes] [--report FILE]
docker extras volume seed restore --to-volume VOL [--name NAME] [--data-dir DIR]
                                  [--allow-create] [--label KEY=VALUE]...
                                  [--expect-image IMG] [--yes] [--force]
                                  [--no-verify] [--report FILE]
```

`--help` carries the full option reference; this page explains the design.

Both commands need the Docker CLI and a reachable Docker Engine daemon. The
daemon must be able to read and write the client-side `--data-dir` through a
bind mount; a remote daemon normally cannot see the client's filesystem, and
the command refuses during preflight if the round trip fails. This applies to
local and remote contexts alike: use a data directory genuinely shared with
the selected daemon or use a local context. Compose-labelled users require the
Docker Compose CLI plugin. The plugin does not install shell completion; after
separately enabling Docker CLI completion with `docker completion bash` (or
the corresponding supported shell command), Docker can request the nested
command and static flag candidates. Completion does not enumerate Docker
resources or paths.

## The seed

A seed is a plain uncompressed tar of the volume contents (`NAME.tar`) plus a
`.meta` sidecar recording the source volume, container image, compose project,
date, byte count, and sha256. Both directions run tar inside one throwaway
container with the seed directory bind-mounted, so the bytes move at disk
speed (a 32 GB volume measured ~20 s each way). The tar is deliberately
uncompressed — compress a seed yourself when you need to ship or keep many.

## Safety model

Restore is not reversible: it clears the target before tar reads a byte. Three
protections exist because of that.

1. **Integrity gate.** Restore checks the sidecar's byte count and sha256
   against the archive before it stops anything and before the clear, so a
   truncated or rotted seed leaves the target exactly as it was. A seed
   captured before these fields existed falls back to a `tar -tf` header
   check. `--no-verify` skips the gate; nothing lets a restore continue past a
   check that ran and failed.
2. **Image lock.** A physical database data dir only starts cleanly under the
   image that wrote it, so restore refuses a mismatched or unknown image.
   Capture records the image from containers attached to the volume, or from
   `--image` when the project is down; restore infers the target's image the
   same way, or takes `--expect-image`. `--force` overrides the lock and
   nothing else.
3. **Named, confirmed stops.** Both directions survey what runs on the volume,
   name every compose project (with its services) and container they will
   stop, and print what will be stopped. Capture prompts only when users are
   pending; restore prompts whenever `--yes` is absent, even if the target is
   idle or will be created. `--yes` skips confirmation. Compose-owned projects
   are stopped with `docker compose -p PROJECT down`; other attached
   containers are stopped directly. Nothing is started back up. `--report
   FILE` is cleared before work and records completed actions as tab-separated
   lines (`down<TAB>PROJECT<TAB>service,service` or
   `stop<TAB>CONTAINER`). If a stop command fails after it may have partly
   acted, an `uncertain<TAB>...` line and warning are recorded; inspect state
   and restart only what is needed. The report is recovery guidance, not an
   automatic restart plan.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | success |
| 1 | failed **before** the target changed — it holds its previous contents |
| 2 | the target no longer holds its previous contents — empty, partial, or removed |
| 3 | declined at the prompt — nothing was changed |

Capture never returns 2. Any other code (for example 127) means the tool never
ran; read it as 1. Restore exit 2 means the target may have changed (cleared,
partially restored, created, or removed); exit 1 is a refusal/failure before
the restore changed the target. Exit 3 is an explicit prompt decline with no
data change.

These commands stop containers and bring Compose projects down on a shared
daemon. They do not restart either. Confirm that stopping the volume's users
is acceptable before running capture or restore, especially on a shared host.

## Labels

`--label KEY=VALUE` (repeatable) stamps labels on the target volume. The tool
invents no key. Docker cannot relabel an existing volume, so a target whose
labels differ is removed and recreated with exactly the labels given — sound
only here, because restore replaces the contents anyway. A volume that cannot
or must not be removed (attached container, non-default driver) keeps its
labels and is cleared in place.

## Examples

Copy one database volume into another:

```sh
docker extras volume seed capture --from-volume myapp-demo_database_data --name myapp --image mysql:8.0
docker extras volume seed restore --to-volume dev-myapp_database_data --name myapp
```

Seed a volume so the compose stack that owns it adopts it silently:

```sh
docker extras volume seed restore --to-volume dev-myapp_database_data --name myapp \
  --allow-create --yes \
  --label com.docker.compose.project=dev-myapp \
  --label com.docker.compose.volume=database_data
```
