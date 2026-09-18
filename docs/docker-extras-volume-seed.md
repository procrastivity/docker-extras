# docker-extras-volume-seed

Archive a Docker named volume to a seed file, and restore a seed file into
another named volume. The tool knows only Docker: compose projects are
discovered from the labels Docker Compose stamps on volumes and containers,
never from compose files or application layout.

```
docker-extras-volume-seed capture --from-volume VOL [--name NAME] [--data-dir DIR]
                           [--image IMG] [--yes] [--report FILE]
docker-extras-volume-seed restore --to-volume VOL [--name NAME] [--data-dir DIR]
                           [--allow-create] [--label KEY=VALUE]...
                           [--expect-image IMG] [--yes] [--force]
                           [--no-verify] [--report FILE]
```

`--help` carries the full option reference; this page explains the design.

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
   stop, and ask before stopping. Nothing is started back up. `--report FILE`
   records what was actually stopped, one tab-separated line each
   (`down<TAB>PROJECT<TAB>service,service` or `stop<TAB>CONTAINER`), so a
   wrapper can restart exactly that.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | success |
| 1 | failed **before** the target changed — it holds its previous contents |
| 2 | the target no longer holds its previous contents — empty, partial, or removed |
| 3 | declined at the prompt — nothing was changed |

Capture never returns 2. Any other code (for example 127) means the tool never
ran; read it as 1.

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
docker-extras-volume-seed capture --from-volume myapp-demo_database_data --name myapp --image mysql:8.0
docker-extras-volume-seed restore --to-volume dev-myapp_database_data --name myapp
```

Seed a volume so the compose stack that owns it adopts it silently:

```sh
docker-extras-volume-seed restore --to-volume dev-myapp_database_data --name myapp \
  --allow-create --yes \
  --label com.docker.compose.project=dev-myapp \
  --label com.docker.compose.volume=database_data
```
