# The binaries this module builds. Build one with `make createrepo-go` or
# `make createapt-go`, or narrow `make build` with BINARIES=createapt-go.
BINARIES ?= createrepo-go createapt-go

.PHONY: build test test-e2e test-e2e-rpm test-e2e-apt fixtures fixtures-rpm fixtures-apt lint tidy createrepo-go createapt-go

# Build the CLIs into bin/.
build: tidy $(BINARIES)

createrepo-go createapt-go:
	go build -o bin/$@ ./cmd/$@

# Fast unit + golden tests (no servers, no containers).
test: tidy
	go tool gotestsum --format testname ./...

# End-to-end tests: drive the built binaries against local disk, a fresh MinIO
# instance (S3), a local sshd (SFTP), a fake-gcs-server container (GCS), and a
# read-only HTTP server, then install from the result with real dnf and apt
# clients. Backends whose dependencies are absent skip themselves. See
# test/e2e/rpm/README.md and test/e2e/apt/README.md.
test-e2e: tidy
	go tool gotestsum --format testname -- -tags e2e ./test/e2e/... -timeout 900s

test-e2e-rpm: tidy
	go tool gotestsum --format testname -- -tags e2e ./test/e2e/rpm/... -timeout 600s

test-e2e-apt: tidy
	go tool gotestsum --format testname -- -tags e2e ./test/e2e/apt/... -timeout 600s

# Regenerate the reference fixtures. The fixtures are committed, so commit the
# result.
fixtures: fixtures-rpm fixtures-apt

# The sample RPMs and a createrepo_c reference repository (needs rpmbuild +
# createrepo_c).
fixtures-rpm:
	./reference/rpm/gen.sh

# The sample .deb and source-package fixtures (needs dpkg-deb).
fixtures-apt:
	./reference/apt/gen.sh

lint:
	golangci-lint run

tidy:
	@go mod tidy
