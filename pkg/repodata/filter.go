package repodata

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Kind classifies a package by the role it plays in a repository. It is derived
// from the package's architecture and name, the same way dnf and the various
// mirroring tools distinguish binary packages from source and debug artifacts.
type Kind string

const (
	KindBinary      Kind = "binary"      // an ordinary installable package
	KindSource      Kind = "source"      // a source rpm (arch "src"/"nosrc")
	KindDebuginfo   Kind = "debuginfo"   // *-debuginfo (and -debuginfo-common)
	KindDebugsource Kind = "debugsource" // *-debugsource
)

// AllKinds lists every kind in a stable order, for help text and validation.
var AllKinds = []Kind{KindBinary, KindSource, KindDebuginfo, KindDebugsource}

// KindOf classifies a single package.
func KindOf(p *Package) Kind {
	switch p.Arch {
	case "src", "nosrc":
		return KindSource
	}
	switch {
	case strings.HasSuffix(p.Name, "-debugsource"):
		return KindDebugsource
	case strings.Contains(p.Name, "-debuginfo"):
		// Covers foo-debuginfo and the foo-debuginfo-common convention.
		return KindDebuginfo
	default:
		return KindBinary
	}
}

// ParseKinds turns a user-supplied list of kind names into Kinds. The alias
// "debug" expands to both debuginfo and debugsource, since operators almost
// always want to include or exclude the pair together.
func ParseKinds(names []string) ([]Kind, error) {
	var out []Kind
	seen := map[Kind]bool{}
	add := func(k Kind) {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for _, n := range names {
		switch strings.ToLower(strings.TrimSpace(n)) {
		case "":
			continue
		case "binary", "rpm", "bin":
			add(KindBinary)
		case "source", "src", "srpm":
			add(KindSource)
		case "debuginfo":
			add(KindDebuginfo)
		case "debugsource":
			add(KindDebugsource)
		case "debug":
			add(KindDebuginfo)
			add(KindDebugsource)
		default:
			return nil, fmt.Errorf("unknown package kind %q (valid: binary, source, debuginfo, debugsource, debug)", n)
		}
	}
	return out, nil
}

// Filter selects a subset of a repository's packages. A zero Filter selects
// everything.
type Filter struct {
	// Include and Exclude are glob patterns (filepath.Match syntax) matched
	// against a package's name and its name-version[-release][.arch] forms, so
	// both "hello" and "hello-2.10-*" select the same package. An empty Include
	// list means "every package"; Exclude is applied after Include.
	Include []string
	Exclude []string

	// Arches restricts the selection to the named architectures. noarch
	// packages are always kept alongside a requested arch, since a repository
	// for one architecture still needs them. Source rpms are not: their arch is
	// "src", so name it explicitly to keep them. An arch that no package in the
	// repository provides is an error.
	Arches []string

	// Kinds, when non-empty, keeps only packages of those kinds. ExcludeKinds
	// drops packages of those kinds and is applied afterwards.
	Kinds        []Kind
	ExcludeKinds []Kind

	// LatestOnly keeps only the highest epoch:version-release of each
	// name+arch.
	LatestOnly bool
}

// Empty reports whether the filter would select every package unchanged.
func (f Filter) Empty() bool {
	return len(f.Include) == 0 && len(f.Exclude) == 0 && len(f.Arches) == 0 &&
		len(f.Kinds) == 0 && len(f.ExcludeKinds) == 0 && !f.LatestOnly
}

// Apply returns the packages the filter selects, in the input order (or sorted
// by NEVRA when LatestOnly collapsed the set). It also returns advisory
// warnings — currently one per Include/Exclude pattern that matched nothing —
// and an error if a requested architecture is absent from the repository.
func (f Filter) Apply(pkgs []*Package) (selected []*Package, warnings []string, err error) {
	if err := f.checkArches(pkgs); err != nil {
		return nil, nil, err
	}

	kindKeep := kindSet(f.Kinds)
	kindDrop := kindSet(f.ExcludeKinds)
	matchedInclude := make(map[string]bool, len(f.Include))
	matchedExclude := make(map[string]bool, len(f.Exclude))

	for _, p := range pkgs {
		if !f.archAllows(p) {
			continue
		}
		k := KindOf(p)
		if len(kindKeep) > 0 && !kindKeep[k] {
			continue
		}
		if kindDrop[k] {
			continue
		}
		if len(f.Include) > 0 {
			pat, ok := matchAny(f.Include, p)
			if !ok {
				continue
			}
			matchedInclude[pat] = true
		}
		if pat, ok := matchAny(f.Exclude, p); ok {
			matchedExclude[pat] = true
			continue
		}
		selected = append(selected, p)
	}

	for _, pat := range f.Include {
		if !matchedInclude[pat] {
			warnings = append(warnings, fmt.Sprintf("--include %q matched no package", pat))
		}
	}
	for _, pat := range f.Exclude {
		if !matchedExclude[pat] {
			warnings = append(warnings, fmt.Sprintf("--exclude %q matched no package", pat))
		}
	}

	if f.LatestOnly {
		selected = LatestVersions(selected)
	}
	return selected, warnings, nil
}

// checkArches reports an error for any requested architecture that no package in
// the repository is built for. noarch does not stand in for a missing arch here:
// asking to copy "aarch64" out of a repository holding only x86_64 binaries is a
// mistake worth failing on, which is exactly what the check is for.
func (f Filter) checkArches(pkgs []*Package) error {
	if len(f.Arches) == 0 {
		return nil
	}
	present := map[string]bool{}
	for _, p := range pkgs {
		present[p.Arch] = true
	}
	var missing []string
	for _, a := range f.Arches {
		if !present[a] {
			missing = append(missing, a)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	have := make([]string, 0, len(present))
	for a := range present {
		have = append(have, a)
	}
	sort.Strings(have)
	return fmt.Errorf("architecture %s is not present in the repository (it holds: %s)",
		strings.Join(missing, ", "), strings.Join(have, ", "))
}

func (f Filter) archAllows(p *Package) bool {
	if len(f.Arches) == 0 {
		return true
	}
	// noarch packages are applicable to every architecture, so a single-arch
	// copy keeps them.
	if p.Arch == "noarch" {
		return true
	}
	for _, a := range f.Arches {
		if p.Arch == a {
			return true
		}
	}
	return false
}

// LatestVersions keeps only the highest epoch:version-release of each
// name+arch, returning the result sorted by NEVRA.
func LatestVersions(pkgs []*Package) []*Package {
	type key struct{ name, arch string }
	best := map[key]*Package{}
	for _, p := range pkgs {
		k := key{p.Name, p.Arch}
		cur, ok := best[k]
		if !ok || CompareEVR(p.Epoch, p.Version, p.Release, cur.Epoch, cur.Version, cur.Release) > 0 {
			best[k] = p
		}
	}
	out := make([]*Package, 0, len(best))
	for _, p := range best {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NEVRA() < out[j].NEVRA() })
	return out
}

// matchAny reports whether any pattern matches the package, returning the
// pattern that did.
func matchAny(patterns []string, p *Package) (string, bool) {
	for _, pat := range patterns {
		if matchPackage(pat, p) {
			return pat, true
		}
	}
	return "", false
}

// matchPackage reports whether a glob pattern selects a package. The pattern is
// tried against progressively more qualified names so that a bare name, a
// name-version, a full NEVRA, and epoch-qualified forms all work.
func matchPackage(pattern string, p *Package) bool {
	nv := p.Name + "-" + p.Version
	nvr := nv + "-" + p.Release
	candidates := []string{
		p.Name,
		nv,
		nvr,
		nvr + "." + p.Arch,
		p.NEVRA(),
		p.Version,
		p.EVR(),
	}
	for _, c := range candidates {
		if ok, err := filepath.Match(pattern, c); err == nil && ok {
			return true
		}
	}
	return false
}

func kindSet(kinds []Kind) map[Kind]bool {
	if len(kinds) == 0 {
		return nil
	}
	m := make(map[Kind]bool, len(kinds))
	for _, k := range kinds {
		m[k] = true
	}
	return m
}
