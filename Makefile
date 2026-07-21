# airport-sdr — build, test and install
#
# Default build is pure Go and cross-compiles anywhere. Hardware and codec
# support live behind build tags (rule 8: the tag set is deliberately small and
# is documented here in full):
#
#   soapy   cgo SoapySDR source          needs libsoapysdr-dev
#   assert  runtime preconditions on     zero cost when absent
#
BINARY  := airport-sdr
PKG     := github.com/octanis/airport-sdr
CMD     := ./cmd/$(BINARY)
BIN_DIR := bin

PREFIX     ?= /usr/local
DESTDIR    ?=
CONFDIR    ?= /etc/$(BINARY)
SYSTEMDDIR ?= /etc/systemd/system

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION) -s -w
GOFLAGS ?=

# Tags for the full hardware build.
FULL_TAGS := soapy

.DEFAULT_GOAL := build
.PHONY: build build-full test watch test-alloc test-assert lint bench \
        cross record install uninstall clean tools help

## build: pure-Go binary (no cgo, cross-compiles anywhere)
build:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) $(CMD)
	@echo "built $(BIN_DIR)/$(BINARY) ($(VERSION), pure Go)"

## build-full: binary with SoapySDR hardware support
build-full:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=1 go build $(GOFLAGS) -tags '$(FULL_TAGS)' \
		-ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) $(CMD)
	@echo "built $(BIN_DIR)/$(BINARY) ($(VERSION), tags: $(FULL_TAGS))"

## test: full test suite under the race detector
test:
	go test -race ./...

## watch: re-run tests on every save (the red-green loop)
watch:
	@command -v inotifywait >/dev/null 2>&1 || { \
		echo "inotifywait not found: apt install inotify-tools"; exit 1; }
	@echo "watching for changes; ctrl-c to stop"
	@while true; do \
		go test ./... || true; \
		inotifywait -qre close_write --include '\.go$$' . >/dev/null 2>&1; \
	done

## test-alloc: assert zero steady-state allocation in the DSP hot path (rule 3)
test-alloc:
	go test -run 'TestAlloc' -v ./internal/dsp/... ./internal/sdr/... ./internal/stream/...

## test-assert: run the suite with runtime preconditions compiled in (rule 5)
test-assert:
	go test -race -tags assert ./...

## lint: vet plus strict static analysis; warnings are errors (rule 10)
lint:
	go vet ./...
	@command -v golangci-lint >/dev/null 2>&1 \
		&& golangci-lint run \
		|| echo "golangci-lint not installed, ran go vet only (make tools)"
	@$(MAKE) --no-print-directory lint-js

## lint-js: syntax-check the browser code, which Go tests cannot reach
lint-js:
	@command -v node >/dev/null 2>&1 || { \
		echo "node not installed, skipping browser syntax check"; exit 0; }
	@for f in internal/web/static/*.js; do node --check "$$f" || exit 1; done
	@echo "browser javascript parses"

## bench: DSP benchmarks, tracked against the edge CPU budget
bench:
	go test -run '^$$' -bench . -benchmem ./internal/dsp/...

## cross: cgo builds for edge targets via docker buildx
cross:
	@echo "cross-compilation is implemented in phase 5"
	@exit 1

## record: capture live IQ to a .cf32 file for the test corpus
record: build-full
	@mkdir -p testdata
	$(BIN_DIR)/$(BINARY) --config configs/config.yaml record \
		--duration $(or $(DURATION),60s) --out testdata/capture.cf32

## replay: demodulate a capture to a WAV file you can listen to
replay: build
	$(BIN_DIR)/$(BINARY) --config configs/config.yaml replay \
		--in testdata/capture.cf32 --out testdata/tower.wav

## tools: install optional developer tooling
tools:
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

## install: install binary, config and service unit
install: build-full
	install -d $(DESTDIR)$(PREFIX)/bin
	install -m 0755 $(BIN_DIR)/$(BINARY) $(DESTDIR)$(PREFIX)/bin/$(BINARY)
	install -d $(DESTDIR)$(CONFDIR)
	@if [ -f $(DESTDIR)$(CONFDIR)/config.yaml ]; then \
		echo "keeping existing $(DESTDIR)$(CONFDIR)/config.yaml"; \
	else \
		install -m 0644 configs/config.yaml $(DESTDIR)$(CONFDIR)/config.yaml; \
	fi
	install -d $(DESTDIR)$(SYSTEMDDIR)
	install -m 0644 packaging/$(BINARY).service $(DESTDIR)$(SYSTEMDDIR)/$(BINARY).service
	@if [ -z "$(DESTDIR)" ] && command -v systemctl >/dev/null 2>&1; then \
		systemctl daemon-reload; \
		echo "installed. enable with: systemctl enable --now $(BINARY)"; \
	fi

## uninstall: remove binary and unit, leaving config in place
uninstall:
	@if [ -z "$(DESTDIR)" ] && command -v systemctl >/dev/null 2>&1; then \
		systemctl disable --now $(BINARY) 2>/dev/null || true; \
	fi
	rm -f $(DESTDIR)$(PREFIX)/bin/$(BINARY)
	rm -f $(DESTDIR)$(SYSTEMDDIR)/$(BINARY).service
	@echo "config left at $(DESTDIR)$(CONFDIR)"

## clean: remove build outputs
clean:
	rm -rf $(BIN_DIR)
	go clean -testcache

## help: list targets
help:
	@grep -hE '^## ' $(MAKEFILE_LIST) | sed 's/## /  /' | sort
