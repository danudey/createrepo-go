.PHONY: build test test-e2e fixtures

# Build the CLI into bin/.
build:
	go build -o bin/createrepo-go ./cmd/createrepo-go

# Fast unit + golden tests (no servers, no containers).
test:
	go test ./...

# End-to-end tests: drive the built binary against local disk, a fresh MinIO
# instance (S3), a local sshd (SFTP), a fake-gcs-server container (GCS), and a
# read-only HTTP server. Backends whose dependencies are absent skip themselves.
# See test/e2e/README.md.
test-e2e:
	go test -tags e2e ./test/e2e/... -timeout 600s

# Regenerate the RPM + createrepo_c reference fixtures (needs rpmbuild + createrepo_c).
fixtures:
	./reference/gen.sh
