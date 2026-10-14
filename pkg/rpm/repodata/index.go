package repodata

import "sort"

// Index is an in-memory view of a repository's package set, keyed by package
// checksum (pkgid). It is assembled by merging the primary, filelists and
// other documents and is the object mutated when adding or removing packages.
type Index struct {
	// Revision is the repomd <revision> of the loaded metadata, if any.
	Revision string

	byID map[string]*Package
}

// NewIndex returns an empty index.
func NewIndex() *Index {
	return &Index{byID: make(map[string]*Package)}
}

// Merge assembles an index from already-parsed metadata documents. The
// filelists and other maps may be nil.
func Merge(primary []*Package, filelists map[string][]File, other map[string][]Changelog) *Index {
	idx := NewIndex()
	for _, p := range primary {
		// Prefer the authoritative full file list from filelists.xml.
		if fl, ok := filelists[p.PkgID]; ok {
			p.Files = fl
		}
		if cl, ok := other[p.PkgID]; ok {
			p.Changelogs = cl
		}
		idx.byID[p.PkgID] = p
	}
	return idx
}

// Add inserts or replaces a package by pkgid.
func (idx *Index) Add(p *Package) {
	idx.byID[p.PkgID] = p
}

// Len returns the number of packages in the index.
func (idx *Index) Len() int { return len(idx.byID) }

// HasPkgID reports whether a package with the given checksum is present.
func (idx *Index) HasPkgID(id string) bool {
	_, ok := idx.byID[id]
	return ok
}

// ByPkgID returns the package with the given checksum, or nil.
func (idx *Index) ByPkgID(id string) *Package { return idx.byID[id] }

// ByLocation returns the package published at the given href, or nil.
func (idx *Index) ByLocation(href string) *Package {
	for _, p := range idx.byID {
		if p.Location == href {
			return p
		}
	}
	return nil
}

// RemoveByPkgID deletes a package by checksum, reporting whether it existed.
func (idx *Index) RemoveByPkgID(id string) bool {
	if _, ok := idx.byID[id]; !ok {
		return false
	}
	delete(idx.byID, id)
	return true
}

// FindByNEVRA returns all packages matching the given name, optionally
// constrained by arch (empty matches any) and EVR (empty matches any).
func (idx *Index) FindByNEVRA(name, arch, evr string) []*Package {
	var out []*Package
	for _, p := range idx.byID {
		if p.Name != name {
			continue
		}
		if arch != "" && p.Arch != arch {
			continue
		}
		if evr != "" && p.EVR() != evr {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Remove deletes every package matching the selector and returns the removed
// packages. arch and evr empty mean "any".
func (idx *Index) Remove(name, arch, evr string) []*Package {
	removed := idx.FindByNEVRA(name, arch, evr)
	for _, p := range removed {
		delete(idx.byID, p.PkgID)
	}
	return removed
}

// Packages returns all packages in a deterministic order (name, arch, then EVR
// and pkgid for stability).
func (idx *Index) Packages() []*Package {
	out := make([]*Package, 0, len(idx.byID))
	for _, p := range idx.byID {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Arch != b.Arch {
			return a.Arch < b.Arch
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		if a.Release != b.Release {
			return a.Release < b.Release
		}
		return a.PkgID < b.PkgID
	})
	return out
}
