# createrepo-go

A lightweight manager for dnf/yum (rpm-md) repositories, targeting RHEL 8 and
later. It replaces the most common, cumbersome `createrepo_c` workflows with a
single tool that works against local disk **and** remote storage, and updates a
live repository in place while transferring as little data as possible.

The headline use case:

```sh
createrepo-go add <repo> <rpm>...
```

uploads the RPMs and adds them to the live repository metadata, creating the
repository if it does not yet exist.

## Why

- **Native Go engine.** RPM headers are parsed and primary/filelists/other +
  repomd.xml are generated in pure Go — no dependency on `createrepo_c` at
  runtime, locally or remotely. This is what makes S3/GCS targets and truly
  minimal transfer possible.
- **Minimal transfer.** Updating a repository fetches only the (small) existing
  metadata, regenerates it locally, and uploads only new RPMs plus the new
  metadata. Existing RPMs are **never downloaded**.
- **Remote validation.** An RPM already present in the repository is validated
  by checksum without transferring it: over SSH the checksum is computed
  remotely (`sha256sum`), and on S3/GCS it is read from object metadata that the
  tool records on upload.
- **Server-side relocation.** Re-adding an identical RPM at a new location (for
  example under a new `--location-prefix`) moves it in place instead of
  re-uploading it, when the backend supports a server-side copy (local rename,
  SSH `cp`, S3/GCS object copy).
- **Near-atomic publish.** New metadata is written under checksum-named files
  and `repomd.xml` is swapped last, so dnf clients never observe a torn repo.
  Superseded metadata **and any RPM the new metadata no longer references**
  (replaced, pruned, removed, or relocated) are then garbage-collected — the
  publish diffs the old and new metadata, so orphaned files never accumulate.

Only RHEL 8+ is targeted: no sqlite metadata (dnf does not use it), and gzip is
the default metadata compression (zstd is available with `--compression zstd`,
but needs RHEL 8.4+).

### Compatibility targets

`--target` selects defaults appropriate for a release. Explicit `--compression`
and `--signature-format` always override the profile.

| `--target` | compression | package signature |
| --- | --- | --- |
| `rhel8` (aliases `el8`, `alma8`, `rocky8`, …) | gzip | `v4` (RSAHEADER) |
| `rhel9` (`el9`, `alma9`, …) | zstd | `v4` (RSAHEADER) |
| `rhel10` (`el10`, `alma10`, …) | zstd | `openpgp` |

The signature mapping is dictated by what each release's rpm can actually read,
verified with dnf on AlmaLinux 8/9/10 (`gpgcheck=1` and `repo_gpgcheck=1`):

- **rpm 4.14 (RHEL 8)** and **rpm 4.16 (RHEL 9)** read only the legacy v4
  `RSAHEADER` signature. They cannot read the newer `OPENPGP` header tag, so a
  package signed only with OPENPGP is treated as **unsigned** there.
- **rpm 4.19 (RHEL 10)** reads both, so `openpgp` is safe (and gzip is
  superseded by zstd).

Because of this, `v4` is the universally compatible signature and is the default
even with no `--target`. When `--sign-packages` runs on an rpm 6 host (which
writes only OPENPGP by default), the `v4` format adds `rpmsign --rpmv4` so the
legacy signature is present alongside it — the package then installs on RHEL
8/9/10 alike. (Note: RHEL 9 uses zstd metadata but still needs the `v4`
signature — the rpm 6 `OPENPGP` tag is RHEL 10+, not RHEL 9.)

## Backends

The repository location's URL scheme selects the backend:

| Scheme | Example | Notes |
| --- | --- | --- |
| local | `/srv/repo` or `file:///srv/repo` | read/write |
| SSH/SFTP | `sftp://user@host/srv/repo` | uses the ssh-agent and `~/.ssh/known_hosts`; remote `sha256sum` validation |
| S3 | `s3://bucket/prefix` | standard AWS credential chain; `--profile`/`--region` (or `AWS_PROFILE`/`AWS_REGION`); `AWS_ENDPOINT_URL` for S3-compatible stores. The bucket's region is detected automatically, and a wrong `--region` is corrected with a warning naming the right endpoint. MFA-protected assume-role profiles are prompted for on stdin, and the resulting session credentials are cached (see below) so later commands do not ask for another code |
| GCS | `gs://bucket/prefix` | application-default credentials |
| HTTP(S) | `https://host/repo/` | **read-only** (`list`, `verify`) |

### Cached AWS session credentials

An MFA code cannot be used twice, so a profile with `role_arn` + `mfa_serial`
would otherwise need a new code (and a new 30-second wait) for every command.
When the active profile assumes a role or names an MFA device, the session
credentials that come back are written to
`${XDG_CACHE_HOME:-~/.cache}/createrepo-go/aws/<hash>.json` (mode 0600, one
file per profile/role/MFA-device combination) and reused until five minutes
before they expire. Only temporary credentials are stored; long-term access
keys are never copied out of `~/.aws/credentials`.

Set `CREATEREPO_AWS_CACHE` to another directory to move the cache, or to `off`
to disable it. Deleting the files forces a fresh prompt.

## Commands

```
createrepo-go add     <repo> <rpm|dir>... # upload RPMs (dirs scanned recursively) and update metadata
createrepo-go remove  <repo> <name>...  # remove packages (by name; --arch/--evr)
createrepo-go rebuild <repo>            # reconcile an existing repo against updated options
createrepo-go copy    <src> <dst>       # copy/mirror a repository to another location
createrepo-go create  <repo>            # initialize an empty repository (records config)
createrepo-go list    <repo>            # list packages
createrepo-go verify  <repo>            # check the published RPMs match the metadata
createrepo-go check   <repo>            # deep-validate metadata + packages (levels)
```

Useful flags (global unless noted):

```
--target rhel8|rhel9|rhel10   set compression + signature defaults for a release
--dry-run                 show what would change without transferring anything
--force                   upload every staged RPM, overwriting the remote copy
                          unconditionally (e.g. when re-signing)
--compression gzip|zstd   metadata compression (default gzip)
--signature-format v4|openpgp  package signature layout (default v4; RHEL 8/9 need v4)
--location-prefix DIR     upload RPMs under DIR (default Packages; "" = repo root)
--profile NAME            AWS named profile for S3 (sets AWS_PROFILE)
--region NAME             AWS region for S3 (sets AWS_REGION); the bucket's own
                          region wins if it differs, with a warning
--prune-older             (add/rebuild) drop older versions of the same name+arch
--prune-break-deps        (add/rebuild) allow --prune-older to drop a version even
                          when another package depends on that specific version
                          (default: keep it and warn)
--repo-name NAME          (create) human-readable repo name recorded in the config
--repo-url URL            (create) public base URL end users fetch from, recorded in the config

An RPM that the updated metadata no longer references is always removed on
publish, so there is no flag to opt into blob cleanup. (The former
--delete-removed flag is now a deprecated no-op, kept only so existing commands
keep working.)

--sign-metadata           GPG-sign repomd.xml -> repomd.xml.asc
--sign-packages           rpmsign the RPMs before upload
--verify-sigs --keyring K verify each RPM's signature before adding
--gpg-key FILE            private key file (signing)
--gpg-key-id ID           key id/uid from the local keyring (signing)
--gpg-passphrase / $CREATEREPO_GPG_PASSPHRASE
```

## Examples

```sh
# Publish two RPMs to an SSH host, signing the metadata with a keyring key.
createrepo-go add sftp://build@mirror/srv/repo \
    dist/*.rpm --sign-metadata --gpg-key-id releases@example.com

# Add to an S3 bucket (RPMs land under Packages/ by default), signing each RPM.
createrepo-go add s3://my-bucket/el8 dist/*.rpm \
    --sign-packages --gpg-key ./signing.key

# Add every RPM under a directory tree (subdirectories are scanned recursively).
createrepo-go add /srv/repo dist/

# Preview an update without changing anything.
createrepo-go add /srv/repo new.rpm --dry-run

# Verify incoming RPMs are signed by a trusted key as they are added.
createrepo-go add /srv/repo incoming/*.rpm --verify-sigs --keyring RPM-GPG-KEY
```

## Rebuilding a repository

`rebuild` loads an existing repository, applies updated options, reconciles the
new state, and republishes it. It always prints a before/after summary and asks
for confirmation before making any change (`--yes` to skip the prompt, or
`--dry-run` to only report). Recorded config defaults and any overriding flags
(`--location-prefix`, `--gpg-key`, `--target`, …) apply exactly as they do to
`add`/`remove`.

### Metadata is regenerated from the packages when they are local

If a package file was replaced under the same name — a rebuilt RPM published
over the old one — the metadata still describes the build it superseded, and
`verify` reports a checksum mismatch. `rebuild` fixes that by re-reading every
RPM and regenerating its metadata (checksum, size, timestamps, dependencies)
from the file itself.

Re-reading local files is free, so it is the default. When the packages live on
remote storage it would mean downloading the whole repository, so it is off
unless you ask:

```sh
# Local: the packages are re-read automatically.
createrepo-go rebuild /srv/repo --yes

# Remote: opt in, and pay for the download.
createrepo-go rebuild s3://my-bucket/el9 --from-packages --yes
```

When `rebuild` is *not* re-reading the packages it says so, because it is then
republishing metadata it has not checked:

```
warning: the metadata is being regenerated from the published index, not re-read
from the RPM files, so anything the metadata gets wrong about a package
(checksum, size, dependencies) stays wrong. Pass --from-packages to re-read
them, which downloads 20 package(s) (~583.5 MiB).
```

Use `--from-packages=false` to suppress the re-read on a local repository.

Two situations are reported rather than silently resolved:

- **Two metadata records naming one file** (a stale record left beside the
  current one by a bad publish) collapse onto the record that matches the file.
  The count is reported as `N duplicate record(s) dropped`; no RPM is deleted,
  since another record still names the file.
- **Two locations holding byte-identical packages** cannot both be indexed, and
  dropping either would leave its RPM unreferenced and due for deletion. Both
  entries are left exactly as published and a warning names the two locations.

Apart from the re-read, rebuild is metadata-only and never downloads RPMs:

```sh
# Drop every superseded version, keeping only the newest of each name+arch.
createrepo-go rebuild /srv/repo --prune-older

# Move every package under a new subdirectory (relocated server-side, no
# re-upload where the backend supports it).
createrepo-go rebuild s3://my-bucket/el9 --location-prefix Packages
```

Opt-in extras:

```
--from-packages            re-read every RPM and regenerate its metadata from the
                           file (default: on for local packages, off for remote)
--resign-packages          re-sign every RPM with the signing key (needs --gpg-key/--gpg-key-id)
--remove-unreferenced-rpms delete .rpm files the metadata no longer references
--remove-stale-metadata    delete repodata files no longer in use
-y, --yes                  skip the confirmation prompt
```

```sh
# Re-sign the whole repository with a new key. This downloads every RPM, so
# rebuild reports the total download size and asks for confirmation first; the
# re-signed packages carry only the new key's signature.
createrepo-go rebuild /srv/repo --resign-packages --gpg-key ./new-signing.key

# Make the on-disk layout match the metadata exactly and prune old versions.
createrepo-go rebuild /srv/repo --prune-older \
    --remove-unreferenced-rpms --remove-stale-metadata
```

Notes:

- `--resign-packages` is the only operation that downloads RPMs. The download
  volume is computed from the metadata and shown before anything is fetched, so
  the confirmation is accurate; specifying a new key **without**
  `--resign-packages` only re-signs `repomd.xml` (cheap), not the packages.
- `--remove-unreferenced-rpms` / `--remove-stale-metadata` require a backend that
  can enumerate its contents (local, SSH/SFTP, S3, GCS). They are unavailable on
  the read-only HTTP backend.

## Copying a repository (`copy`)

`copy` reads a repository from one location and writes it to another. Source and
destination may each be any supported backend, so it mirrors between local disk,
SSH/SFTP, S3, GCS, and a read-only HTTP source.

```sh
# Mirror a public HTTP repository onto S3, exactly as it stands.
createrepo-go copy https://downloads.example.com/el9 s3://my-bucket/el9
```

### Exact by default

By default the copy is **exact**: every file is transferred byte for byte, so
the destination is a replica and any `repomd.xml.asc` it carries stays valid.
Nothing is regenerated and no timestamps change.

Everything the copy touches is verified as it passes through: each index file
and each RPM is checked against the checksum the metadata records, and a
mismatch aborts the copy. A file already present at the destination with
matching content is not transferred again, so an interrupted copy resumes
cheaply.

An exact copy is refused when it would produce a repository that misleads its
clients. Today that means metadata carrying `<location xml:base="...">`, which
tells dnf to fetch packages from the original host no matter where it read the
metadata from. Rewrite it with `--baseurl`/`--remove-baseurl`, or pass `--force`
to copy it verbatim anyway.

### Writing into a repository that already exists

`copy` refuses to write over an existing repository unless told which of the
three things you mean:

```
--overwrite  replace it; files at the destination the source does not have are deleted
--continue   resume an interrupted copy; fails if the destination is a different repository
--update     merge the source's packages into it (an incremental copy)
```

`--continue` establishes that it is resuming the same copy from the
`copy_source` recorded in the destination's `createrepo-go.json`, and failing
that by checking that the destination holds nothing the source does not have.

`--update` refuses to merge packages signed by a **different key** from the
destination's own, since the result would be unusable for any client that trusts
only one of them. Re-sign as they are copied (`--resign-packages`) to merge them
anyway. The keys are compared from the recorded fingerprints where both
repositories have them, and otherwise by reading the signature out of one
package from each side.

### Selecting and transforming

```
--latest-only              copy only the newest version of each name+arch
--include PATTERN          only copy packages matching these globs (repeatable)
--exclude PATTERN          do not copy packages matching these globs (repeatable)
--arch ARCH[,ARCH...]      only copy these architectures; fails if one is absent
--kinds KIND[,KIND...]     only copy these kinds: binary, source, debuginfo, debugsource (alias: debug)
--exclude-kinds KIND...    do not copy these kinds
--rebuild-metadata         regenerate the metadata from the copied RPMs
--resign-packages          re-sign every copied RPM, replacing the source's signature
--prune-older              after copying, drop superseded versions from the destination
--prune-break-deps         let --prune-older drop a version another package depends on
--baseurl URL              replace the metadata's <location xml:base> with this URL
--remove-baseurl           drop it, so clients fetch packages from the copy
--location-prefix DIR      land the copied RPMs under DIR (default: keep the source's layout)
--repo-name NAME           name to record in the destination's config file
--skip-verify              do not check GPG signatures even when a key is available
--remove-unreferenced-rpms delete .rpm files the copied metadata does not reference
--remove-stale-metadata    delete repodata files no longer in use
-y, --yes                  skip the confirmation prompt
```

`--prune-older` reconciles the destination after the copy, so it also drops
versions the destination already held — that is what makes it useful with
`--update`. On a fresh copy, `--latest-only` gets the same result without
transferring the versions that are about to be dropped. `--baseurl` plays the
role `--repo-url` plays on `create`: it sets the recorded base URL, as well as
rewriting the metadata's `xml:base` when there is one.

`--include`/`--exclude` patterns are shell globs matched against a package's
name and its `name-version`, `name-version-release`, and full NEVRA forms, so
`hello`, `hello-2.10*` and `libfoo-1.2.0-1.x86_64` all select what you would
expect. `--arch` keeps `noarch` packages alongside the architecture you ask for,
because a repository for one architecture still needs them; source rpms have
arch `src`, so name it to keep them.

Any of these options makes the copy **rebuild** the destination's metadata
rather than replicate it, and the run reports which one did.

### Verification and signing

If the source repository records a signing key in its `createrepo-go.json`, or
you supply one with `--keyring`, the copy verifies against it: the detached
`repomd.xml` signature, and every RPM it reads (via `rpmkeys`). Verification is
automatic; `--verify-sigs` makes a missing key an error rather than a warning,
and `--skip-verify` turns it off.

Rebuilt metadata is a different document from the one the source signed, so a
**signed** source may only be transformed if the copy gets a signature of its
own:

```sh
# Take only the newest x86_64 build of each package, no debug or source rpms,
# and sign the rebuilt metadata with our own key.
createrepo-go copy /srv/upstream /srv/mirror \
    --latest-only --arch x86_64 --exclude-kinds debug,source \
    --sign-metadata --gpg-key-id releases@example.com
```

Without `--sign-metadata` and a key that copy is refused, because it would
otherwise publish metadata alongside a signature that no longer matches it. An
unsigned source has no such constraint.

`--sign-packages` signs the copied RPMs (for an unsigned source);
`--resign-packages` replaces the source's signature with yours. Both rewrite the
package, so its metadata is re-derived from the signed file.

### Notes

- The copy holds only one package on local disk at a time, so a mirror of any
  size needs no more room than its largest RPM.
- `--dry-run` reports the whole plan without fetching or writing anything.
- A source that cannot enumerate its contents (plain HTTP) contributes only the
  files its metadata references; anything else it stores is invisible and is
  reported as such.
- `--overwrite` deletes destination files the source does not have, so it asks
  for confirmation first unless `--yes` or `--dry-run` is given.

## Repository config file

`create`, `add`, `remove`, `rebuild` and `copy` maintain a small
`createrepo-go.json` file at the
repository root (alongside `repodata/`). It records the repository's identity and
the signing choices in effect:

```json
{
  "name": "Frobulator Beta EL9",
  "baseurl": "https://downloads.example.com/ee/rpms/el9",
  "location_prefix": "Packages",
  "target": "rhel9",
  "sign_packages": true,
  "sign_metadata": true,
  "signature_format": "v4",
  "gpg_key_id": "FF652CAEF8C583AB827F219E992DF1A087059154",
  "copy_source": "https://downloads.example.com/ee/rpms/el9"
}
```

- `name` / `baseurl` are set with `--repo-name` / `--repo-url` on `create` and
  preserved across later `add`/`remove` runs.
- `location_prefix` records the `--location-prefix` subdirectory RPMs were
  uploaded into, so later `add` runs that omit the flag keep placing packages in
  the same directory.
- `target` records the compatibility profile in effect. A later `add`/`remove`
  that omits `--target` re-applies that profile's defaults (compression,
  signature format, and any future target-derived settings), so the repository
  stays consistent with the release it was built for.
- `copy_source` is written by `copy` and records where the repository was
  copied from, so a later `copy --continue` can tell that it is resuming the
  same copy rather than overwriting an unrelated repository.
- `gpg_key_id` is also what `copy` verifies a source repository against when no
  `--keyring` is supplied: it exports that key from the local GnuPG keyring and
  checks the metadata signature and every RPM it reads with it.
- `gpg_key_id` is always stored as the key's **fingerprint**, resolved from
  whichever of `--gpg-key`/`--gpg-key-id` was supplied, so it is portable across
  machines.
- The recorded settings act as **defaults**: a later `add`/`remove` that omits
  the relevant flags re-uses them (e.g. re-signs `repomd.xml` with the same key,
  keeps the same target profile). Pass the flags explicitly to override — an
  explicit CLI `--target` or `--signature-format` wins over the recorded ones.
  Config-driven metadata/package signing resolves the key from the local GnuPG
  keyring by fingerprint, so that key must be importable there.

The file is plain JSON and is **not** part of the rpm-md metadata — dnf/yum
ignore it and it is never referenced by `repomd.xml`.

```sh
# Initialize a repository and record its metadata + signing defaults.
createrepo-go create /srv/repo \
    --repo-name "Frobulator Beta EL9" \
    --repo-url  "https://downloads.example.com/ee/rpms/el9" \
    --sign-metadata --gpg-key-id releases@example.com

# Later updates re-sign automatically from the recorded config.
createrepo-go add /srv/repo dist/*.rpm
```

## Verifying a repository (`verify`)

`verify` checks that every RPM the metadata references is present with the size
and checksum the metadata records, and that the repository's intra-repository
dependencies are satisfied. It modifies nothing.

```sh
createrepo-go verify /srv/repo
# OK: 42 package(s) present with the expected size; checksums: 42 verified from
# content; dependencies satisfied
```

How far the checksum check gets depends on what the backend can prove without
transferring the object:

| Backend | Default checksum check |
|---------|------------------------|
| local disk | Computed from the stored file. |
| SSH/SFTP | Computed by `sha256sum` on the remote host, so the RPM never crosses the network. |
| S3, GCS | The checksum **recorded when the object was uploaded**. This catches metadata and objects disagreeing, but not an object whose content changed afterwards. |
| HTTP(S) | None: no checksum is available without downloading. |

`--checksums` proves every RPM's checksum from its content regardless,
downloading each package the backend cannot hash in place:

```sh
# Prove the contents of an S3 repository, not just its recorded checksums.
createrepo-go verify s3://my-bucket/el9 --checksums
```

The summary always states which of the three applied, so an unproved checksum is
never reported as though it had been verified, and a run with anything left
unconfirmed says so and points at `--checksums`.

```
--checksums          hash every RPM's content, downloading where necessary
--concurrency N      packages to check in parallel (default 6)
```

`verify` reports the estimated download volume before starting a `--checksums`
run against a backend that cannot hash in place. Nothing is written to disk:
each RPM is hashed as it streams past.

For a deeper sweep that also validates the metadata index files themselves and
the RPMs' internal header/payload digests, use `check --level fetch` below.

## Validating a repository (`check`)

`verify` is the quick consistency check: it confirms every indexed package's
RPM exists (and, where the backend can hash remotely, that its checksum matches
the pkgid). `check` is the deeper validator — a faithful, backend-agnostic port
of the standalone `dnf-repocheck` tool. It reads the repository metadata, parses
the package index, and confirms the metadata is internally consistent and that
the packages it references really exist with the size and checksum the metadata
claims.

Both commands also validate the **dependency graph**: every package requirement
that the repository could satisfy from its own packages (some package provides
the capability) must be met by an available version. A requirement whose
capability no package in the repository provides — glibc, `/bin/sh`, `rpmlib(...)`
— is an external dependency and is left to the installing system. This catches a
prune or removal that dropped a version another package still needs (for example
`app` requiring `deplib = 1.0` after `deplib-1.0` was pruned). `rebuild` reports
the same broken dependencies in its plan, and `--prune-older` will not drop a
version that another package pins unless `--prune-break-deps` is given.

It exists to catch repositories where a published package file has diverged
from what `createrepo` indexed — for example when one RPM is published over
another without regenerating the metadata. End users see this as dnf errors
like:

```
[MIRROR] calico-fluent-bit-4.0.13-1.el8.x86_64.rpm: Interrupted by header callback:
  Server reports Content-Length: 13264076 but expected size is: 13262596
```

Unlike `dnf-repocheck`, which only speaks HTTP, `check` runs against **any**
backend — local disk, SSH/SFTP, S3, GCS, or HTTP(S) — so the same validation
covers a repository at rest as well as one published live.

### Levels

Validation is layered; each `--level` includes everything the previous one does:

| `--level`  | Checks |
|------------|--------|
| `metadata` | `repodata/repomd.xml` is present and parseable, and every index file it references (primary/filelists/other) reads back with the correct size, checksum, and decompressed (open) size/checksum. |
| `head`     | …plus every package in the index exists with the size the metadata claims (and, when the backend can checksum without transferring the file — S3, GCS, SFTP — the correct checksum). |
| `fetch`    | …plus every package is downloaded and its size **and** checksum verified, then — if the `rpm` command is available — `rpm --checksig --nosignature` verifies the package's internal header/payload digests. |

The default level is `head`.

### Version selection

`--versions` controls whether all versions of a package are checked or only the
newest (`all` / `latest`). The default depends on the level: `latest` for
`fetch` (so a full download stays cheap) and `all` for `metadata`/`head`.
"Latest" uses the shared librpm-faithful version comparison (including the `~`
and `^` separators), so the `rpm` command is **not** required to determine it.

### Input

The positional argument (or `--repo-file`) may be a repository location — a
local path, or a `file://`, `sftp://`, `s3://`, `gs://` or `http(s)://` URL — or
a path/URL to a `.repo` file (ending in `.repo`). A `.repo` file's baseurl may
contain `$releasever`/`$basearch` variables; supply the values to substitute
with `--releasever` and `--arch`, and each combination is checked as its own
target.

### Examples

```sh
# HEAD-check every package in a live HTTP repo for RHEL 8 and 9 from a .repo file:
createrepo-go check --releasever 8,9 \
  https://downloads.tigera.io/ee/rpms/v3.22/calico_enterprise.repo

# Fully download and checksum-verify a local repository:
createrepo-go check --level fetch /srv/repo

# Checksum-verify just two packages on S3 without a full sweep:
createrepo-go check --level head --packages calico-fluent-bit,calico-node \
  s3://my-bucket/el8

# Only validate that a GCS repo's metadata is internally consistent:
createrepo-go check --level metadata gs://my-bucket/el9
```

### Flags

| Flag | Default | Meaning |
|------|---------|---------|
| `--repo-file` | – | Path/URL to a `.repo` file (alternative to the positional arg). |
| `--level` | `head` | `metadata` \| `head` \| `fetch`. |
| `--arch` | host arch | Comma-separated arches (e.g. `x86_64,aarch64`). `any` checks every arch in the metadata. |
| `--releasever` | – | Comma-separated `$releasever` values (e.g. `8,9`) for a `.repo` baseurl. |
| `--packages` | all | Comma-separated package names to check. |
| `--versions` | level-dependent | `latest` \| `all`. |
| `--concurrency` | `6` | Parallel package checks. |
| `--timeout` | `60s` | Per-operation timeout (`0` to disable). |
| `--verbose` | `false` | Print a line for every check, not just issues. |

### The `rpm` command

The deeper package digest verification at `--level fetch` shells out to `rpm`.
If `rpm` is not installed the tool prints a warning naming the check it is
skipping and continues — file size and checksum are still verified entirely in
Go. Likewise, `xz`-compressed metadata requires the `xz` command (no pure-Go xz
decoder is bundled); `zstd`, `gzip`, and `bzip2` metadata are handled natively.

### Exit codes

`check` exits `0` when all checks pass, and non-zero when at least one check
fails (the failure details are printed first). Other commands exit non-zero on
any error.

## Library

The CLI is a thin wrapper over reusable packages:

- `pkg/repodata` — rpm-md data model and primary/filelists/other/repomd XML.
- `pkg/rpmmeta` — extract repository metadata from an RPM file.
- `pkg/backend` — storage abstraction (local/sftp/s3/gcs/http) with an optional
  `RemoteHasher` capability for download-free validation.
- `pkg/repo` — load, mutate and publish a repository (`Open`/`AddRPM`/`Remove`/
  `Commit`); `OpenWith` accepts a custom backend.
- `pkg/repocheck` — layered validation (`metadata`/`head`/`fetch`) of a
  repository over any backend, including `.repo`-file and `$releasever` handling
  (`repocheck.Run`).
- `pkg/sign` — repomd signing, RPM verification, RPM package signing, and signing
  key fingerprint resolution.
- `pkg/repoconfig` — load/save the `createrepo-go.json` repository config file.

## Testing

```sh
go test ./...        # fast unit + golden tests
make test-e2e        # full end-to-end suite (servers/containers; slower)
```

The unit/golden tests build sample RPMs and compare generated metadata
field-by-field against `createrepo_c` output, assert the minimal-transfer
behavior (no RPM is read when adding a package to an existing repo), and
round-trip GPG signing/verification. Signing tests are skipped automatically if
`gpg`/`rpmsign`/`rpmkeys` are absent. Regenerate the fixtures with
`reference/gen.sh` (needs `rpmbuild` and `createrepo_c`).

The **end-to-end suite** (`test/e2e/`, behind the `e2e` build tag) drives the
built CLI against every backend — local disk, a freshly started MinIO instance
for S3, a local `sshd` for SFTP, a `fake-gcs-server` container for GCS, and a
read-only HTTP server — running one comprehensive scenario set identically
against each. It also runs real **AlmaLinux 8/9/10** clients (in docker or
podman) that install packages from the generated repositories with `dnf`, using
the `--target` profile that matches each release, to prove the metadata and
signatures work with existing clients. Backends and clients whose dependencies
are missing skip themselves. Run it with `make test-e2e` or
`go test -tags e2e ./test/e2e/...`; see [`test/e2e/README.md`](test/e2e/README.md)
for the full coverage list and the optional dependencies.
