.PHONY: gen gen-check cover-changed build size clean test test-race test-coverage test-coverage-check test-fuzz bench install check ci fmt imports fumpt staticcheck revive vet errcheck nilaway gosec ineffassign deadcode govulncheck

# Build variables
# maestro-d is maestro-runner plus the daemon commands.
BINARY_NAME=maestro-d
COMMANDS=maestro-d maestro-d-hub
VERSION?=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT?=$(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_DATE?=$(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS=-ldflags "-X github.com/devicelab-dev/maestro-runner/pkg/version.Version=${VERSION} -X github.com/devicelab-dev/maestro-runner/pkg/version.Commit=${COMMIT} -X github.com/devicelab-dev/maestro-runner/pkg/version.BuildDate=${BUILD_DATE}"

# Install directory. maestro-d keeps its own home rather than sharing
# maestro-runner's, so the two can be installed side by side; the binary
# resolves <home> as the parent of its bin/ directory.
INSTALL_DIR?=$(if $(MAESTRO_D_HOME),$(MAESTRO_D_HOME),$(HOME)/.maestro-d)

# Go commands
GOCMD=go
# -trimpath keeps the build machine's absolute paths out of the binary, so a
# panic reported by a user does not carry the builder's home directory through
# every stack frame (#149).
GOBUILD=$(GOCMD) build -trimpath
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOVET=$(GOCMD) vet
GOMOD=$(GOCMD) mod

# Build targets
all: build

# Regenerate the command tables (Go, TypeScript, docs) from the flow parser.
gen:
	$(GOCMD) run ./pkg/daemon/gen

gen-check:
	$(GOCMD) run ./pkg/daemon/gen -check

build:
	$(GOBUILD) $(LDFLAGS) -o ./ ./cmd/...
	@mkdir -p "$(INSTALL_DIR)/bin"
	@set -e; for cmd in $(COMMANDS); do \
		cp "$$cmd" "$(INSTALL_DIR)/bin/.$$cmd.new"; \
		mv -f "$(INSTALL_DIR)/bin/.$$cmd.new" "$(INSTALL_DIR)/bin/$$cmd"; \
	done
	@if [ -d drivers ]; then cp -r drivers "$(INSTALL_DIR)/"; fi

size: build
	@ls -lh $(COMMANDS)

define build-target
	@set -e; for cmd in $(COMMANDS); do \
		GOOS=$(1) GOARCH=$(2) $(GOBUILD) $(LDFLAGS) -o "$$cmd-$(1)-$(2)$(3)" "./cmd/$$cmd"; \
	done
endef

build-linux:
	$(call build-target,linux,amd64)

build-darwin:
	$(call build-target,darwin,amd64)
	$(call build-target,darwin,arm64)

build-windows:
	$(call build-target,windows,amd64,.exe)

build-all: build-linux build-darwin build-windows

install:
	$(GOCMD) install -trimpath $(LDFLAGS) ./cmd/...

clean:
	$(GOCLEAN)
	rm -f $(BINARY_NAME)
	rm -f $(BINARY_NAME)-*

test:
	$(GOTEST) -v ./...

test-race:
	$(GOTEST) -v -race ./...

test-coverage:
	$(GOTEST) -v -race -coverprofile=coverage.out -covermode=atomic ./...
	$(GOCMD) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

test-coverage-check:
	$(GOTEST) -v -race -coverprofile=coverage.out -covermode=atomic ./...
	@go tool cover -func=coverage.out | grep total | awk '{print $$3}' | sed 's/%//' | \
		awk '{if ($$1 < 80) {print "Coverage " $$1 "% is below 80% threshold"; exit 1} else {print "Coverage " $$1 "% meets 80% threshold"}}'

# maestro-d: statement coverage of the lines this branch adds or changes,
# counting both the unit run and the binaries the e2e tests start (built
# with -cover through MAESTRO_D_E2E_COVERDIR). Fails below 90%.
# The server and command tests drive one another through the package boundary.
# Instrument this group together so those calls count in the package they exercise.
COVER_SHARED_PKGS=./cmd/... ./pkg/daemon/... ./pkg/hubd/... ./pkg/cli/... ./pkg/hub/... ./pkg/fsage/... ./pkg/version/...
COVER_PKGS=$(COVER_SHARED_PKGS) ./pkg/aicheck/... ./pkg/core/... ./pkg/rfb/... ./pkg/driver/computer/... ./pkg/cloudprov/... ./pkg/devicefarm/... ./pkg/execbound/... ./pkg/simulator/... ./pkg/emulator/... ./pkg/executor/... ./pkg/driver/wda/... ./pkg/driver/devicelab_ios/... ./pkg/driver/xcodecache/... ./pkg/driver/browser/... ./pkg/uiautomator2/... ./pkg/maestro/... ./pkg/tracelog/... ./pkg/report/... ./pkg/capture/... ./pkg/driver/mock/... ./pkg/driver/uiautomator2/... ./pkg/driver/devicelab/... ./pkg/device/... ./pkg/netshape/... ./pkg/flow/...
cover-changed:
	@rm -rf .cover && mkdir -p .cover/bin
	$(GOTEST) -race -timeout 30m -coverprofile=coverage.out -covermode=atomic $(filter-out $(COVER_SHARED_PKGS),$(COVER_PKGS))
	MAESTRO_D_E2E_COVERDIR=$(CURDIR)/.cover/bin $(GOTEST) -race -timeout 30m -count=1 -coverprofile=.cover/shared.out -covermode=atomic -coverpkg=$(shell echo $(COVER_SHARED_PKGS) | tr ' ' ',') $(COVER_SHARED_PKGS)
	@tail -n +2 .cover/shared.out >> coverage.out
	@if [ -n "$$(ls .cover/bin)" ]; then $(GOCMD) tool covdata textfmt -i=.cover/bin -o=e2e.out; else rm -f e2e.out; fi
	$(GOCMD) run ./tools/covchanged coverage.out e2e.out

test-fuzz:
	$(GOTEST) -v -fuzz=. -fuzztime=30s ./...

bench:
	$(GOTEST) -v -bench=. -benchmem ./...

# Code quality tools
fmt:
	gofmt -s -w .

imports:
	goimports -w .

fumpt:
	gofumpt -w .

vet:
	$(GOVET) ./...

staticcheck:
	staticcheck ./...

revive:
	revive ./...

errcheck:
	errcheck ./...

nilaway:
	nilaway ./...

gosec:
	gosec ./...

ineffassign:
	ineffassign ./...

deadcode:
	deadcode ./...

govulncheck:
	govulncheck ./...

# Quality check - run all checks (use test-race for race detection)
check: fmt imports fumpt vet staticcheck revive errcheck nilaway gosec ineffassign deadcode govulncheck test-race
	@echo "All checks passed!"

# Full CI check (includes coverage threshold)
ci: fmt imports fumpt vet staticcheck revive errcheck nilaway gosec ineffassign deadcode govulncheck test-coverage-check
	@echo "CI checks passed!"

deps:
	$(GOMOD) download
	$(GOMOD) tidy

# Install dev tools
dev-setup:
	go install golang.org/x/tools/cmd/goimports@latest
	go install mvdan.cc/gofumpt@latest
	go install honnef.co/go/tools/cmd/staticcheck@latest
	go install github.com/mgechev/revive@latest
	go install github.com/kisielk/errcheck@latest
	go install go.uber.org/nilaway/cmd/nilaway@latest
	go install github.com/securego/gosec/v2/cmd/gosec@latest
	go install github.com/gordonklaus/ineffassign@latest
	go install golang.org/x/tools/cmd/deadcode@latest
	go install golang.org/x/vuln/cmd/govulncheck@latest

# Run targets
run:
	$(GOBUILD) $(LDFLAGS) -o $(BINARY_NAME) ./cmd/maestro-d
	./$(BINARY_NAME) $(ARGS)

validate:
	./$(BINARY_NAME) validate $(FLOW)

# Release
release: clean build-all
	@echo "Release builds created:"
	@ls -la $(BINARY_NAME)-*
