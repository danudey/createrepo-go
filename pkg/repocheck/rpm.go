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
	"fmt"
	"os/exec"
	"strings"
)

// RPMTool wraps the optional rpm command used for deeper package validation at
// the fetch level.
type RPMTool struct {
	path      string // resolved rpm binary, empty if unavailable
	available bool
}

// DetectRPM locates the rpm command on PATH.
func DetectRPM() RPMTool {
	if p, err := exec.LookPath("rpm"); err == nil {
		return RPMTool{path: p, available: true}
	}
	return RPMTool{}
}

// Available reports whether the rpm command was found and so deeper package
// digest verification is possible.
func (t RPMTool) Available() bool { return t.available }

// verifyPayload runs `rpm --checksig --nosignature` against a downloaded RPM,
// which validates the package's internal header and payload digests (the
// MD5/SHA digests embedded in the RPM), catching corruption that a matching
// file-level checksum alone would not. GPG signatures are intentionally not
// required here (signature verification is a separate concern). It returns a
// human-readable detail string and whether the package verified.
func (t RPMTool) verifyPayload(path string) (detail string, ok bool, err error) {
	if !t.available {
		return "", false, fmt.Errorf("rpm command not available")
	}
	cmd := exec.Command(t.path, "--checksig", "--nosignature", "--verbose", path)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()

	text := out.String()
	// In verbose mode rpm prints one line per digest, e.g.
	//   Header SHA256 digest: OK
	//   Payload SHA256 digest: OK
	// A failure prints "NOT OK" / "BAD".
	lower := strings.ToLower(text)
	if runErr != nil || strings.Contains(lower, "not ok") || strings.Contains(lower, "bad") {
		return condenseDigestLines(text), false, nil
	}
	return condenseDigestLines(text), true, nil
}

// condenseDigestLines collapses rpm's multi-line digest output into a compact
// single-line summary.
func condenseDigestLines(text string) string {
	var parts []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.Contains(strings.ToLower(line), "digest") {
			parts = append(parts, line)
		}
	}
	if len(parts) == 0 {
		return strings.TrimSpace(text)
	}
	return strings.Join(parts, "; ")
}
