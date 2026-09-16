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

// Package repocheck validates dnf/yum (rpm-md) repositories. It reads the
// repository metadata, parses the package index, and checks that the metadata
// is internally consistent and that the packages it references actually exist
// with the size and checksum the metadata claims. This catches repositories
// where a published package file has diverged from what createrepo indexed (for
// example when one RPM is published over another), which manifests for end
// users as dnf errors like:
//
//	[MIRROR] calico-fluent-bit-...: Interrupted by header callback:
//	  Server reports Content-Length: 13264076 but expected size is: 13262596
//
// Unlike a purely HTTP-based checker, repocheck runs against any repository the
// backend package can reach — a local directory, an SSH/SFTP host, an S3 or GCS
// bucket, or an HTTP(S) server — so the same validation covers a repository at
// rest as well as one published live.
package repocheck

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/danudey/createrepo-go/pkg/backend"
)

// repomdPath is the location of the repository index relative to the repo root.
const repomdPath = "repodata/repomd.xml"

const userAgent = "createrepo-go/check (+https://github.com/danudey/createrepo-go)"

// Options describes a full validation run for Run.
type Options struct {
	// Input is a repository location (local path or file://, sftp://, s3://,
	// gs:// or http(s):// URL), or a path/URL to a .repo file (ending in
	// ".repo").
	Input string
	// Releasevers are the $releasever values to substitute into a .repo file's
	// baseurl (e.g. "8", "9"). May be empty when the baseurl has no $releasever.
	Releasevers []string
	// Arches restricts which package architectures are checked; empty means
	// every architecture present in the metadata. Values are also substituted
	// for $basearch in a .repo file's baseurl.
	Arches []string
	// Packages restricts which package names are checked; empty means all.
	Packages []string
	// Level and LatestOnly control the depth and breadth of the checks.
	Level      Level
	LatestOnly bool
	// Concurrency bounds the number of packages checked in parallel.
	Concurrency int
	// Timeout bounds each individual backend operation. Zero means no timeout.
	Timeout time.Duration

	// RPM is the (optional) rpm command used for payload-digest verification at
	// the fetch level. Build it with DetectRPM.
	RPM RPMTool

	// Logf, when non-nil, receives a line for every individual check.
	Logf func(format string, args ...any)
	// OnTargetStart, when non-nil, is called once per concrete repository before
	// its checks begin (useful for printing a per-target header).
	OnTargetStart func(t Target)
}

// Target identifies a concrete repository that was checked.
type Target struct {
	Label      string
	BaseURL    string
	Releasever string
	Basearch   string
}

// Run executes the validation described by opts. It returns the per-artifact
// results and any non-fatal configuration warnings (e.g. skipped sections). A
// returned error indicates the run could not be performed at all (bad input, or
// no repositories to check); per-artifact failures are reported via the
// results' Status, not the error.
func Run(ctx context.Context, opts Options) (results []Result, warnings []string, err error) {
	sections, err := loadRepoInput(opts.Input, fetchOverHTTP(opts.Timeout))
	if err != nil {
		return nil, nil, err
	}

	targets, warnings := expandTargets(sections, opts.Releasevers, opts.Arches)
	if len(targets) == 0 {
		return nil, warnings, fmt.Errorf("no repositories to check")
	}

	cfg := Config{
		Level:       opts.Level,
		LatestOnly:  opts.LatestOnly,
		Arches:      opts.Arches,
		Packages:    opts.Packages,
		Concurrency: opts.Concurrency,
		Timeout:     opts.Timeout,
	}
	ck := newChecker(cfg, opts.RPM, opts.Logf)

	for _, t := range targets {
		if opts.OnTargetStart != nil {
			opts.OnTargetStart(Target{
				Label:      t.label,
				BaseURL:    t.baseURL,
				Releasever: t.releasever,
				Basearch:   t.basearch,
			})
		}
		be, err := backend.Open(ctx, t.baseURL)
		if err != nil {
			ck.add(Result{Target: t.label, Kind: "repository", Loc: t.baseURL, Status: StatusFail,
				Detail: "cannot open repository: " + err.Error()})
			continue
		}
		ck.checkTarget(ctx, be, t.label)
		if c, ok := be.(backend.Closer); ok {
			_ = c.Close()
		}
	}

	return ck.Results(), warnings, nil
}

// fetchOverHTTP returns a function that downloads a .repo file over HTTP(S).
func fetchOverHTTP(timeout time.Duration) func(string) ([]byte, error) {
	client := &http.Client{Timeout: timeout}
	return func(rawURL string) ([]byte, error) {
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("HTTP %s", resp.Status)
		}
		return io.ReadAll(resp.Body)
	}
}
