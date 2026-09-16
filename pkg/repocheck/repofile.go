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
	"bufio"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// repoSection is a single "[section]" stanza of a yum/dnf .repo file.
type repoSection struct {
	ID         string // the [section] id
	Name       string // the name= field, if any
	BaseURL    string // the baseurl= field (variables not yet substituted)
	MirrorList string // mirrorlist= / metalink=, if any (unsupported)
	Enabled    bool
}

// parseRepoFile parses the INI-style content of a .repo file into its sections.
// It is intentionally lenient: unknown keys are ignored.
func parseRepoFile(content string) ([]repoSection, error) {
	var sections []repoSection
	var cur *repoSection

	flush := func() {
		if cur != nil {
			sections = append(sections, *cur)
			cur = nil
		}
	}

	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			flush()
			cur = &repoSection{
				ID:      strings.TrimSpace(line[1 : len(line)-1]),
				Enabled: true, // dnf defaults enabled=1 when absent
			}
			continue
		}
		if cur == nil {
			// Key/value outside of a section; skip it.
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(strings.ToLower(key))
		value = strings.TrimSpace(value)
		switch key {
		case "name":
			cur.Name = value
		case "baseurl":
			// baseurl may be a space-separated list; take the first entry.
			if fields := strings.Fields(value); len(fields) > 0 {
				cur.BaseURL = fields[0]
			}
		case "mirrorlist", "metalink":
			cur.MirrorList = value
		case "enabled":
			cur.Enabled = value == "1" || strings.EqualFold(value, "true") || strings.EqualFold(value, "yes")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	flush()

	if len(sections) == 0 {
		return nil, fmt.Errorf("no [section] stanzas found; this does not look like a .repo file")
	}
	return sections, nil
}

// substituteVars replaces the dnf/yum repo variables we support. Both "$var"
// and "${var}" forms are handled. "$arch" is treated as an alias of "$basearch".
func substituteVars(s, releasever, basearch string) string {
	r := strings.NewReplacer(
		"${releasever}", releasever,
		"$releasever", releasever,
		"${basearch}", basearch,
		"$basearch", basearch,
		"${arch}", basearch,
		"$arch", basearch,
	)
	return r.Replace(s)
}

// target is a concrete repository to validate: a fully-substituted baseurl plus
// the dimensions that produced it (for labelling output).
type target struct {
	label      string
	baseURL    string
	releasever string
	basearch   string
}

// expandTargets produces the cross-product of (section × releasever × arch) that
// need to be checked. A releasever is only iterated when the baseurl actually
// references $releasever; likewise for $basearch. The arches list is always used
// for package-level filtering regardless of whether it appears in the URL.
// Disabled sections are skipped.
func expandTargets(sections []repoSection, releasevers, arches []string) ([]target, []string) {
	var targets []target
	var warnings []string

	for _, sec := range sections {
		if !sec.Enabled {
			warnings = append(warnings, fmt.Sprintf("section %q is disabled (enabled=0); skipping", sec.ID))
			continue
		}
		if sec.BaseURL == "" {
			if sec.MirrorList != "" {
				warnings = append(warnings, fmt.Sprintf("section %q uses mirrorlist/metalink which is not supported; skipping", sec.ID))
			} else {
				warnings = append(warnings, fmt.Sprintf("section %q has no baseurl; skipping", sec.ID))
			}
			continue
		}

		relList := []string{""}
		if strings.Contains(sec.BaseURL, "$releasever") || strings.Contains(sec.BaseURL, "${releasever}") {
			if len(releasevers) == 0 {
				warnings = append(warnings, fmt.Sprintf("section %q baseurl references $releasever but no --releasever was provided", sec.ID))
				continue
			}
			relList = releasevers
		}

		archList := []string{""}
		usesArch := strings.Contains(sec.BaseURL, "$basearch") || strings.Contains(sec.BaseURL, "${basearch}") ||
			strings.Contains(sec.BaseURL, "$arch") || strings.Contains(sec.BaseURL, "${arch}")
		if usesArch {
			if len(arches) == 0 {
				warnings = append(warnings, fmt.Sprintf("section %q baseurl references $basearch but no --arch was provided", sec.ID))
				continue
			}
			archList = arches
		}

		for _, rel := range relList {
			for _, arch := range archList {
				base := substituteVars(sec.BaseURL, rel, arch)
				if !strings.HasSuffix(base, "/") {
					base += "/"
				}
				label := sec.ID
				switch {
				case rel != "" && arch != "":
					label = fmt.Sprintf("%s (releasever=%s arch=%s)", sec.ID, rel, arch)
				case rel != "":
					label = fmt.Sprintf("%s (releasever=%s)", sec.ID, rel)
				case arch != "":
					label = fmt.Sprintf("%s (arch=%s)", sec.ID, arch)
				}
				targets = append(targets, target{
					label:      label,
					baseURL:    base,
					releasever: rel,
					basearch:   arch,
				})
			}
		}
	}
	return targets, warnings
}

// loadRepoInput resolves the user-supplied input into repo sections. The input
// may be:
//   - a path or backend URL to a .repo file (ending in ".repo")
//   - any repository location understood by the backend package: a local
//     directory, file://, sftp://, s3://, gs:// or http(s):// URL.
//
// A .repo file is fetched (over http/https) or read from local disk and parsed
// into its sections; anything else is treated as a single bare baseurl.
func loadRepoInput(input string, fetch func(string) ([]byte, error)) ([]repoSection, error) {
	if strings.HasSuffix(strings.ToLower(pathOf(input)), ".repo") {
		var data []byte
		var err error
		if isHTTPURL(input) {
			data, err = fetch(input)
			if err != nil {
				return nil, fmt.Errorf("fetching repo file %q: %w", input, err)
			}
		} else {
			data, err = os.ReadFile(localPath(input))
			if err != nil {
				return nil, fmt.Errorf("reading repo file %q: %w", input, err)
			}
		}
		return parseRepoFile(string(data))
	}

	// Not a .repo file: treat the input as a bare repository location. The
	// backend package resolves the scheme when the target is opened.
	return []repoSection{{
		ID:      "repo",
		Name:    "baseurl",
		BaseURL: input,
		Enabled: true,
	}}, nil
}

// isHTTPURL reports whether s is an http or https URL.
func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https")
}

// localPath strips a file:// scheme, returning a filesystem path unchanged
// otherwise.
func localPath(s string) string {
	if strings.HasPrefix(s, "file://") {
		if u, err := url.Parse(s); err == nil {
			return u.Path
		}
	}
	return s
}

// pathOf returns the path component of a URL, or the string itself if it is not
// a URL (so a local ".repo" path is still recognised).
func pathOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" {
		return rawURL
	}
	return u.Path
}
