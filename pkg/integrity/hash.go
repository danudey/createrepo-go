package integrity

import (
	"crypto/md5"  // #nosec G501 -- a checksum type repository metadata may name; only compared
	"crypto/sha1" // #nosec G505 -- likewise
	"crypto/sha256"
	"crypto/sha512"
	"hash"
	"strings"
)

// NewHash returns a hash for a checksum type as repository metadata names it
// ("sha256", "sha", "sha1", "sha512", "md5", …), and whether the type is
// known. The weak types are computed because old repositories still publish
// them: refusing to compute them would mean refusing to check the repository.
func NewHash(algo string) (hash.Hash, bool) {
	switch strings.ToLower(strings.ReplaceAll(algo, "-", "")) {
	case "", "sha256", "sha2256":
		return sha256.New(), true
	case "sha224":
		return sha256.New224(), true
	case "sha384":
		return sha512.New384(), true
	case "sha512", "sha2512":
		return sha512.New(), true
	case "sha", "sha1":
		return sha1.New(), true // #nosec G401 -- see above
	case "md5":
		return md5.New(), true // #nosec G401 -- see above
	}
	return nil, false
}

// IsSHA256 reports whether a checksum type names sha256, the only one the
// backends can compute or record without transferring a file.
func IsSHA256(algo string) bool {
	switch strings.ToLower(strings.ReplaceAll(algo, "-", "")) {
	case "", "sha256", "sha2256":
		return true
	}
	return false
}
