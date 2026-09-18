// Copyright (c) 2026 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package repocheck

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/danudey/createrepo-go/pkg/backend"
	"github.com/danudey/createrepo-go/pkg/repodata"
)

// Level selects how deep the checks go. Each level is cumulative.
type Level int

// The available check levels, in increasing order of cost.
const (
	LevelMetadata Level = iota // repomd + index files present and valid
	LevelHead                  // + packages exist with the correct size
	LevelFetch                 // + packages downloaded, checksums verified, rpm -K
)

func (l Level) String() string {
	switch l {
	case LevelMetadata:
		return "metadata"
	case LevelHead:
		return "head"
	case LevelFetch:
		return "fetch"
	default:
		return "unknown"
	}
}

// ParseLevel parses a Level from its string name.
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "metadata", "meta":
		return LevelMetadata, nil
	case "head", "exists":
		return LevelHead, nil
	case "fetch", "full", "download":
		return LevelFetch, nil
	default:
		return 0, fmt.Errorf("invalid level %q (want metadata|head|fetch)", s)
	}
}

// Status is the outcome of a single check.
type Status string

// The outcomes a single check can report. Only StatusFail means the
// repository is broken; StatusWarn and StatusSkip leave the run successful.
const (
	StatusOK   Status = "OK"
	StatusFail Status = "FAIL"
	StatusWarn Status = "WARN"
	StatusSkip Status = "SKIP"
)

// kindRepomd is the Result.Kind reported for the repository index itself.
const kindRepomd = "repomd.xml"

// Result is one validated artifact.
type Result struct {
	Target string // the label of the repository the artifact belongs to
	Kind   string // "repomd.xml", "metadata:<type>" or a package nevra
	Loc    string // backend-relative path or URL of the artifact
	Status Status
	Detail string
}

// Config holds the resolved validation configuration.
type Config struct {
	Level       Level
	LatestOnly  bool
	Arches      []string // empty means "all arches in the metadata"
	Packages    []string // empty means "all packages"
	Concurrency int
	// Timeout bounds each individual backend operation. Zero means no timeout.
	Timeout time.Duration
}

// checker runs validations against targets and accumulates results.
type checker struct {
	cfg Config
	rpm RPMTool

	mu      sync.Mutex
	results []Result
	logf    func(format string, args ...any)
}

func newChecker(cfg Config, rpm RPMTool, logf func(string, ...any)) *checker {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &checker{cfg: cfg, rpm: rpm, logf: logf}
}

// Results returns a copy of the accumulated results.
func (ck *checker) Results() []Result {
	ck.mu.Lock()
	defer ck.mu.Unlock()
	out := make([]Result, len(ck.results))
	copy(out, ck.results)
	return out
}

func (ck *checker) add(r Result) {
	ck.mu.Lock()
	ck.results = append(ck.results, r)
	ck.mu.Unlock()
	symbol := map[Status]string{StatusOK: "  ok ", StatusFail: "FAIL ", StatusWarn: "warn ", StatusSkip: "skip "}[r.Status]
	ck.logf("[%s] %s: %s %s", symbol, r.Target, r.Kind, r.Detail)
}

// opCtx derives a per-operation context honoring the configured timeout.
func (ck *checker) opCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if ck.cfg.Timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, ck.cfg.Timeout)
}

// getAll reads a backend object fully into memory (metadata files are small).
func (ck *checker) getAll(ctx context.Context, be backend.Backend, relpath string) ([]byte, error) {
	c, cancel := ck.opCtx(ctx)
	defer cancel()
	rc, err := be.Get(c, relpath)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// checkTarget validates a single concrete repository accessed via be.
func (ck *checker) checkTarget(ctx context.Context, be backend.Backend, label string) {
	repomdLoc := joinLoc(be.String(), repomdPath)

	raw, err := ck.getAll(ctx, be, repomdPath)
	if errors.Is(err, backend.ErrNotExist) {
		ck.add(Result{Target: label, Kind: kindRepomd, Loc: repomdLoc, Status: StatusFail, Detail: "no repodata/repomd.xml (not a repository?)"})
		return
	}
	if err != nil {
		ck.add(Result{Target: label, Kind: kindRepomd, Loc: repomdLoc, Status: StatusFail, Detail: "fetch failed: " + err.Error()})
		return
	}
	md, err := repodata.ParseRepomd(raw)
	if err != nil {
		ck.add(Result{Target: label, Kind: kindRepomd, Loc: repomdLoc, Status: StatusFail, Detail: err.Error()})
		return
	}
	ck.add(Result{
		Target: label, Kind: kindRepomd, Loc: repomdLoc, Status: StatusOK,
		Detail: fmt.Sprintf("revision %s, %d metadata files", md.Revision, len(md.Data)),
	})

	// Validate each metadata index file; capture the decompressed primary.
	var primaryBytes []byte
	for i := range md.Data {
		d := md.Data[i]
		decompressed, res := ck.checkMetadataFile(ctx, be, label, d)
		ck.add(res)
		if d.Type == "primary" && decompressed != nil {
			primaryBytes = decompressed
		}
	}

	if ck.cfg.Level == LevelMetadata {
		return
	}

	if primaryBytes == nil {
		ck.add(Result{
			Target: label, Kind: "packages", Loc: be.String(), Status: StatusWarn,
			Detail: "primary metadata unavailable; cannot validate packages",
		})
		return
	}

	pkgs, err := repodata.ParsePrimary(primaryBytes)
	if err != nil {
		ck.add(Result{Target: label, Kind: "packages", Loc: be.String(), Status: StatusFail, Detail: err.Error()})
		return
	}

	// Validate the dependency graph across the full package set (not the
	// arch/version-filtered selection): every intra-repository requirement must
	// be met by some available version.
	ck.checkDependencies(label, be, pkgs)

	selected := ck.selectPackages(pkgs)
	ck.logf("%s: %d package(s) selected for %s check", label, len(selected), ck.cfg.Level)
	ck.checkPackages(ctx, be, label, selected)
}

// checkDependencies verifies that every intra-repository dependency is met. A
// requirement whose capability no package in the repository provides is an
// external dependency (e.g. glibc, /bin/sh) and is not reported; one the
// repository provides but at no satisfying version is a broken dependency graph,
// typically the result of a prune or removal that dropped a needed version.
func (ck *checker) checkDependencies(label string, be backend.Backend, pkgs []*repodata.Package) {
	problems := repodata.CheckDependencies(pkgs)
	if len(problems) == 0 {
		ck.add(Result{
			Target: label, Kind: "dependencies", Loc: be.String(), Status: StatusOK,
			Detail: "all intra-repository dependencies satisfied",
		})
		return
	}
	for _, dp := range problems {
		ck.add(Result{
			Target: label, Kind: "dependency", Loc: dp.Package.Location, Status: StatusFail,
			Detail: dp.String(),
		})
	}
}

// checkMetadataFile downloads and verifies one repomd index entry: its
// compressed size and checksum, then (after decompression) its open size and
// open checksum. It returns the decompressed contents (used for primary.xml)
// and the result record.
func (ck *checker) checkMetadataFile(ctx context.Context, be backend.Backend, label string, d repodata.DataEntry) ([]byte, Result) {
	kind := "metadata:" + d.Type
	loc := joinLoc(be.String(), d.Location)

	raw, err := ck.getAll(ctx, be, d.Location)
	if err != nil {
		return nil, Result{Target: label, Kind: kind, Loc: loc, Status: StatusFail, Detail: "fetch failed: " + err.Error()}
	}

	// Compressed (closed) size + checksum.
	if d.Size > 0 && int64(len(raw)) != d.Size {
		return nil, Result{
			Target: label, Kind: kind, Loc: loc, Status: StatusFail,
			Detail: fmt.Sprintf("size %d != metadata %d", len(raw), d.Size),
		}
	}
	if got, ok := verifyChecksum(raw, d.ChecksumType, d.Checksum); !ok {
		return nil, Result{
			Target: label, Kind: kind, Loc: loc, Status: StatusFail,
			Detail: fmt.Sprintf("%s %s != metadata %s", d.ChecksumType, got, d.Checksum),
		}
	}

	// Decompressed (open) size + checksum. The open checksum uses the same
	// algorithm as the closed checksum in createrepo/repomd output, so reuse the
	// entry's checksum type.
	dr, closeFn, derr := decompressReader(bytes.NewReader(raw), d.Location)
	if derr != nil {
		// The compressed file's size + checksum are still validated above.
		return nil, Result{
			Target: label, Kind: kind, Loc: loc, Status: StatusWarn,
			Detail: fmt.Sprintf("compressed file valid, but open-checksum not verified: %v", derr),
		}
	}
	openHasher, openSupported := newHasher(d.ChecksumType)
	decompressed, err := io.ReadAll(io.TeeReader(dr, openHasher))
	closeFn()
	if err != nil {
		return nil, Result{Target: label, Kind: kind, Loc: loc, Status: StatusFail, Detail: "decompressing: " + err.Error()}
	}

	var openIssues []string
	if d.OpenSize > 0 && int64(len(decompressed)) != d.OpenSize {
		openIssues = append(openIssues, fmt.Sprintf("open-size %d != metadata %d", len(decompressed), d.OpenSize))
	}
	if openSupported && d.OpenChecksum != "" {
		got := hex.EncodeToString(openHasher.Sum(nil))
		if !strings.EqualFold(got, d.OpenChecksum) {
			openIssues = append(openIssues, fmt.Sprintf("open-checksum %s != metadata %s", got, d.OpenChecksum))
		}
	}
	if len(openIssues) > 0 {
		return decompressed, Result{Target: label, Kind: kind, Loc: loc, Status: StatusFail, Detail: strings.Join(openIssues, "; ")}
	}

	return decompressed, Result{
		Target: label, Kind: kind, Loc: loc, Status: StatusOK,
		Detail: fmt.Sprintf("size %d, %s ok", len(raw), d.ChecksumType),
	}
}

// selectPackages applies the arch and package-name filters and, when
// LatestOnly is set, keeps only the highest EVR per (name, arch).
func (ck *checker) selectPackages(all []*repodata.Package) []*repodata.Package {
	archSet := toSet(ck.cfg.Arches)
	nameSet := toSet(ck.cfg.Packages)

	var filtered []*repodata.Package
	for _, p := range all {
		if len(nameSet) > 0 {
			if _, ok := nameSet[p.Name]; !ok {
				continue
			}
		}
		if len(archSet) > 0 {
			_, archOK := archSet[p.Arch]
			// noarch packages are applicable to every arch.
			if !archOK && p.Arch != "noarch" {
				continue
			}
		}
		filtered = append(filtered, p)
	}

	if !ck.cfg.LatestOnly {
		return filtered
	}

	type key struct{ name, arch string }
	best := map[key]*repodata.Package{}
	for _, p := range filtered {
		k := key{p.Name, p.Arch}
		cur, ok := best[k]
		if !ok {
			best[k] = p
			continue
		}
		if repodata.CompareEVR(p.Epoch, p.Version, p.Release, cur.Epoch, cur.Version, cur.Release) > 0 {
			best[k] = p
		}
	}
	out := make([]*repodata.Package, 0, len(best))
	for _, p := range best {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NEVRA() < out[j].NEVRA() })
	return out
}

// checkPackages runs the per-package checks with bounded concurrency.
func (ck *checker) checkPackages(ctx context.Context, be backend.Backend, label string, pkgs []*repodata.Package) {
	conc := ck.cfg.Concurrency
	if conc < 1 {
		conc = 1
	}
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for _, p := range pkgs {
		wg.Add(1)
		sem <- struct{}{}
		go func(p *repodata.Package) {
			defer wg.Done()
			defer func() { <-sem }()
			ck.add(ck.checkPackage(ctx, be, label, p))
		}(p)
	}
	wg.Wait()
}

func (ck *checker) checkPackage(ctx context.Context, be backend.Backend, label string, p *repodata.Package) Result {
	kind := p.NEVRA()
	loc := joinLoc(be.String(), p.Location)

	if ck.cfg.Level == LevelHead {
		return ck.headCheck(ctx, be, label, p, kind, loc)
	}
	return ck.fetchCheck(ctx, be, label, p, kind, loc)
}

// headCheck verifies a package exists with the expected size, and (when the
// backend can report a checksum without transferring the file) that its
// checksum matches the metadata's pkgid.
func (ck *checker) headCheck(ctx context.Context, be backend.Backend, label string, p *repodata.Package, kind, loc string) Result {
	c, cancel := ck.opCtx(ctx)
	defer cancel()
	fi, err := be.Stat(c, p.Location)
	if errors.Is(err, backend.ErrNotExist) {
		return Result{Target: label, Kind: kind, Loc: loc, Status: StatusFail, Detail: "package missing"}
	}
	if err != nil {
		return Result{Target: label, Kind: kind, Loc: loc, Status: StatusFail, Detail: "stat failed: " + err.Error()}
	}
	if fi.Size < 0 {
		return Result{Target: label, Kind: kind, Loc: loc, Status: StatusWarn, Detail: "backend did not report a size"}
	}
	if p.SizePackage > 0 && fi.Size != p.SizePackage {
		return Result{
			Target: label, Kind: kind, Loc: loc, Status: StatusFail,
			Detail: fmt.Sprintf("size mismatch: backend reports %d, metadata says %d", fi.Size, p.SizePackage),
		}
	}

	// Verify the checksum without downloading, if the backend can.
	if sum := ck.remoteChecksum(ctx, be, fi, p.Location, p.ChecksumType); sum != "" {
		if !strings.EqualFold(sum, p.PkgID) {
			return Result{
				Target: label, Kind: kind, Loc: loc, Status: StatusFail,
				Detail: fmt.Sprintf("checksum mismatch: backend reports %s, pkgid %s", sum, p.PkgID),
			}
		}
		return Result{
			Target: label, Kind: kind, Loc: loc, Status: StatusOK,
			Detail: fmt.Sprintf("exists, size %d, checksum ok", fi.Size),
		}
	}
	return Result{
		Target: label, Kind: kind, Loc: loc, Status: StatusOK,
		Detail: fmt.Sprintf("exists, size %d", fi.Size),
	}
}

// remoteChecksum returns a checksum for relpath obtained without downloading
// the object: either one Stat already reported, or one a RemoteHasher backend
// can compute. It returns "" when no download-free checksum is available.
func (ck *checker) remoteChecksum(ctx context.Context, be backend.Backend, fi *backend.FileInfo, relpath, algo string) string {
	if fi.Checksum != "" && strings.EqualFold(fi.ChecksumType, algo) {
		return fi.Checksum
	}
	hasher, ok := be.(backend.RemoteHasher)
	if !ok {
		return ""
	}
	c, cancel := ck.opCtx(ctx)
	defer cancel()
	sum, ok, err := hasher.Hash(c, relpath, algo)
	if err != nil || !ok {
		return ""
	}
	return sum
}

// fetchCheck downloads a package, verifies its size and checksum, and (when the
// rpm command is available) verifies its internal header/payload digests.
func (ck *checker) fetchCheck(ctx context.Context, be backend.Backend, label string, p *repodata.Package, kind, loc string) Result {
	c, cancel := ck.opCtx(ctx)
	defer cancel()
	rc, err := be.Get(c, p.Location)
	if errors.Is(err, backend.ErrNotExist) {
		return Result{Target: label, Kind: kind, Loc: loc, Status: StatusFail, Detail: "package missing"}
	}
	if err != nil {
		return Result{Target: label, Kind: kind, Loc: loc, Status: StatusFail, Detail: "fetch failed: " + err.Error()}
	}
	defer rc.Close()

	tmp, err := os.CreateTemp("", "repocheck-*.rpm")
	if err != nil {
		return Result{Target: label, Kind: kind, Loc: loc, Status: StatusFail, Detail: err.Error()}
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	hasher, supported := newHasher(p.ChecksumType)
	written, err := io.Copy(io.MultiWriter(tmp, hasher), rc)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Result{Target: label, Kind: kind, Loc: loc, Status: StatusFail, Detail: "downloading: " + err.Error()}
	}
	if p.SizePackage > 0 && written != p.SizePackage {
		return Result{
			Target: label, Kind: kind, Loc: loc, Status: StatusFail,
			Detail: fmt.Sprintf("size mismatch: downloaded %d bytes, metadata says %d", written, p.SizePackage),
		}
	}
	if supported && p.PkgID != "" {
		got := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(got, p.PkgID) {
			return Result{
				Target: label, Kind: kind, Loc: loc, Status: StatusFail,
				Detail: fmt.Sprintf("%s mismatch: downloaded %s, pkgid %s", p.ChecksumType, got, p.PkgID),
			}
		}
	}

	detail := fmt.Sprintf("size %d, %s ok", written, p.ChecksumType)
	if ck.rpm.Available() {
		rpmDetail, ok, rerr := ck.rpm.verifyPayload(tmpName)
		switch {
		case rerr != nil:
			detail += fmt.Sprintf("; rpm check error: %v", rerr)
		case !ok:
			return Result{
				Target: label, Kind: kind, Loc: loc, Status: StatusFail,
				Detail: fmt.Sprintf("size/checksum ok but rpm payload verification failed: %s", rpmDetail),
			}
		default:
			detail += "; rpm digests ok"
		}
	} else {
		detail += "; rpm payload check skipped (rpm not installed)"
	}
	return Result{Target: label, Kind: kind, Loc: loc, Status: StatusOK, Detail: detail}
}

// verifyChecksum computes the hash of data under the named algorithm and
// compares it to want. If the algorithm is unknown or want is empty, it reports
// ok=true (nothing to check) with the computed value for context.
func verifyChecksum(data []byte, algo, want string) (got string, ok bool) {
	hasher, supported := newHasher(algo)
	_, _ = hasher.Write(data)
	got = hex.EncodeToString(hasher.Sum(nil))
	if want == "" || !supported {
		return got, true
	}
	return got, strings.EqualFold(got, want)
}

// joinLoc joins a backend's display root with a repo-relative path using a
// single slash separator, for human-readable diagnostics.
func joinLoc(base, relpath string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(relpath, "/")
}

func toSet(items []string) map[string]struct{} {
	if len(items) == 0 {
		return nil
	}
	m := make(map[string]struct{}, len(items))
	for _, it := range items {
		if it != "" {
			m[it] = struct{}{}
		}
	}
	return m
}

// Summarize counts results by status.
func Summarize(results []Result) (ok, fail, warn, skip int) {
	for _, r := range results {
		switch r.Status {
		case StatusOK:
			ok++
		case StatusFail:
			fail++
		case StatusWarn:
			warn++
		case StatusSkip:
			skip++
		}
	}
	return
}
