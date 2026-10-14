# End-to-end tests

These tests drive the **built `createrepo-go` binary** as a subprocess against
real storage backends, validating the full stack — flag parsing, RPM metadata
generation, minimal-transfer logic, signing, and garbage collection — exactly
as a user would experience it.

A single comprehensive scenario suite (`scenarios_test.go`) is run unchanged
against every writable backend, so behavior is proven identical regardless of
where the repository lives:

| Backend  | How it is provided                                              | Skipped when…            |
| -------- | --------------------------------------------------------------- | ------------------------ |
| `local`  | a temp directory                                                | never                    |
| `s3`     | a fresh **MinIO** server started per run (`AWS_ENDPOINT_URL`)   | `minio` not on `PATH`    |
| `sftp`   | a non-root **sshd** + an in-process ssh-agent on a temp port    | `sshd`/`sftp-server` absent |
| `gcs`    | a **fake-gcs-server** container (`STORAGE_EMULATOR_HOST`)       | `docker` unavailable     |
| `http`   | the published repo served read-only via `net/http` (`TestE2EReadOnlyHTTP`) | never        |

Backends whose dependencies are missing **skip themselves** rather than fail,
so the suite degrades gracefully on a minimal machine while exercising
everything available.

## Real AlmaLinux client validation

`TestE2EClients` / `TestE2EClientUpdate` go a step further: they run actual
**AlmaLinux 8, 9, and 10** containers (docker or podman) that consume the
repositories with real `dnf`, proving the generated metadata and signatures
work with the rpm versions those releases ship (rpm 4.14 / 4.16 / 4.19+).

For every backend and every AlmaLinux version, the test:

1. builds a repository with the **matching `--target` profile** — `rhel8` for
   AlmaLinux 8 (gzip + v4 signature), `rhel9` for 9 (zstd + v4), `rhel10` for
   10 (zstd + openpgp) — signing the metadata and packages when host GPG tooling
   is present;
2. fronts that repository over HTTP (the backend's bytes are streamed through an
   in-process server, so S3/GCS/SFTP repos are exercised exactly as a client
   would reach them via an HTTP mirror);
3. runs an `almalinux:N` container with `--network=host` that imports the key,
   configures the repo with `gpgcheck=1` + `repo_gpgcheck=1`, and runs
   `dnf makecache` + `dnf install` + executes the installed program.

The transaction enables only our repository (`--disablerepo='*'`), so it is
satisfied entirely from the repo plus packages already in the base image — no
external mirrors are contacted. `TestE2EClientUpdate` additionally publishes a
newer package version with `--prune-older` and confirms a client installs the
new version while the superseded one is gone.

This directly validates the compatibility matrix in the top-level README:
zstd metadata is rejected by nothing on 9/10, gzip works on 8, and each
release's rpm accepts the signature format its profile selects.

## Running

```sh
go test -tags e2e ./test/e2e/...
```

The `e2e` build tag keeps these tests out of the default `go test ./...` run
(they start servers and containers and take longer).

Useful invocations:

```sh
# One backend only.
go test -tags e2e ./test/e2e/ -run 'TestE2E/s3-minio' -v

# One scenario across all backends.
go test -tags e2e ./test/e2e/ -run 'TestE2E/.*/repeated_update_stress' -v

# Verbose, with a longer timeout (the GCS container pull can be slow first time).
go test -tags e2e ./test/e2e/ -v -timeout 600s
```

## Optional dependencies

- **minio** — S3 backend. Install the `minio` server binary.
- **docker** or **podman** — GCS backend (pulls `fsouza/fake-gcs-server`
  automatically) and the AlmaLinux client tests (pulls `almalinux:8/9/10`).
  Override the runtime with `CR_E2E_RUNTIME=podman`.
- **sshd**, an OpenSSH **sftp-server** helper, **ssh-keygen** — SFTP backend.
- **gpg**, **rpmsign**, **rpmkeys** — the signing scenarios (`sign_metadata`,
  `sign_packages`, `verify_sigs_gate`).
- **rpmbuild** — builds the bumped `hello-2.10-4` fixture used by the
  update/prune/coexistence scenarios; without it those three scenarios skip.

## What is covered

The suite (`scenarios_test.go`) exercises, per backend:

- repository creation (`create`, and implicit creation on first `add`);
- adding one and many packages; incremental adds against a live repo;
- idempotent re-add (skip), `--force` re-upload, minimal-transfer (an
  already-present RPM is validated remotely and never re-uploaded);
- coexisting versions, `--prune-older`, and automatic garbage collection of a
  pruned package's blob;
- `remove` (blob garbage-collected once unreferenced, `--arch` filter, and the
  no-match error);
- server-side relocation (re-adding an identical RPM under a new
  `--location-prefix` moves it without re-uploading and removes the old copy);
- `--location-prefix`, `--compression zstd`, and `--dry-run` (writes nothing);
- metadata signing (`--sign-metadata`, verified with `gpg`), package signing
  (`--sign-packages`, verified against the key), and the incoming-signature
  gate (`--verify-sigs`);
- a repeated create/update/remove stress loop that asserts superseded metadata
  is garbage-collected on every publish;
- `verify` detecting a missing blob and a corrupted (wrong size/checksum) blob;
- `takeover` analysing a repository without changing it (and `--adopt` writing
  only the config file), and refusing a takeover that would leave a detached
  `repomd.xml.asc` describing metadata that no longer exists.

The read-only HTTP backend runs the read subset (`list`, `verify`) and asserts
that writes are rejected.

On top of that, `TestE2EClients` and `TestE2EClientUpdate` (see above) prove the
output is consumable by real AlmaLinux 8/9/10 `dnf` clients, per profile, across
every backend.
