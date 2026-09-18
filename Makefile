PREFIX ?= $(HOME)/.local
BINDIR = $(PREFIX)/bin
PLUGIN_DIR ?= $(HOME)/.docker/cli-plugins

TOOLS = $(wildcard bin/docker-*)

.PHONY: install uninstall install-plugin uninstall-plugin lint test

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
	shellcheck bin/docker-* plugin/docker-extras tests/*.sh

test:
	bash tests/docker-extras-volume-seed-test.sh
