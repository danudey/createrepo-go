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
- **Damaged packages are refused.** Every RPM is checked against its own
  header and payload digests and signatures before it is signed, uploaded or
  indexed, so a truncated or corrupted package never reaches the metadata, and
  is never given a signature that vouches for it. Text the XML metadata cannot
  hold is handled as `createrepo_c` handles it: a string that is not valid
  UTF-8 is read as Latin-1 and converted, and a package carrying a control
  character is refused.

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
| SSH/SFTP | `sftp://user@host/srv/repo` | uses the ssh-agent and `~/.ssh/known_hosts`; remote `sha256sum` validation. The host key is verified against `~/.ssh/known_hosts`, and the connection fails if that file cannot be read or does not list the host — pass `--insecure-ignore-host-key` to connect without verification |
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
createrepo-go takeover <repo>           # report what managing an existing repo with this tool would change
createrepo-go list    <repo>            # list packages
createrepo-go verify  <repo|file.repo>  # check a repository end to end, as dnf reads it
```

Useful flags (global unless noted):

```
--target rhel8|rhel9|rhel10   set compression + signature defaults for a release
--dry-run                 show what would change without transferring anything
--progress                show progress for uploads, downloads and copies
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
--verify-sigs --keyring K verify each RPM's signature before adding, as the
                          --target release's rpm would (natively; no rpm needed)
--gpg-key FILE            private key file (signing)
--gpg-key-id ID           key id/uid from the local keyring (signing)
--gpg-key-url URL         URL users fetch the public key from (a .repo file's
                          gpgkey=); recorded, and every signature is checked
                          against it
--gpg-passphrase / $CREATEREPO_GPG_PASSPHRASE
--gpg-no-prompt / $CREATEREPO_GPG_NO_PROMPT
                          never let gpg prompt for a passphrase, for metadata
                          or package signing; fail instead (for CI)
```

### Checking signatures against your users' key

Every signature a run makes (`repomd.xml.asc` and each RPM signed with `--sign-packages`) is
checked as soon as it is made:

- It must come from the configured key or one of its subkeys. A signature by
  any other key (for example, a `--gpg-key-id` uid that matches two keys)
  fails the run before anything is published.
- When the command finishes, it prints `warning:` lines for anything that
  could stop users verifying the repository, then a `signatures:` summary line.

Pass `--gpg-key-url` once with the URL your users fetch the public key from
(the `gpgkey=` in your `.repo` file). It is saved as `gpg_key_url` in `createrepo-go.json`, and later runs
fetch that key and verify each new signature against it. The warnings cover:

- a signature made by a **subkey** rather than the primary key. gpg picks a
  signing subkey automatically, and users can verify it only if the public key
  they install includes that subkey
- a published key that **does not verify** the signatures, because it is a
  different key or because it lacks the signing subkey (export it with
  `gpg --armor --export <fingerprint>`, which includes subkeys)
- a key that **expires** within 30 days, or a published copy that expires
  earlier than your keyring's copy
- a published key file that is served over plain **http**, cannot be fetched,
  or contains **private** key material
- signatures using a digest clients refuse (SHA-1, MD5), or a package signature the
  `--target` release's rpm cannot read

Without a key URL, the run still reports subkey signatures and expiring keys,
and the summary line says the signatures were not checked against your users'
key.

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

## Progress reporting (`--progress`)

Anything that moves package-sized amounts of data can report how far along it
is. `--progress` is global, and off by default:

```sh
createrepo-go copy https://downloads.example.com/el9 s3://my-bucket/el9 --progress
```

```
  download calico-node-3.28.0-1.el9.x86_64.rpm   [=========---]   78%  101.0 MiB/129.0 MiB
  download calico-typha-3.28.0-1.el9.x86_64.rpm  [===---------]   22%  12.4 MiB/56.1 MiB
  download [====--------]   34%  12/57  1.9 GiB/5.7 GiB  48.3 MiB/s  ETA 1m34s
```

Each transfer in flight has a bar of its own, labelled with what is being done
to it (`download`, `upload`, `sign`). Where several packages
transfer at once — `verify --checksums` runs several in
parallel — each one gets a bar. The bottom line is the operation as a whole:
packages finished, bytes moved, the current rate, and the time left at that
rate.

The bars cover the operations whose cost is the repository's size rather than
its metadata's:

| Command | What is reported |
| --- | --- |
| `add`, `create`, `remove`, `rebuild` | every file the publish uploads, relocates, writes or deletes |
| `copy` | each object fetched from the source and written to the destination |
| `rebuild --from-packages`, `--resign-packages` | the RPMs downloaded to re-read or re-sign |
| `verify --checksums` | the RPMs downloaded to hash, where the backend cannot hash them in place |

Notes:

- The display is written to **stderr**, so a command's output is exactly what it
  would have been without the flag and stays safe to pipe. Lines the command
  prints erase the bars first rather than landing on top of them.
- Without a terminal on stderr (a log file, a CI job) the bars are replaced by a
  single summary line every 30 seconds, plus one when the operation finishes.
- Under `--dry-run` nothing is transferred, so nothing is reported.
- An object store's upload is hashed before it is sent. The bar for such an
  upload therefore fills once for the hashing pass and again for the transfer;
  only the bytes that reach the network are counted towards the total.

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

## Taking over an existing repository (`takeover`)

`takeover` answers the question that comes before any of the above when the
repository was created by something else: **what would this tool do to it?**

```sh
createrepo-go takeover s3://my-bucket/el9 --target rhel9
```

It analyses the published repository and reports exactly what republishing it
with the given settings would change — while changing nothing itself. The
analysis is a real dry run, not a description of one: the repository is loaded,
the reconciliation the settings imply is applied to the in-memory index, and the
commit plan is computed from it. Every flag that affects a publish affects the
report (`--target`, `--compression`, `--location-prefix`, `--changelog-limit`,
`--sign-metadata`/`--gpg-key-id`, `--prune-older`, `--from-packages`), so the
way to find out what a setting costs is to pass it.

The report has four parts: the repository as published, the settings a republish
would use, what it would write, move and delete, and a per-package diff of the
metadata that would change. Then the findings, ranked:

| Severity | Meaning |
| --- | --- |
| `blocking` | republishing as configured breaks something clients rely on |
| `advisory` | the repository changes in a way worth knowing about first |
| `info` | observed; changes nothing |

The command exits non-zero while any blocking finding stands, so it can gate a
migration (`--json` emits the whole report for a CI step to act on).

### What it catches

- **Metadata this tool does not generate.** It writes primary, filelists and
  other. A publish carries every other document the repository publishes into
  the new `repomd.xml` unchanged: comps (`dnf group install`), updateinfo
  (`dnf update --security`), modulemd and so on. These are reported as kept, but
  this tool never updates them. The sqlite and zchunk forms of primary,
  filelists and other describe the old package list, so a publish drops them.
  That is advisory, because nothing on RHEL 8+ reads the sqlite databases.
- **A metadata signature that would stop verifying.** A republish rewrites
  `repomd.xml` and leaves `repomd.xml.asc` beside it, so a signed repository that
  is not re-signed breaks every client with `repo_gpgcheck=1`. Signing with a
  *different* key from the one that signed it is reported too — the signature is
  read to identify its issuer, no keyring needed.
- **Packages indexed with another checksum algorithm.** The metadata stays
  readable by dnf, but every checksum this tool computes is a sha256 and would
  disagree with a sha512 pkgid: `verify` would report a mismatch on every package
  and re-adding one would fail. `--from-packages` resolves it by recomputing.
- **Metadata that does not describe the stored files.** With `--from-packages`
  each RPM is re-read and compared field by field, so a package republished over
  another under the same name is found before the takeover, not after.
- **Where the packages actually live**, versus where `--location-prefix` would
  put the next one; relocation (and whether the backend can do it server-side);
  `xml:base` pointing clients at another host; RPMs the metadata does not
  reference; repodata left over from earlier publishes; packages the metadata
  references but the repository does not hold; and a backend that can be read but
  never written (plain HTTP).

Metadata compressed with xz or bzip2 is read as well as gzip and zstd, since
that is what other tooling publishes (xz needs the `xz` command).

### Claiming the repository

```sh
# Look first.
createrepo-go takeover /srv/repo --target rhel8 --from-packages

# Record the settings — this writes createrepo-go.json and nothing else.
createrepo-go takeover /srv/repo --target rhel8 --adopt --yes \
    --repo-name "Legacy EL8" --repo-url https://downloads.example.com/el8

# Then publish with them, when the findings say it is safe to.
createrepo-go rebuild /srv/repo --from-packages
```

`--adopt` is deliberately the smallest possible step: it writes the config file
and publishes nothing, so the repository keeps serving exactly what it served
before while later commands inherit the settings. The location prefix it records
is the directory the packages are *already* in (unless `--location-prefix` says
to move them), so a later `add` joins them instead of starting a second
directory.

```
--from-packages    re-read every RPM and compare it against the metadata
                   (default: on for local packages, off for remote)
--prune-older      report what dropping superseded versions would do
--sample N         packages to download to identify who signed them (default 1, 0 to skip)
--json             emit the full report as JSON
--adopt            record the settings in createrepo-go.json (publishes nothing)
--repo-name/--repo-url  identity to record with --adopt
-y, --yes          skip the --adopt confirmation prompt
```

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
- An exact copy refuses to replace a destination file whose content is known
  to differ from the source's (a package of another repository, or a damaged
  one), and names it. `--overwrite` or `--force` replaces it. The top-level
  index and its signatures change on every publish and are always replaced.
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
  "gpg_key_url": "https://downloads.example.com/RPM-GPG-KEY-frobulator",
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
- A hand-edited `gpg_key_id` must still be a hexadecimal key id or fingerprint
  (optionally `0x`-prefixed). A trailing `!` is allowed and tells gpg to use
  exactly that key rather than one of its subkeys. Anything else, such as a
  uid, is refused when the file is read, because the file may come from a
  repository you do not control.
- `gpg_key_url` records `--gpg-key-url`: where users fetch the public key.
  Every later signing run checks its signatures against the key found there
  (see [Checking signatures against your users' key](#checking-signatures-against-your-users-key)).
  It must be an http(s) URL.
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

`verify` checks a repository end to end, the way dnf reads it, and reports every
defect it finds in one pass. It modifies nothing. It runs against any backend:
local disk, SSH/SFTP, S3, GCS or HTTP(S). So the same check covers a repository
at rest and one published live.

1. **Metadata.** `repodata/repomd.xml` is present and parses, names a primary
   document, and names no type twice. Every document it references (primary,
   filelists, other, and also comps, updateinfo, modules and the sqlite
   databases) is present with the recorded size and checksum, decompresses,
   and has the recorded open size and open checksum. Primary, filelists and
   other must also parse.
2. **The metadata signature.** `repomd.xml.asc` is checked; see
   [Signatures](#signatures).
3. **Packages.** Every RPM the metadata references is present with the size
   and checksum the metadata records, and is intact, with sound signatures.
4. **Dependencies.** Every package requirement the repository could satisfy
   from its own packages must be met by an available version. A requirement no
   package in the repository provides (glibc, `/bin/sh`) is left to the
   installing system. This catches a prune or removal that dropped a version
   another package still needs.

Metadata too broken to load is reported as problems, like anything else. The
packages are then not checked, since there is no index to check them against.

The package check catches a repository where a published package has diverged
from what the metadata says, for example one RPM published over another
without regenerating the metadata. Users see that as dnf errors like:

```
[MIRROR] calico-fluent-bit-4.0.13-1.el8.x86_64.rpm: Interrupted by header callback:
  Server reports Content-Length: 13264076 but expected size is: 13262596
```

```sh
createrepo-go verify /srv/repo
# OK: metadata: 3 document(s) intact (primary, filelists, other)
# packages: 42 present with the expected size; checksums: 42 verified from
# content; dependencies satisfied
# contents: 42 intact (header and payload digests checked); signatures: 42 signed
# by B29070C0BBA03456860DBA5C260B206AE739E175 (verified against a trusted key)
# repomd.xml: signed by B29070C0BBA03456860DBA5C260B206AE739E175 (verified against a trusted key)
```

### Checksums

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

`verify` reports the estimated download volume before starting a `--checksums`
run against a backend that cannot hash in place. Nothing is written to disk:
each RPM is hashed as it streams past.

### Package contents

A matching checksum proves the file is the one the metadata describes, not that
the file is sound: metadata generated from a damaged package agrees with it.
So every RPM is also checked against itself — that it parses, that its header
and payload digests match, and that every signature it carries matches what it
signs. This needs no `rpm` installation.

How much of each package is read depends, again, on the backend:

| Backend | What is read |
|---------|--------------|
| local disk | The whole file: header and payload digests are checked. |
| S3, GCS, SFTP, HTTP(S) | The header only (a few KB per package), which holds the header digests and signatures. With `--checksums` the downloaded file is checked in full, in the same pass that hashes it. |

`--skip-package-contents` turns this off, for a quick presence check of a
large remote repository.

### Signatures

Signatures are checked whenever they are present, with no flag needed:

- **Integrity.** A signature that does not match the package or `repomd.xml`
  it signs (damaged, or copied from another package) is always a problem. This
  needs no key.
- **Trust.** Signatures are verified against trusted keys: `--keyring` files
  and published keys together, or, failing both, the signing key
  `createrepo-go.json` records, exported from the local GnuPG keyring. Without
  any, `verify` still identifies who signed each package, but cannot prove it.
- **The key your users install.** Keys from `--keyring-url` (http(s) or
  `file://`, repeatable) are trusted, and every signature present,
  `repomd.xml`'s included, must verify against the trusted keys: a signature
  that key cannot verify is one your users cannot verify either. Without
  `--keyring-url`, the `gpg_key_url` that `createrepo-go.json` records is
  fetched and checked for **consistency only**. `repomd.xml`'s signature must
  verify against it, and every package signature must be by a key it contains.
  It is never trusted, because the repository chose it and could otherwise
  vouch for itself. When a signature was made by a subkey that the published
  copy lacks, the problem says so.
- **One key.** Every package must be signed by the same key. A package signed
  by another key, or an unsigned package in a signed repository, is a problem.
  When the config records a signing key, that key is the one every package
  and `repomd.xml` must be signed by.
- **The target's rules.** Signatures are judged as the `--target` release's
  rpm judges them (the recorded target by default). RHEL 8 and 9 cannot read
  the rpm 6 `OPENPGP` signature, so a package signed only that way is unsigned
  to their clients (a warning, or a problem when signatures are required).
  RHEL 9 and later refuse SHA-1 and DSA signatures and RSA keys under 2048
  bits, so such a signature is a problem for those targets.

Three flags make the checks stricter:

```
--require-trusted          every signature must verify against a trusted key
--require-signed-packages  every RPM must carry a signature the target accepts
                           (implied when the config records sign_packages)
--require-signed-metadata  repomd.xml must be signed (implied when the config
                           records sign_metadata)
```

```sh
# Prove that a repository is signed, end to end, by the key we publish.
createrepo-go verify https://downloads.example.com/el9 --target rhel9 \
    --keyring RPM-GPG-KEY-example --require-trusted \
    --require-signed-packages --require-signed-metadata
```

### Verifying from a `.repo` file

`verify` also accepts a `.repo` file in place of a repository: a path, a
`file://` URL or an http(s) URL ending in `.repo`. It verifies every enabled
repository in the file the way a client using that file would:

- the first `baseurl` is verified. A repository with only a `mirrorlist` or
  `metalink` fails, because verify cannot follow those
- its `gpgkey=` URLs are the published keys. A key that cannot be loaded is a
  problem, since clients cannot import it either
- `gpgcheck=1` (or `pkg_gpgcheck=1`) requires every RPM to carry a signature
  the keys verify. `repo_gpgcheck=1` requires the same of `repomd.xml`. Either
  one without a `gpgkey=` is a problem
- variables are substituted from `--releasever` (which also sets
  `$releasever_major` and `$releasever_minor`), `--arch` (sets both `$arch`
  and `$basearch`) and `--var name=value`. Any variable without a value is an
  error, reported before anything is fetched
- `--releasever` and `--arch` take comma-separated lists. A section that uses
  the variable is verified once per value, so one section describing EL8 and
  EL9 repositories is verified as both

```sh
createrepo-go verify ./example.repo --releasever 8,9 --arch x86_64,aarch64
```

Each line of output is prefixed with the repository's `[id]` and the values
used, such as `[calico releasever=9 arch=x86_64]`. The command fails if any
repository fails.
A `.repo` file fetched over http(s) may only point at http(s) repositories,
not local paths or object-store URLs: its author chose those locations, not
the person running verify.

### Choosing what to check

By default every package of every version and architecture is checked.

```
--metadata-only            check the metadata alone: every repodata document, the
                           repomd.xml signature, and dependencies; no package files
--packages a,b             only these package names
--arch x86_64,aarch64      only these architectures (noarch always counts)
--versions latest          only the newest version of each name and architecture
```

The dependency check always covers the whole repository, whatever is
selected.

### Flags

```
--checksums                hash every RPM's content, downloading where necessary
--concurrency N            packages to check in parallel (default 6)
--keyring FILE             trusted public keys (repeatable)
--keyring-url URL          trusted public keys fetched from a URL, as your users
                           fetch them (repeatable); signatures must verify
--releasever V,...         .repo $releasever values, one repository each
--arch A,...               architectures to check; .repo $arch/$basearch values
--var NAME=VALUE           value for any other .repo file variable (repeatable)
--packages a,b             only these package names
--versions all|latest      every version (default), or only the newest
--metadata-only            check the metadata, not the package files
--require-trusted          fail unless every signature verifies against a trusted key
--require-signed-packages  fail unless every RPM is signed in a way the target accepts
--require-signed-metadata  fail unless repomd.xml is signed
--skip-package-contents    do not read the RPMs (no digest or signature checks)
--timeout D                give up on a backend operation that makes no progress for
                           D (default 60s; 0 disables); a download that keeps
                           making progress is never cut off
-v, --verbose              a line for every metadata document and package checked
```

### Requirements

`verify` needs no `rpm` installation: package digests and signatures are
checked in Go. `xz`-compressed metadata needs the `xz` command, because no
pure-Go xz decoder is bundled. `zstd`, `gzip` and `bzip2` metadata are handled
natively.

### Exit codes

`verify` exits 0 when every check passes, and non-zero when anything fails,
after printing every problem it found.

## Library and testing

The packages behind the CLI, and how to run the unit and end-to-end suites, are
described in the [project README](../README.md).
