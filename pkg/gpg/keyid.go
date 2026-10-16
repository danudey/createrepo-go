package gpg

import (
	"fmt"
	"strings"
)

// CheckKeyID reports whether s is a hexadecimal key id or fingerprint: a short
// (8) or long (16) key id, or a v4 (40) or v6 (64) fingerprint, optionally
// prefixed with "0x". A trailing "!" is allowed too; it is how gpg is told to
// use exactly that key rather than picking one of its subkeys.
//
// It is what a key id read from a repository's config file must look like
// before it goes anywhere near gpg or rpmsign. That file is not trusted (copy
// and verify read it from the source repository), and both tools treat a key
// id as more than a name: gpg parses one starting with "-" as an option, and
// rpm expands %(...) in a --define as a shell command.
func CheckKeyID(s string) error {
	hex := strings.TrimSuffix(s, "!")
	if len(hex) > 2 && (hex[:2] == "0x" || hex[:2] == "0X") {
		hex = hex[2:]
	}
	switch len(hex) {
	case 8, 16, 40, 64:
		if strings.Trim(hex, "0123456789abcdefABCDEF") == "" {
			return nil
		}
	}
	return fmt.Errorf("key id %q is not a hexadecimal key id or fingerprint", s)
}

// LongKeyID reduces a key id or fingerprint to the lowercase 16-hex long key id
// that a signature packet carries, leaving anything shorter alone. A "0x"
// prefix and a trailing "!" are dropped, as neither is part of the id.
func LongKeyID(s string) string {
	s = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
	s = strings.TrimPrefix(strings.TrimSuffix(s, "!"), "0x")
	if len(s) > 16 {
		return s[len(s)-16:]
	}
	return s
}
