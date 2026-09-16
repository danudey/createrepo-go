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
	"compress/bzip2"
	"compress/gzip"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"fmt"
	"hash"
	"io"
	"os/exec"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// newHasher returns a hash for the named algorithm. supported reports whether
// the algorithm is recognised; for unknown algorithms a sha256 hasher is
// returned so a file can still be processed (but not verified against the
// expected value).
func newHasher(typ string) (h hash.Hash, supported bool) {
	switch strings.ToLower(typ) {
	case "sha256", "sha2-256":
		return sha256.New(), true
	case "sha512", "sha2-512":
		return sha512.New(), true
	case "sha1", "sha":
		return sha1.New(), true
	case "sha384":
		return sha512.New384(), true
	case "md5":
		return md5.New(), true
	default:
		return sha256.New(), false
	}
}

// decompressReader wraps r with a decompressor chosen from the href's
// extension. For .xz it shells out to the "xz" command (no pure-Go xz decoder
// is bundled); the returned closeFn must be called to release any external
// process or decoder resources.
func decompressReader(r io.Reader, href string) (out io.Reader, closeFn func(), err error) {
	lower := strings.ToLower(href)
	noop := func() {}
	switch {
	case strings.HasSuffix(lower, ".zst") || strings.HasSuffix(lower, ".zstd"):
		zr, err := zstd.NewReader(r)
		if err != nil {
			return nil, noop, err
		}
		return zr.IOReadCloser(), func() { zr.Close() }, nil
	case strings.HasSuffix(lower, ".gz"):
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, noop, err
		}
		return gz, func() { _ = gz.Close() }, nil
	case strings.HasSuffix(lower, ".bz2"):
		return bzip2.NewReader(r), noop, nil
	case strings.HasSuffix(lower, ".xz"):
		return xzDecompress(r)
	default:
		// Assume uncompressed XML.
		return r, noop, nil
	}
}

// xzDecompress pipes r through the external xz command.
func xzDecompress(r io.Reader) (io.Reader, func(), error) {
	if _, err := exec.LookPath("xz"); err != nil {
		return nil, func() {}, fmt.Errorf("xz-compressed metadata requires the 'xz' command, which was not found in PATH")
	}
	cmd := exec.Command("xz", "--decompress", "--stdout")
	cmd.Stdin = r
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, func() {}, err
	}
	if err := cmd.Start(); err != nil {
		return nil, func() {}, err
	}
	return pipe, func() { _ = cmd.Wait() }, nil
}
