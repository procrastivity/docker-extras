PREFIX ?= $(HOME)/.local
BINDIR = $(PREFIX)/bin
PLUGIN_DIR ?= $(HOME)/.docker/cli-plugins

TOOLS = $(wildcard bin/docker-*)

.PHONY: install uninstall install-plugin uninstall-plugin lint test hooks changelog dist checksums

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
	shellcheck bin/docker-* plugin/docker-extras tests/*.sh contrib/release contrib/check-commit-msg scripts/install.sh

test:
	bash tests/docker-extras-volume-seed-test.sh

hooks:
	install -m 0755 contrib/check-commit-msg .git/hooks/commit-msg

changelog:
	git-cliff $(if $(TAG),--tag $(TAG)) --output CHANGELOG.md

# The release tarball: exactly what the installer needs, from HEAD, so an
# uncommitted change can never leak into a release asset.
dist:
	mkdir -p dist
	git archive --format=tar.gz -o dist/docker-extras.tar.gz HEAD bin plugin LICENSE README.md

# Explicit names, not a glob: dist/ also collects non-release files
# (RELEASE_NOTES.md in CI), and a glob would silently checksum whatever
# happens to be there.
checksums:
	cd dist && sha256sum docker-extras.tar.gz > SHA256SUMS
