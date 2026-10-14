# createrepo-go and createapt-go

Two lightweight package-repository managers built from one Go module:

- **[createrepo-go](docs/createrepo-go.md)** manages dnf/yum (rpm-md)
  repositories for RHEL 8 and later.
- **[createapt-go](docs/createapt-go.md)** manages apt repositories for Debian
  and Ubuntu.

Both work against local disk **and** remote storage (SSH/SFTP, S3, GCS; HTTP(S)
read-only), and update a live repository in place while transferring as little
data as possible: only the small metadata is fetched and rewritten, packages
already present are validated remotely by checksum rather than re-uploaded, and
existing packages are never downloaded. They share the commands (`add`,
`remove`, `create`, `rebuild`, `copy`, `list`, `verify`, `check`), the flags,
the storage backends and the progress display; each tool's page covers its
format-specific behavior.

```sh
createrepo-go add s3://my-bucket/el9 dist/*.rpm
createapt-go  add s3://my-bucket/apt dist/*.deb
```

## Building

Build one tool or both. The binaries are independent: each pulls in only the
code its format needs.

```sh
make build                       # both, into bin/
make createrepo-go               # just one
make createapt-go
make build BINARIES=createapt-go # the same, through the build target

go install github.com/danudey/createrepo-go/cmd/createrepo-go@latest
go install github.com/danudey/createrepo-go/cmd/createapt-go@latest
```

Releases publish signed archives of each binary for every supported platform.

## Layout

| Path | What it holds |
| --- | --- |
| `cmd/createrepo-go`, `cmd/createapt-go` | the two CLIs |
| `pkg/backend` | shared: storage abstraction (local/sftp/s3/gcs/http) with optional `RemoteHasher`, `Lister`, `Copier` and `FileStore` capabilities; refuses paths that escape the root |
| `pkg/progress` | shared: the `--progress` display |
| `pkg/xfer` | shared: fetch, verify, upload and hash files; the verbatim `copy` engine |
| `pkg/publish` | shared: the publish planner (upload, skip, relocate, collect) and the write order that keeps a repository whole |
| `pkg/integrity` | shared: the per-file presence, size and checksum check behind `verify` |
| `pkg/checkcore` | shared: levels, results and collection for `check` |
| `pkg/gpg` | shared: OpenPGP signing, verification and key handling |
| `pkg/compress` | shared: in-process decoding of gzip, zstd, xz and bzip2 metadata |
| `pkg/configfile` | shared: load and save the per-repository JSON config |
| `internal/cli`, `internal/parallel` | shared: command-line plumbing and a bounded worker pool |
| `pkg/rpm/*` | rpm-md: `repodata` (data model and XML), `rpmmeta` (read an RPM), `repo` (load, mutate, publish), `repocheck`, `repoconfig`, `sign`, `takeover` |
| `pkg/apt/*` | apt: `aptdata` (deb822 model, `Packages`/`Sources`/`Release`, version comparison, layout), `debmeta` (read a `.deb` or `.dsc`), `repo`, `repocheck`, `repoconfig`, `sign` |
| `internal/rpm` | a vendored copy of `github.com/cavaliergopher/rpm` (see its `doc.go`) |
| `reference/rpm`, `reference/apt` | test fixtures and the scripts that generate them |
| `test/e2e/rpm`, `test/e2e/apt` | end-to-end suites |

Each format's `repo` package is the library entry point: `Open`/`Add`/`Remove`/
`Commit`, with `OpenWith` accepting a custom backend.

The rule for what is shared: mechanism lives in the shared packages, and
anything that knows a format — what files a repository is made of, how its
metadata is generated, its dependency model, its signing policy — stays in
`pkg/rpm` or `pkg/apt`. A fix to transfers, publishing order, verification or
signing therefore lands in both tools at once.

## Testing

```sh
make test            # fast unit + golden tests for both tools
make test-e2e        # both end-to-end suites (servers/containers; slower)
make test-e2e-rpm    # just one
make test-e2e-apt
make lint
```

The **rpm** unit and golden tests compare generated metadata field-by-field
against `createrepo_c` output for committed sample RPMs, assert the
minimal-transfer behavior, and round-trip GPG signing. Regenerate the fixtures
with `make fixtures-rpm` (needs `rpmbuild` and `createrepo_c`).

The **apt** unit tests compare extracted control stanzas against
`dpkg-deb --field`, cross-check version ordering against
`dpkg --compare-versions`, and round-trip `Release` signing through both the
native OpenPGP path and `gpg`. Their sample packages are built, not committed:
run `make fixtures-apt` (needs `dpkg-deb`) first, or the tests that need them
skip themselves.

The **end-to-end suites** (behind the `e2e` build tag) build each CLI and drive
it against every backend — local disk, a freshly started MinIO for S3, a local
`sshd` for SFTP, a `fake-gcs-server` container for GCS, and a read-only HTTP
server — then install from the result with real clients: AlmaLinux 8/9/10 `dnf`
for rpm, and `apt` for apt. Anything whose dependencies are missing skips
itself. See [`test/e2e/rpm/README.md`](test/e2e/rpm/README.md) and
[`test/e2e/apt/README.md`](test/e2e/apt/README.md) for coverage and
dependencies.
