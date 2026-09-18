# docker-extras

Small `docker-*` developer-experience utilities, in the spirit of
[git-extras](https://github.com/tj/git-extras): each tool does one job, knows
only Docker, and knows nothing about any surrounding application or
infrastructure.

## Tools

| Tool | What it does |
| --- | --- |
| [`docker-volume-seed`](docs/docker-volume-seed.md) | Archive a named volume to a verified seed file, and restore a seed into another named volume — with an image lock, integrity checks before anything destructive, and honest exit codes. |

Every tool prints its full usage with `--help`.

## Install

```sh
make install                 # copies bin/docker-* into ~/.local/bin
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
make lint                    # shellcheck on bin/, plugin/, tests/
make test                    # regression harness (needs a docker daemon; skips without one)
```

Want to add a tool? Read [CONTRIBUTING.md](CONTRIBUTING.md) — the admission
bar is the point of the collection.

## License

[MIT](LICENSE)
