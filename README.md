# docker-extras

Small `docker-*` developer-experience utilities, in the spirit of
[git-extras](https://github.com/tj/git-extras): each tool does one job, knows
only Docker, and knows nothing about any surrounding application or
infrastructure.

## Tools

| Tool | What it does |
| --- | --- |
| `docker extras volume seed` ([design and safety](docs/docker-extras-volume-seed.md)) | Archive a named volume to a verified seed file, and restore a seed into another named volume — with an image lock, integrity checks before anything destructive, and honest exit codes. |

Every tool prints its full usage with `--help`.

## Install

From the latest release. The installer selects and verifies a plugin archive
for Linux/macOS on amd64/arm64, then installs only `docker extras` in
`~/.docker/cli-plugins`. `DOCKER_EXTRAS_VERSION=vX.Y.Z` pins a release, and
`DOCKER_EXTRAS_PLUGIN_NAME=tools` installs `docker tools` instead:

```sh
curl -fsSL https://github.com/procrastivity/docker-extras/releases/latest/download/docker-extras-install.sh | sh
```

To install the plugin under another safe Docker command name:

```sh
curl -fsSL https://github.com/procrastivity/docker-extras/releases/latest/download/docker-extras-install.sh | DOCKER_EXTRAS_PLUGIN_NAME=tools sh
```

The installer records the plugin path and checksum under
`${XDG_STATE_HOME:-~/.local/state}/docker-extras`. When upgrading a v1 Bash
installation, it verifies every recorded file and every destination before
changing anything; a modified old tool or plugin blocks the whole migration.
After a successful audit it removes only unchanged, installer-owned Bash
files and atomically replaces the ownership record. `DOCKER_EXTRAS_NO_PLUGIN`
and an explicitly set `DOCKER_EXTRAS_INSTALL_DIR` are rejected because this
release has no standalone-tool install mode.

```sh
curl -fsSL https://github.com/procrastivity/docker-extras/releases/latest/download/docker-extras-uninstall.sh | sh
```

The uninstaller removes only files whose recorded checksums still match. It
refuses to guess without an install record and refuses to remove files that
were modified after installation. Docker plugin names must start with a
lowercase letter and contain only lowercase letters and digits.

From a checkout, install the Go CLI plugin (no flat or standalone launcher):

```sh
make install                 # installs docker extras into ~/.docker/cli-plugins
make install PLUGIN_NAME=tools
```

### Docker CLI plugin

```sh
make install-plugin          # builds and installs docker-extras
make install-plugin PLUGIN_NAME=tools  # expose it as docker tools
make uninstall-plugin PLUGIN_NAME=tools

docker extras volume seed --help
docker extras volume seed capture --help
docker extras volume seed restore --help
```

Capture and restore require the Docker CLI and a reachable Docker Engine
daemon; Compose-owned projects additionally require the Docker Compose CLI
plugin. The daemon must be able to bind-mount the local seed directory. In
particular, a remote Docker context does not automatically share files with
the client host, so capture/restore refuse when the daemon cannot read and
write that directory. On a shared host, these operations stop users of the
selected volume and do not restart them; coordinate with other users before
running them.

Docker shell completion is a separate opt-in integration, not installed by
this plugin. Generate and enable Docker's completion script for your shell
using `docker completion bash`, `docker completion zsh`, or the corresponding
supported shell command, following the instructions printed by that command.
Once enabled, Docker asks the plugin for the nested `volume seed` command and
static flag completions.

## Development

```sh
make hooks                   # pre-commit + commit-msg hooks (Conventional Commits)
make lint                    # shellcheck on bin/, plugin/, contrib/, scripts/, tests/
make test                    # Go plugin Docker regression
make package-test            # cross-platform archives + installer migration checks
make go-test                 # Go tests (including daemon tests) + go vet
make check                   # lint + Go + Docker + packaging gates, the CI/release gate
make go-build                # build it to build/docker-extras
```

In an Amp orb, `.agents/setup` installs Docker Engine and `.amp/services.yaml`
declares its supervised daemon. Start declared services before running the
Docker-backed checks:

```sh
amp orb services ensure
make check
```

With [nix](https://install.determinate.systems) and direnv, `direnv allow`
loads a dev shell with Go 1.26.7 from the locked nixpkgs input, alongside
shellcheck, git-cliff, gh, and pre-commit;
without nix, bring those tools yourself.

The port reused Toolsmith's Cobra-based Go CLI and injected stdout/stderr
conventions (Toolsmith checkout `8ce9d56faaaa60aa8c2bde05df6e682083bc52aa`,
module `github.com/procrastivity/toolsmith`, Go 1.23; Toolsmith contract
v1.5), plus a structured command error and explicit exit-code mapping. This
is dogfood, not a claim of Toolsmith contract conformance. The plugin reuses
Cobra and injected streams, but adapts errors to the capture/restore exit
contract (including exit 2 for a possibly changed target and exit 3 for a
decline); it omits Toolsmith's manifest/harness projection, global
`--json`/`--verbose` chassis, and harness-install model because this binary
is a Docker CLI plugin. Packaging also differs: Docker discovers an
executable named `docker-<plugin-name>` and invokes its metadata handshake,
whereas Toolsmith's harness artifacts are projections from its installed
binary. Toolsmith contract v1.5's `[check]` clauses and checker cover marked
mechanical source/build properties, not behavioral parity, safety, Docker
integration, or conformance merely because some conventions are shared.

Want to add a tool? Read [CONTRIBUTING.md](CONTRIBUTING.md) — the admission
bar is the point of the collection.

## Releasing

```sh
contrib/release --patch | --minor | --major | vX.Y.Z
```

The script gates on `REQUIRE_DOCKER=1 make check`, generates the release notes with
[git-cliff](https://github.com/orhun/git-cliff), writes them into the
annotated tag message, and pushes the branch and the tag. It creates no
commit, and no CHANGELOG.md is committed: the notes travel in the tag.
The tag workflow re-runs the gate, builds four plugin-only archives, and
publishes the GitHub Release — body from the tag message (`--notes-from-tag`),
assets `docker-extras-{linux,darwin}-{amd64,arm64}.tar.gz`, both installer
scripts, and `SHA256SUMS`. Cross-builds are produced for all four targets;
the project does not claim macOS or ARM runtime coverage unless executed on
those platforms.
`make changelog` renders a full local changelog into `dist/` on demand.

## License

[MIT](LICENSE)
