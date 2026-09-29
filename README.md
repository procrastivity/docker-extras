# docker-extras

Small `docker-*` developer-experience utilities, in the spirit of
[git-extras](https://github.com/tj/git-extras): each tool does one job, knows
only Docker, and knows nothing about any surrounding application or
infrastructure.

## Tools

| Tool | What it does |
| --- | --- |
| [`docker-extras-volume-seed`](docs/docker-extras-volume-seed.md) | Archive a named volume to a verified seed file, and restore a seed into another named volume — with an image lock, integrity checks before anything destructive, and honest exit codes. |

Every tool prints its full usage with `--help`.

## Install

From the latest release — tools into `~/.local/bin`, plus the `docker extras`
CLI plugin (set `DOCKER_EXTRAS_NO_PLUGIN=1` to skip it; `DOCKER_EXTRAS_VERSION=vX.Y.Z`
pins a version, `DOCKER_EXTRAS_INSTALL_DIR` changes the destination, and
`DOCKER_EXTRAS_PLUGIN_NAME=tools` installs `docker tools` instead):

```sh
curl -fsSL https://github.com/procrastivity/docker-extras/releases/latest/download/docker-extras-install.sh | sh
```

To install the plugin under another safe Docker command name:

```sh
curl -fsSL https://github.com/procrastivity/docker-extras/releases/latest/download/docker-extras-install.sh | DOCKER_EXTRAS_PLUGIN_NAME=tools sh
```

The installer records the exact files it wrote, including the selected plugin
name, under `${XDG_STATE_HOME:-~/.local/state}/docker-extras`. To remove an
installation made by this installer:

```sh
curl -fsSL https://github.com/procrastivity/docker-extras/releases/latest/download/docker-extras-uninstall.sh | sh
```

The uninstaller removes only files whose recorded checksums still match. It
refuses to guess for older installations without a record and refuses to
remove files that were modified after installation. Docker plugin names must
start with a lowercase letter and contain only lowercase letters and digits.

From a checkout:

```sh
make install                 # copies bin/docker-extras-* into ~/.local/bin
make install PREFIX=/usr/local
```

### As a `docker` subcommand (optional)

Docker CLI plugin names allow no hyphens, so the tools cannot be plugins
themselves. One umbrella plugin fronts them instead:

```sh
make install-plugin          # symlinks plugin/docker-extras into ~/.docker/cli-plugins
make install-plugin PLUGIN_NAME=tools  # expose it as docker tools
make uninstall-plugin PLUGIN_NAME=tools

docker extras                # list the tools
docker extras volume-seed --help
```

The symlink points into your checkout, so the plugin always runs the
checkout's `bin/` and falls back to PATH.

## Development

```sh
make hooks                   # pre-commit + commit-msg hooks (Conventional Commits)
make lint                    # shellcheck on bin/, plugin/, contrib/, scripts/, tests/
make test                    # Go plugin Docker regression + installer checks
make go-test                 # Go tests (including daemon tests) + go vet
make check                   # lint + Go checks + Docker integration, the CI/release gate
make go-build                # build it to build/docker-extras (not yet released)
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

Want to add a tool? Read [CONTRIBUTING.md](CONTRIBUTING.md) — the admission
bar is the point of the collection.

## Releasing

```sh
contrib/release --patch | --minor | --major | vX.Y.Z
```

The script gates on `make check`, generates the release notes with
[git-cliff](https://github.com/orhun/git-cliff), writes them into the
annotated tag message, and pushes the branch and the tag. It creates no
commit, and no CHANGELOG.md is committed: the notes travel in the tag.
The tag workflow re-runs the gate, builds the assets (`make dist` stamps
the plugin VERSION from the tag), and publishes the GitHub Release —
body from the tag message (`--notes-from-tag`), assets
`docker-extras.tar.gz`, `docker-extras-install.sh`,
`docker-extras-uninstall.sh`, and `SHA256SUMS`.
`make changelog` renders a full local changelog into `dist/` on demand.

## License

[MIT](LICENSE)
