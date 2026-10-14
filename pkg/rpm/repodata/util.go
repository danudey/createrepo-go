package repodata

import (
	"fmt"
	"strings"
)

// formatEVR renders epoch:version-release, omitting a zero epoch the way the
// rpm tooling conventionally displays it.
func formatEVR(epoch int, version, release string) string {
	if epoch != 0 {
		return fmt.Sprintf("%d:%s-%s", epoch, version, release)
	}
	return fmt.Sprintf("%s-%s", version, release)
}

// formatNEVRA renders the full name-[epoch:]version-release.arch identifier.
func formatNEVRA(name string, epoch int, version, release, arch string) string {
	return fmt.Sprintf("%s-%s.%s", name, formatEVR(epoch, version, release), arch)
}

// IsPrimaryFile reports whether a file path is included in the primary.xml
// <file> list. This mirrors createrepo_c's filter: files under a "bin/"
// directory, anything in /etc, and the sendmail symlink.
func IsPrimaryFile(path string) bool {
	return strings.Contains(path, "bin/") ||
		strings.HasPrefix(path, "/etc/") ||
		path == "/usr/lib/sendmail"
}

// PrimaryFiles returns the subset of a package's files that belong in the
// primary metadata, preserving order.
func (p *Package) PrimaryFiles() []File {
	var out []File
	for _, f := range p.Files {
		if IsPrimaryFile(f.Path) {
			out = append(out, f)
		}
	}
	return out
}
