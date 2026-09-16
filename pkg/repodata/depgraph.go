package repodata

// This file implements a small dependency solver over a repository's package
// set. It answers two questions used when verifying or pruning a repository:
//
//   - CheckDependencies: which requirements of the repository's packages are
//     unmet, considering only intra-repository dependencies (a requirement whose
//     capability no package in the repository provides at all — glibc, /bin/sh,
//     rpmlib(...) — is an external dependency and out of scope).
//   - RemovalBreakages: which packages cannot be removed from a set without
//     leaving some surviving package's requirement unsatisfied — e.g. package B
//     requires A = 1.0 specifically, so A-1.0 must not be pruned even though a
//     newer A-1.2 exists.
//
// Version-range matching mirrors rpm's rpmdsCompare: an unversioned provide or
// require matches on name alone; otherwise the version ranges must overlap.

import (
	"fmt"
	"strings"
)

// Constraint renders a dependency entry as "name op version", or just "name"
// when the entry carries no version constraint.
func (e Entry) Constraint() string {
	op := map[string]string{"EQ": "=", "LT": "<", "GT": ">", "LE": "<=", "GE": ">="}[e.Flags]
	if op == "" {
		return e.Name
	}
	v := e.Ver
	if e.Epoch > 0 {
		v = fmt.Sprintf("%d:%s", e.Epoch, v)
	}
	if e.Rel != "" {
		v += "-" + e.Rel
	}
	return fmt.Sprintf("%s %s %s", e.Name, op, v)
}

// sense captures the comparison operators a dependency flag encodes.
type sense struct{ lt, eq, gt bool }

func parseSense(flags string) sense {
	switch flags {
	case "EQ":
		return sense{eq: true}
	case "LT":
		return sense{lt: true}
	case "GT":
		return sense{gt: true}
	case "LE":
		return sense{lt: true, eq: true}
	case "GE":
		return sense{gt: true, eq: true}
	default:
		return sense{}
	}
}

// entrySatisfies reports whether a Provides entry prov satisfies a Requires
// entry req; the two names are assumed already equal. An unversioned provide or
// require matches on name alone. Otherwise the two version ranges must overlap,
// following rpm's rpmdsCompare rules.
func entrySatisfies(prov, req Entry) bool {
	if prov.Flags == "" || req.Flags == "" {
		return true
	}
	ps, rs := parseSense(prov.Flags), parseSense(req.Flags)
	switch cmp := compareDepEVR(prov, req); {
	case cmp < 0 && (ps.gt || rs.lt):
		return true
	case cmp > 0 && (ps.lt || rs.gt):
		return true
	case cmp == 0 && ((ps.eq && rs.eq) || (ps.lt && rs.lt) || (ps.gt && rs.gt)):
		return true
	default:
		return false
	}
}

// compareDepEVR compares two versioned dependency entries. Like rpm, the epoch
// is always compared (a missing epoch is normalized to 0 by the parser), the
// version is always compared, and the release is compared only when both entries
// carry one — so a requirement of "foo = 1.2" (no release) matches a provide of
// "foo = 1.2-3".
func compareDepEVR(a, b Entry) int {
	if a.Epoch != b.Epoch {
		if a.Epoch < b.Epoch {
			return -1
		}
		return 1
	}
	if c := CompareVersions(a.Ver, b.Ver); c != 0 {
		return c
	}
	if a.Rel != "" && b.Rel != "" {
		return CompareVersions(a.Rel, b.Rel)
	}
	return 0
}

// selfProvide is the capability a package implicitly provides: its own name at
// its own epoch:version-release.
func selfProvide(p *Package) Entry {
	return Entry{Name: p.Name, Flags: "EQ", Epoch: p.Epoch, Ver: p.Version, Rel: p.Release}
}

// skipRequirement filters out requirements a repository is never expected to
// satisfy from its own packages: rpm's internal rpmlib(...) feature probes and
// rich/boolean dependencies (which begin with '(').
func skipRequirement(req Entry) bool {
	return strings.HasPrefix(req.Name, "rpmlib(") || strings.HasPrefix(req.Name, "(")
}

// providerSatisfies reports whether package p, on its own, satisfies req via its
// name (self-provide), an explicit Provides entry, or a packaged file path.
func providerSatisfies(p *Package, req Entry) bool {
	if strings.HasPrefix(req.Name, "/") {
		for _, f := range p.Files {
			if f.Path == req.Name {
				return true
			}
		}
	}
	if req.Name == p.Name && entrySatisfies(selfProvide(p), req) {
		return true
	}
	for _, prov := range p.Provides {
		if prov.Name == req.Name && entrySatisfies(prov, req) {
			return true
		}
	}
	return false
}

// providerIndex maps capability and file names to the packages (and the specific
// Provides entries) that offer them, for fast requirement lookups.
type providerIndex struct {
	caps  map[string][]capProvider
	files map[string][]*Package
}

type capProvider struct {
	pkg   *Package
	entry Entry
}

func buildProviderIndex(pkgs []*Package) *providerIndex {
	pi := &providerIndex{caps: map[string][]capProvider{}, files: map[string][]*Package{}}
	for _, p := range pkgs {
		pi.caps[p.Name] = append(pi.caps[p.Name], capProvider{p, selfProvide(p)})
		for _, prov := range p.Provides {
			pi.caps[prov.Name] = append(pi.caps[prov.Name], capProvider{p, prov})
		}
		for _, f := range p.Files {
			pi.files[f.Path] = append(pi.files[f.Path], p)
		}
	}
	return pi
}

// satisfy returns the first package in the index that satisfies req, or nil.
func (pi *providerIndex) satisfy(req Entry) *Package {
	if strings.HasPrefix(req.Name, "/") {
		if ps := pi.files[req.Name]; len(ps) > 0 {
			return ps[0]
		}
	}
	for _, cp := range pi.caps[req.Name] {
		if entrySatisfies(cp.entry, req) {
			return cp.pkg
		}
	}
	return nil
}

// nameKnown reports whether any package in the index provides the requirement's
// capability name at all (ignoring the version), which distinguishes an
// intra-repository dependency from an external one.
func (pi *providerIndex) nameKnown(req Entry) bool {
	if strings.HasPrefix(req.Name, "/") && len(pi.files[req.Name]) > 0 {
		return true
	}
	return len(pi.caps[req.Name]) > 0
}

// DependencyProblem is a requirement of a package in the repository that the
// repository's own packages could satisfy by name — some package provides the
// capability — but no available version satisfies the version constraint. A
// requirement whose capability no package in the repository provides is treated
// as an external dependency (e.g. glibc, /bin/sh) and is not reported.
type DependencyProblem struct {
	Package  *Package
	Requires Entry
}

func (p DependencyProblem) String() string {
	return fmt.Sprintf("%s requires %q, which no available version provides",
		p.Package.NEVRA(), p.Requires.Constraint())
}

// CheckDependencies reports every intra-repository requirement that no package
// in pkgs satisfies. It is used to verify a repository and to detect a prune or
// removal that has broken the dependency graph. External dependencies are out of
// scope; see DependencyProblem.
func CheckDependencies(pkgs []*Package) []DependencyProblem {
	pi := buildProviderIndex(pkgs)
	var problems []DependencyProblem
	for _, p := range pkgs {
		for _, req := range p.Requires {
			if skipRequirement(req) {
				continue
			}
			if pi.satisfy(req) != nil {
				continue
			}
			if !pi.nameKnown(req) {
				continue // external dependency, not the repository's concern
			}
			problems = append(problems, DependencyProblem{Package: p, Requires: req})
		}
	}
	return problems
}

// Breakage records that a package (Dependent) has a requirement (Requires) that
// only Provider satisfies among the survivors of a proposed removal — so
// removing Provider would leave Dependent's dependency unmet.
type Breakage struct {
	Provider  *Package // the version that uniquely satisfies the requirement
	Dependent *Package // the package that depends on Provider
	Requires  Entry
}

func (b Breakage) String() string {
	return fmt.Sprintf("%s is required by %s (%q)",
		b.Provider.NEVRA(), b.Dependent.NEVRA(), b.Requires.Constraint())
}

// RemovalBreakages reports which of the removing packages cannot be dropped from
// the set `all` without breaking a package that survives the removal. For each
// surviving package whose requirement would become unsatisfied once the removing
// set is gone, it returns the removed package(s) that were satisfying it.
//
// A removed package can appear in several breakages (needed by more than one
// dependent), and a dependent can appear several times (its requirement was met
// by more than one removed provider). `all` must include the removing packages.
func RemovalBreakages(all, removing []*Package) []Breakage {
	if len(removing) == 0 {
		return nil
	}
	removeSet := make(map[string]bool, len(removing))
	for _, p := range removing {
		removeSet[p.PkgID] = true
	}
	keep := make([]*Package, 0, len(all))
	for _, p := range all {
		if !removeSet[p.PkgID] {
			keep = append(keep, p)
		}
	}
	keepIdx := buildProviderIndex(keep)

	var out []Breakage
	for _, q := range keep {
		for _, req := range q.Requires {
			if skipRequirement(req) {
				continue
			}
			// Still satisfied by a survivor? Then the removal does not break q.
			if keepIdx.satisfy(req) != nil {
				continue
			}
			// Otherwise attribute the break to every removed package that was
			// satisfying this requirement (an external dependency matches none).
			for _, p := range removing {
				if providerSatisfies(p, req) {
					out = append(out, Breakage{Provider: p, Dependent: q, Requires: req})
				}
			}
		}
	}
	return out
}
