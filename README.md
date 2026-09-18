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
pins a version, `DOCKER_EXTRAS_INSTALL_DIR` changes the destination):

```sh
curl -fsSL https://github.com/procrastivity/docker-extras/releases/latest/download/docker-extras-install.sh | sh
```

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

docker extras                # list the tools
docker extras volume-seed --help
```

The symlink points into your checkout, so the plugin always runs the
checkout's `bin/` and falls back to PATH.

## Development

```sh
make hooks                   # pre-commit + commit-msg hooks (Conventional Commits)
make lint                    # shellcheck on bin/, plugin/, contrib/, scripts/, tests/
make test                    # regression harness (needs a docker daemon; skips without one)
make check                   # lint + test, the release gate
```

With [nix](https://install.determinate.systems) and direnv, `direnv allow`
loads a dev shell pinning shellcheck, git-cliff, gh, and pre-commit;
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
`docker-extras.tar.gz`, `docker-extras-install.sh`, and `SHA256SUMS`.
`make changelog` renders a full local changelog into `dist/` on demand.

## License

[MIT](LICENSE)
