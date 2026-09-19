PREFIX ?= $(HOME)/.local
BINDIR = $(PREFIX)/bin
PLUGIN_DIR ?= $(HOME)/.docker/cli-plugins

TOOLS = $(wildcard bin/docker-*)

.PHONY: install uninstall install-plugin uninstall-plugin lint test check hooks changelog release-notes dist checksums

install:
	install -d "$(BINDIR)"
	install -m 0755 $(TOOLS) "$(BINDIR)/"

uninstall:
	cd bin && for t in docker-*; do rm -f "$(BINDIR)/$$t"; done

# Symlink, not copy: the dispatcher resolves tools via its own real location,
# so the link keeps a checkout's bin/ preferred over PATH.
install-plugin:
	install -d "$(PLUGIN_DIR)"
	ln -sf "$(abspath plugin/docker-extras)" "$(PLUGIN_DIR)/docker-extras"

uninstall-plugin:
	rm -f "$(PLUGIN_DIR)/docker-extras"

lint:
	shellcheck .agents/setup .agents/resume bin/docker-* plugin/docker-extras tests/*.sh contrib/release contrib/check-commit-msg scripts/install.sh

test:
	bash tests/docker-extras-volume-seed-test.sh

check: lint test

# Both hook types on purpose: the commit-msg hook does not install with
# the default stage (the wip/duo family learned this the hard way).
hooks:
	pre-commit install --hook-type pre-commit --hook-type commit-msg

# CHANGELOG.md is generated on demand, never committed (ste9/toolsmith
# C6.3: nothing in the repo can disagree with the tag it was built from).
# The release notes travel in the annotated tag message instead — see
# contrib/release — so this target is a local convenience, not a release
# input.
changelog:
	mkdir -p dist
	git-cliff --output dist/CHANGELOG.md

# The release body: contrib/release writes this file into the annotated
# tag message, and release.yml reads it back with --notes-from-tag. TAG
# may name a tag that already exists (a re-render) or one about to be cut
# (contrib/release, local dry runs); those need different git-cliff
# selections, so probe for the ref first.
release-notes:
	@test -n "$(TAG)" || { echo "error: TAG is required, e.g. make release-notes TAG=v0.0.1" >&2; exit 1; }
	@mkdir -p dist
	@if git rev-parse -q --verify "refs/tags/$(TAG)" >/dev/null; then \
		git-cliff --current --strip header --output dist/RELEASE_NOTES.md; \
	else \
		git-cliff --unreleased --tag "$(TAG)" --strip header --output dist/RELEASE_NOTES.md; \
	fi

# The release tarball: exactly what the installer needs, from HEAD, so an
# uncommitted change can never leak into a release asset. The staged
# plugin's VERSION is stamped from git describe at archive time — the tag,
# on a release build — so no release commit has to carry it; a build
# between tags stamps the describe suffix (-N-gSHA, -dirty) honestly.
# The installer is copied here too, so `checksums` can own the whole
# SHA256SUMS manifest.
dist:
	rm -rf dist/stage
	mkdir -p dist/stage
	git archive --format=tar HEAD bin plugin LICENSE README.md | tar -x -C dist/stage
	VER="$$(git describe --tags --match 'v[0-9]*' --always --dirty)"; \
	sed -i -E "s/^VERSION=\"[^\"]*\"/VERSION=\"$${VER#v}\"/" dist/stage/plugin/docker-extras; \
	grep -q "^VERSION=\"$${VER#v}\"$$" dist/stage/plugin/docker-extras || \
		{ echo "error: could not stamp VERSION in the staged plugin" >&2; exit 1; }
	tar -czf dist/docker-extras.tar.gz -C dist/stage .
	rm -rf dist/stage
	cp scripts/install.sh dist/docker-extras-install.sh

# Explicit names, not a glob: dist/ also collects non-release files
# (RELEASE_NOTES.md), and a glob would silently checksum whatever happens
# to be there.
checksums:
	cd dist && sha256sum docker-extras.tar.gz docker-extras-install.sh > SHA256SUMS
