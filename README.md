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
make hooks                   # commit-msg hook: commits must be Conventional Commits
make lint                    # shellcheck on bin/, plugin/, contrib/, scripts/, tests/
make test                    # regression harness (needs a docker daemon; skips without one)
```

Want to add a tool? Read [CONTRIBUTING.md](CONTRIBUTING.md) — the admission
bar is the point of the collection.

## Releasing

```sh
contrib/release --patch | --minor | --major | vX.Y.Z
```

The script gates on `make lint` + `make test`, regenerates
[CHANGELOG.md](CHANGELOG.md) with [git-cliff](https://github.com/orhun/git-cliff),
stamps the plugin version, commits `chore(release): vX.Y.Z`, tags, and pushes.
The tag workflow re-runs the gate and publishes the GitHub Release with
`docker-extras.tar.gz`, `docker-extras-install.sh`, and `SHA256SUMS`.

## License

[MIT](LICENSE)
