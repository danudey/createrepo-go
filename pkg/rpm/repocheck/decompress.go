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
	"crypto/md5"  // #nosec G501 -- a checksum type repomd.xml may name; see newHasher.
	"crypto/sha1" // #nosec G505 -- likewise.
	"crypto/sha256"
	"crypto/sha512"
	"hash"
	"strings"
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
		// #nosec G401 -- the algorithm is dictated by the repository metadata,
		// not chosen here. Old repositories still publish sha1 and md5 sums,
		// and refusing to compute them would mean refusing to check the repo.
		return sha1.New(), true
	case "sha384":
		return sha512.New384(), true
	case "md5":
		// #nosec G401 -- see the sha1 case above.
		return md5.New(), true
	default:
		return sha256.New(), false
	}
}
