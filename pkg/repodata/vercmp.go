package repodata

// This file implements rpm's version comparison (rpmvercmp) so callers can
// order package versions without depending on the rpm command. Version and
// release strings are split into maximal runs of digits or letters (all other
// characters are separators); runs are compared pairwise, digit runs always
// sort above letter runs, digit runs compare numerically (ignoring leading
// zeros), and the modern '~' (sorts before everything, including the empty
// string) and '^' (sorts after a bare base version) separators are honored so
// pre-release and post-release builds order correctly.

// CompareVersions compares two rpm version (or release) strings and returns
// -1, 0 or 1.
func CompareVersions(a, b string) int {
	if a == b {
		return 0
	}
	ai, bi := 0, 0
	for ai < len(a) || bi < len(b) {
		// Skip non-alphanumeric separators, except tilde and caret which are
		// significant.
		for ai < len(a) && !isAlnum(a[ai]) && a[ai] != '~' && a[ai] != '^' {
			ai++
		}
		for bi < len(b) && !isAlnum(b[bi]) && b[bi] != '~' && b[bi] != '^' {
			bi++
		}

		// Tilde: sorts before everything.
		aTilde := ai < len(a) && a[ai] == '~'
		bTilde := bi < len(b) && b[bi] == '~'
		if aTilde || bTilde {
			switch {
			case !aTilde:
				return 1
			case !bTilde:
				return -1
			}
			ai++
			bi++
			continue
		}

		// Caret: sorts after everything when the other side is exhausted.
		aCaret := ai < len(a) && a[ai] == '^'
		bCaret := bi < len(b) && b[bi] == '^'
		if aCaret || bCaret {
			switch {
			case ai >= len(a):
				return -1
			case bi >= len(b):
				return 1
			case !aCaret:
				return 1
			case !bCaret:
				return -1
			}
			ai++
			bi++
			continue
		}

		if ai >= len(a) || bi >= len(b) {
			break
		}

		// Compare a run of digits or a run of letters.
		isNum := isDigit(a[ai])
		as, ae := ai, ai
		bs, be := bi, bi
		if isNum {
			for ae < len(a) && isDigit(a[ae]) {
				ae++
			}
			for be < len(b) && isDigit(b[be]) {
				be++
			}
		} else {
			for ae < len(a) && isAlpha(a[ae]) {
				ae++
			}
			for be < len(b) && isAlpha(b[be]) {
				be++
			}
		}

		aSeg := a[as:ae]
		bSeg := b[bs:be]
		// A numeric segment always wins against an alphabetic one or an empty
		// counterpart.
		if bs == be {
			if isNum {
				return 1
			}
			return -1
		}
		if isNum != isDigit(b[bs]) {
			if isNum {
				return 1
			}
			return -1
		}

		if isNum {
			aSeg = trimLeadingZeros(aSeg)
			bSeg = trimLeadingZeros(bSeg)
			if len(aSeg) != len(bSeg) {
				if len(aSeg) < len(bSeg) {
					return -1
				}
				return 1
			}
		}
		if c := compareStr(aSeg, bSeg); c != 0 {
			return c
		}
		ai, bi = ae, be
	}

	switch {
	case ai >= len(a) && bi >= len(b):
		return 0
	case ai >= len(a):
		return -1
	default:
		return 1
	}
}

// CompareEVR compares two full epoch:version-release tuples and returns -1, 0
// or 1. Epoch dominates; version and release fall back to CompareVersions.
func CompareEVR(aEpoch int, aVer, aRel string, bEpoch int, bVer, bRel string) int {
	if aEpoch != bEpoch {
		if aEpoch < bEpoch {
			return -1
		}
		return 1
	}
	if c := CompareVersions(aVer, bVer); c != 0 {
		return c
	}
	return CompareVersions(aRel, bRel)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isAlnum(c byte) bool { return isDigit(c) || isAlpha(c) }

func trimLeadingZeros(s string) string {
	i := 0
	for i < len(s)-1 && s[i] == '0' {
		i++
	}
	return s[i:]
}

func compareStr(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
