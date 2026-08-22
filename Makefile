BINARY  := bin/coding-agent-loop
PKG     := ./...
GOFILES := $(shell find . -name '*.go' -not -path './vendor/*')
CONFIG  ?= config.json
MODELS  ?= models.json

# EMBED_CONFIG/EMBED_MODELS name the exact files go:embed compiles into the
# binary (embedded.go at the repo root can only embed "config.json" and
# "models.json" literally — go:embed patterns aren't parameterizable). They
# are deliberately NOT overridable like CONFIG/MODELS above: those two only
# affect which file the built binary is told to read via --config at runtime,
# which is a different question from what got compiled in as its fallback.
EMBED_CONFIG := config.json
EMBED_MODELS := models.json

.PHONY: all help build build-check build-summary config check run once dry-run \
        install uninstall print-service \
        test coverage vet fmt fmt-check lint staticcheck vulcheck ci tidy clean

all: build

## help: list available targets
help:
	@grep -E '^## ' Makefile | sed 's/^## /  /'

## build-check: refuse to build unless config.json and models.json exist to be embedded
build-check:
	@missing=""; \
	test -f "$(EMBED_CONFIG)" || missing="$$missing $(EMBED_CONFIG)"; \
	test -f "$(EMBED_MODELS)" || missing="$$missing $(EMBED_MODELS)"; \
	if [ -n "$$missing" ]; then \
		echo "coding-agent-loop: refusing to build, missing:$$missing"; \
		echo "  go:embed compiles these into the binary as its fallback config/model ladder; run 'make config' to create $(EMBED_CONFIG) from config.example.json, and make sure $(EMBED_MODELS) exists alongside it"; \
		exit 1; \
	fi

## build-summary: print the config.json and models.json content that will be compiled into the binary
build-summary:
	@echo "== $(EMBED_CONFIG) (compiled in as the fallback config) =="; \
	if command -v jq >/dev/null 2>&1; then \
		jq '{label: .github.label, owners: .github.owners, exclude_repos: .github.exclude_repos, poll_interval: .github.poll_interval, search_limit: .github.search_limit, max_concurrent_repos: .run.max_concurrent_repos, discord_enabled: .discord.enabled}' "$(EMBED_CONFIG)"; \
	else \
		cat "$(EMBED_CONFIG)"; \
	fi; \
	echo "== $(EMBED_MODELS) (compiled in as the fallback model ladder) =="; \
	if command -v jq >/dev/null 2>&1; then \
		jq '[.models[] | {id, alias, roles, priority}]' "$(EMBED_MODELS)"; \
	else \
		cat "$(EMBED_MODELS)"; \
	fi

build: build-check build-summary
	go build -ldflags="-w -s" -o $(BINARY) ./cmd

## config: create config.json from config.example.json if it doesn't exist yet
config:
	@test -f $(CONFIG) && echo "$(CONFIG) already exists" || cp config.example.json $(CONFIG)

## check: run start-up checks (binaries, auth, config) and exit
check: build
	$(BINARY) --config $(CONFIG) --check

## run: start the daemon in the foreground
run: build
	$(BINARY) --config $(CONFIG)

## once: single discovery pass, then exit
once: build
	$(BINARY) --config $(CONFIG) --once

## dry-run: full pipeline, but never push, open a PR, or edit an issue
dry-run: build
	$(BINARY) --config $(CONFIG) --once --dry-run --log-level debug

## install: install + enable + start the systemd unit (requires root)
install: build
	sudo $(BINARY) --install --config $(CONFIG)

## uninstall: stop, disable, and remove everything `make install` created (requires root)
# Deliberately does not depend on `build`: uninstalling must work even without
# a config.json/models.json in place, since removing a broken install is
# exactly when those might be missing or invalid. If a binary from a previous
# `make build`/`make install` already exists, it's reused as-is instead of
# rebuilding, since go:embed now requires config.json/models.json to be
# present at compile time — a state uninstall must still be able to run in.
uninstall:
	@if [ ! -x "$(BINARY)" ]; then \
		echo "no existing $(BINARY); building one (requires $(EMBED_CONFIG) and $(EMBED_MODELS) at the repo root, since go:embed compiles them in)"; \
		go build -ldflags="-w -s" -o $(BINARY) ./cmd; \
	fi
	sudo $(BINARY) --uninstall

## print-service: print the embedded systemd unit without installing anything
print-service: build
	$(BINARY) --print-service

## test: run the full test suite with the race detector
test:
	go test -race $(PKG)

## coverage: run tests and print per-function coverage
coverage:
	go test -race -coverprofile=coverage.out $(PKG)
	go tool cover -func=coverage.out

vet:
	go vet $(PKG)

fmt:
	gofmt -w $(GOFILES)

## fmt-check: fail if any file is not gofmt-formatted (CI-friendly, no writes)
fmt-check:
	@unformatted="$$(gofmt -l $(GOFILES))"; \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt-formatted:"; echo "$$unformatted"; exit 1; \
	fi

## lint: vet + formatting check
lint: vet fmt-check

## staticcheck: run staticcheck (must be installed: go install honnef.co/go/tools/cmd/staticcheck@latest)
staticcheck:
	staticcheck $(PKG)

## vulcheck: run govulncheck (must be installed: go install golang.org/x/vuln/cmd/govulncheck@latest)
vulcheck:
	govulncheck $(PKG)

## ci: everything CI should run — lint, then the full test suite
ci: lint test

tidy:
	go mod tidy

clean:
	rm -rf bin coverage.out
