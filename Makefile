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

.PHONY: all help build build-check build-summary config embed-ready check run once dry-run no-mutate \
        install uninstall purge print-service migrate-config \
        test coverage vet fmt fmt-check lint staticcheck vulcheck ci tidy clean \
				ssh-add

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

build: embed-ready build-check build-summary
	go build -ldflags="-w -s" -o $(BINARY) ./cmd

## config: create config.json from config.example.json if it doesn't exist yet
config:
	@test -f $(CONFIG) && echo "$(CONFIG) already exists" || cp config.example.json $(CONFIG)

# embed-ready: make sure the files go:embed compiles in actually exist.
#
# config.json is gitignored, so a fresh clone or a git worktree — which is
# exactly what the agent builds every run in — has no config.json for
# embedded.go to embed, and the whole module fails to compile with
# "pattern config.json: no matching files found". Every target below that
# compiles anything depends on this, so testing a clean checkout works.
.PHONY: embed-ready
embed-ready:
	@test -f $(EMBED_CONFIG) || cp config.example.json $(EMBED_CONFIG)
	@test -f $(EMBED_MODELS) || { echo "missing $(EMBED_MODELS), which has no example to copy from"; exit 1; }

## check: run start-up checks (binaries, auth, config) and exit
check: build
	$(BINARY) --config $(CONFIG) --check

## run: start the daemon in the foreground
run: build
	$(BINARY) --config $(CONFIG)

## once: single discovery pass, then exit
once: build
	$(BINARY) --config $(CONFIG) --once

## dry-run: report what one pass would do; runs no Claude and costs nothing
dry-run: build
	$(BINARY) --config $(CONFIG) --once --dry-run --log-level debug

## no-mutate: full pipeline including a real (billed) Claude run, but never push, open a PR, or edit an issue
no-mutate: build
	$(BINARY) --config $(CONFIG) --once --no-mutate --log-level debug

## install: install + enable + start the systemd unit (requires root)
install: build
	sudo $(BINARY) --install --config $(CONFIG)

## uninstall: stop, disable, and remove the service; leaves data in place (requires root)
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

## purge: destructive — stop, disable, remove the service, and delete its workspace/logs/state data (requires root)
purge:
	@if [ ! -x "$(BINARY)" ]; then \
		echo "no existing $(BINARY); building one (requires $(EMBED_CONFIG) and $(EMBED_MODELS) at the repo root, since go:embed compiles them in)"; \
		go build -ldflags="-w -s" -o $(BINARY) ./cmd; \
	fi
	sudo $(BINARY) --uninstall --purge

## print-service: print the embedded systemd unit without installing anything
print-service: build
	$(BINARY) --print-service

## migrate-config: bring CONFIG up to the current config.json schema — keeps
## every value you've already set, adds new fields at their default, and
## reports (without failing on) any field the schema has since dropped. The
## original is saved alongside it as CONFIG.bak before anything is written.
migrate-config: build
	$(BINARY) --config $(CONFIG) --migrate-config

## test: run the full test suite with the race detector
test: embed-ready
	go test -race $(PKG)

## coverage: run tests and print per-function coverage
coverage: embed-ready
	go test -race -coverprofile=coverage.out $(PKG)
	go tool cover -func=coverage.out

vet: embed-ready
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
staticcheck: embed-ready
	staticcheck $(PKG)

## vulcheck: run govulncheck (must be installed: go install golang.org/x/vuln/cmd/govulncheck@latest)
vulcheck: embed-ready
	govulncheck $(PKG)

## ci: everything CI should run — lint, then the full test suite
ci: lint test

tidy:
	go mod tidy

clean:
	rm -rf bin coverage.out

ssh-add:
	eval "$(ssh-agent -s)"
	ssh-add ~/.ssh/github
