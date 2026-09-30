# Contributing

The collection stays useful only while every tool clears the same bar
(moreutils keeps its collection coherent the same way). A new tool must
satisfy all of these:

1. **One job.** The tool does one thing a docker user reaches for repeatedly.
   Flags configure that one job; they do not add a second one.
2. **Knows only Docker.** No knowledge of any application, compose file
   layout, CI system, or company infrastructure. Anything environment-specific
   arrives through a flag or an environment variable with a sane default.
3. **Names its damage.** A destructive command says what it will change before
   it changes it, asks unless `--yes`, and its exit codes let a caller tell
   "nothing changed" from "the target changed" (see `docker extras volume seed`'s
   exit-code contract for the model).
4. **Self-documenting.** `--help` carries the full usage, and a header comment
   explains the non-obvious decisions. A page in `docs/` mirrors both.
5. **Tested where it counts.** A harness in `tests/` covers the failure the
   tool exists to prevent, not just the happy path. Harnesses must only touch
   docker objects they created and must clean up from an EXIT trap.
6. **Small implementation, tested gates.** Commands use the Go/Cobra plugin
   tree and Docker CLI; release artifacts expose only the nested Docker plugin.
   Bash compatibility fixtures remain shellcheck-clean, and `make check` passes.

Mechanics:

- Commits are [Conventional Commits](https://www.conventionalcommits.org)
  (git-cliff builds each release's notes from them); `make hooks` installs
  the pre-commit hooks, including the commit-msg hook that enforces it.
- Add commands under `cmd/docker-extras` and tests alongside the owning Go
  behavior; do not add a standalone or flat Docker command alias.
- Add a row to the README table, a page in `docs/`, and focused daemon/failure
  coverage wired into the Makefile's required gates.
