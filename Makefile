# RepoChat — build, test and release targets. Requires Go 1.25+ and git.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
LDFLAGS := -s -w -X github.com/nalyk/repochat/internal/cli.Version=$(VERSION) -X github.com/nalyk/repochat/internal/cli.Commit=$(COMMIT)
GOFLAGS := -trimpath
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: build install test test-race test-onboarding test-contract fuzz vet check dist clean

build: ## build ./repochat for this machine
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o repochat .

install: build ## copy the binary to ~/.local/bin
	./repochat install

vet:
	go vet ./...
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }

test: ## unit and integration tests (stand-in tunnel-client)
	go test -count=1 ./...

test-race:
	go test -race -count=1 ./...

test-onboarding: build ## first-run flow through a real pseudo-terminal
	go test -c -o internal/cli/cli.test ./internal/cli/
	python3 test/onboarding_pty.py ./repochat internal/cli/cli.test
	@rm -f internal/cli/cli.test

# Contract suite against the REAL tunnel-client and a local mock control plane.
# Needs the helper: `./repochat install-helper` (or set TUNNEL_CLIENT=/path).
TUNNEL_CLIENT ?= $(shell ls $${REPOCHAT_DATA_DIR:-$${XDG_DATA_HOME:-$$HOME/.local/share}/repochat}/bin/tunnel-client 2>/dev/null)
test-contract:
	REPOCHAT_REQUIRE_TUNNEL_CLIENT=1 REPOCHAT_REAL_TUNNEL_CLIENT='$(TUNNEL_CLIENT)' \
		go test -count=1 -v -run RealTunnelClient ./internal/cli/

fuzz: ## short fuzzing pass over the input validators
	go test -run '^$$' -fuzz '^FuzzCheckPath$$' -fuzztime 20s ./internal/repo/
	go test -run '^$$' -fuzz '^FuzzGlob$$' -fuzztime 20s ./internal/repo/
	go test -run '^$$' -fuzz '^FuzzRevision$$' -fuzztime 20s ./internal/repo/
	go test -run '^$$' -fuzz '^FuzzParseChanges$$' -fuzztime 20s ./internal/repo/

check: vet test-race test-onboarding ## everything CI runs

dist: ## cross-compile release archives with checksums into ./dist
	@rm -rf dist && mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; name=repochat-$(VERSION)-$$os-$$arch; \
		echo "building $$name"; mkdir -p dist/$$name; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/$$name/repochat . || exit 1; \
		cp README.md LICENSE dist/$$name/ || exit 1; \
		tar -C dist -czf dist/$$name.tar.gz $$name && rm -rf dist/$$name; \
	done
	@cd dist && (sha256sum *.tar.gz 2>/dev/null || shasum -a 256 *.tar.gz) > SHA256SUMS.txt && cat SHA256SUMS.txt

clean:
	rm -rf dist repochat internal/cli/cli.test
