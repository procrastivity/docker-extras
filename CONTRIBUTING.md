# Contributing

The collection stays useful only while every tool clears the same bar
(moreutils keeps its collection coherent the same way). A new tool must
satisfy all of these:

1. **One job.** The tool does one thing a docker user reaches for repeatedly.
   Flags configure that one job; they do not add a second one.
2. **Knows only Docker.** No knowledge of any application, compose file
   layout, CI system, or company infrastructure. Anything environment-specific
   arrives through a flag or an environment variable with a sane default.
3. **Names its damage.** A destructive tool says what it will change before it
   changes it, asks unless `--yes`, and its exit codes let a caller tell
   "nothing changed" from "the target changed" (see `docker-volume-seed`'s
   exit-code contract for the model).
4. **Self-documenting.** `--help` carries the full usage, and a header comment
   explains the non-obvious decisions. A page in `docs/` mirrors both.
5. **Tested where it counts.** A harness in `tests/` covers the failure the
   tool exists to prevent, not just the happy path. Harnesses must only touch
   docker objects they created and must clean up from an EXIT trap.
6. **Plain bash, shellcheck-clean.** `set -euo pipefail`, no dependencies
   beyond docker and POSIX userland, `make lint` passes.

Mechanics:

- Put the executable in `bin/docker-<name>` (no `.sh` suffix, mode 755).
- Add the name to `TOOLS` in `plugin/docker-extras`.
- Add a row to the README table, a page in `docs/`, and a harness in `tests/`
  wired into the Makefile's `test` target.
