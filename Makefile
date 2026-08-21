BINARY  := bin/coding-agent-loop
PKG     := ./...
GOFILES := $(shell find . -name '*.go' -not -path './vendor/*')
CONFIG  ?= config.json

.PHONY: all help build config check run once dry-run \
        install uninstall print-service \
        test coverage vet fmt fmt-check lint ci tidy clean

all: build

## help: list available targets
help:
	@grep -E '^## ' Makefile | sed 's/^## /  /'

build:
	go build -o $(BINARY) ./cmd

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

## uninstall: stop, disable, and remove the systemd unit (requires root; leaves /opt/coding-agent-loop in place)
uninstall:
	sudo systemctl disable --now coding-agent-loop.service 2>/dev/null || true
	sudo rm -f /etc/systemd/system/coding-agent-loop.service
	sudo systemctl daemon-reload

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

## ci: everything CI should run — lint, then the full test suite
ci: lint test

tidy:
	go mod tidy

clean:
	rm -rf bin coverage.out
