PLUGIN_DIR ?= $(HOME)/.docker/cli-plugins
PLUGIN_NAME ?= extras
BIN_DIR ?= $(HOME)/.local/bin
# Non-empty NO_PLUGIN installs only the PATH binary, like DOCKER_EXTRAS_NO_PLUGIN.
NO_PLUGIN ?=

.PHONY: install uninstall install-bin uninstall-bin install-plugin uninstall-plugin lint test check go-test go-build package-test hooks changelog release-notes dist checksums

# Install docker-extras into BIN_DIR (meant for PATH) and, unless NO_PLUGIN is
# set, link it into PLUGIN_DIR as the nested Docker CLI plugin. Docker runs a
# plugin through the path it found, so the link name sets the Docker command.
install: install-bin
	@if [ -z "$(NO_PLUGIN)" ]; then $(MAKE) install-plugin; fi

uninstall: uninstall-plugin uninstall-bin

install-bin:
	mkdir -p build
	go build -o build/docker-extras ./cmd/docker-extras
	install -d "$(BIN_DIR)"
	install -m 0755 build/docker-extras "$(BIN_DIR)/docker-extras"

uninstall-bin:
	rm -f "$(BIN_DIR)/docker-extras"

# The plugin is a symlink to the PATH binary; install-bin must run first.
install-plugin:
	test -x "$(BIN_DIR)/docker-extras"
	install -d "$(PLUGIN_DIR)"
	ln -sfn "$(BIN_DIR)/docker-extras" "$(PLUGIN_DIR)/docker-$(PLUGIN_NAME)"

# Remove the plugin only while it is still this checkout's link.
uninstall-plugin:
	@link="$(PLUGIN_DIR)/docker-$(PLUGIN_NAME)"; \
	if [ -L "$$link" ] && [ "$$(readlink "$$link")" = "$(BIN_DIR)/docker-extras" ]; then \
		rm -f "$$link"; \
	elif [ -e "$$link" ] || [ -L "$$link" ]; then \
		echo "leaving $$link: it is not a link to $(BIN_DIR)/docker-extras" >&2; \
	fi

lint:
	shellcheck .agents/setup .agents/resume .agents/update-wip bin/docker-* plugin/docker-extras tests/*.sh contrib/release contrib/check-commit-msg scripts/install.sh scripts/uninstall.sh

test:
	bash tests/docker-extras-volume-seed-test.sh

check: lint go-test test package-test

# Keep Go tests and static analysis in the required gate; the released plugin
# binary is built separately for each supported operating system/architecture.
go-test:
	go test ./...
	go vet ./...

go-build:
	mkdir -p build
	go build -o build/docker-extras ./cmd/docker-extras

package-test: checksums
	bash tests/install-uninstall-test.sh dist

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

# Release archives hold the one binary at its historical plugin/docker-extras
# path, so every installer can read every release's archive. The tag workflow
# builds the exact checked-out commit; VERSION is embedded at archive build
# time rather than committed.
dist:
	rm -rf dist/stage
	mkdir -p dist/stage/plugin
	VERSION="$$(git describe --tags --match 'v[0-9]*' --always --dirty)"; \
	VERSION="$${VERSION#v}"; \
	cp LICENSE dist/stage/LICENSE; \
	for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
		os="$${target%/*}"; arch="$${target#*/}"; \
		CGO_ENABLED=0 GOOS="$$os" GOARCH="$$arch" go build -trimpath \
			-ldflags "-s -w -X main.version=$$VERSION" \
			-o dist/stage/plugin/docker-extras ./cmd/docker-extras || exit 1; \
		tar -czf "dist/docker-extras-$$os-$$arch.tar.gz" -C dist/stage plugin/docker-extras LICENSE || exit 1; \
	done
	rm -rf dist/stage
	cp scripts/install.sh dist/docker-extras-install.sh
	cp scripts/uninstall.sh dist/docker-extras-uninstall.sh

# Explicit names, not a glob: dist/ also collects non-release files
# (RELEASE_NOTES.md), and a glob would silently checksum whatever happens
# to be there.
checksums: dist
	cd dist && \
	files="docker-extras-linux-amd64.tar.gz docker-extras-linux-arm64.tar.gz docker-extras-darwin-amd64.tar.gz docker-extras-darwin-arm64.tar.gz docker-extras-install.sh docker-extras-uninstall.sh"; \
	if command -v sha256sum >/dev/null 2>&1; then \
		sha256sum $$files > SHA256SUMS; \
	elif command -v shasum >/dev/null 2>&1; then \
		shasum -a 256 $$files > SHA256SUMS; \
	else \
		echo "error: sha256sum or shasum is required" >&2; exit 1; \
	fi
