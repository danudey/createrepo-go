package repodata

import (
	"encoding/xml"
	"fmt"
	"strconv"
)

// --- wire structs for unmarshalling (namespace-agnostic by local name) ---

type xmlVersion struct {
	Epoch int    `xml:"epoch,attr"`
	Ver   string `xml:"ver,attr"`
	Rel   string `xml:"rel,attr"`
}

type xmlChecksum struct {
	Type  string `xml:"type,attr"`
	Pkgid string `xml:"pkgid,attr"`
	Value string `xml:",chardata"`
}

type xmlLocation struct {
	Href string `xml:"href,attr"`
	// Base is the xml:base attribute. The tag carries no namespace so it
	// matches the attribute by local name regardless of how the document
	// declares the xml prefix.
	Base string `xml:"base,attr"`
}

type xmlFile struct {
	Type string `xml:"type,attr"`
	Path string `xml:",chardata"`
}

type xmlEntry struct {
	Name  string `xml:"name,attr"`
	Flags string `xml:"flags,attr"`
	Epoch string `xml:"epoch,attr"`
	Ver   string `xml:"ver,attr"`
	Rel   string `xml:"rel,attr"`
	Pre   string `xml:"pre,attr"`
}

type xmlEntries struct {
	Entries []xmlEntry `xml:"entry"`
}

type xmlPrimary struct {
	XMLName  xml.Name        `xml:"metadata"`
	Packages []xmlPrimaryPkg `xml:"package"`
}

type xmlPrimaryPkg struct {
	Type     string      `xml:"type,attr"`
	Name     string      `xml:"name"`
	Arch     string      `xml:"arch"`
	Version  xmlVersion  `xml:"version"`
	Checksum xmlChecksum `xml:"checksum"`
	Summary  string      `xml:"summary"`
	Desc     string      `xml:"description"`
	Packager string      `xml:"packager"`
	URL      string      `xml:"url"`
	Time     struct {
		File  int64 `xml:"file,attr"`
		Build int64 `xml:"build,attr"`
	} `xml:"time"`
	Size struct {
		Package   int64 `xml:"package,attr"`
		Installed int64 `xml:"installed,attr"`
		Archive   int64 `xml:"archive,attr"`
	} `xml:"size"`
	Location xmlLocation `xml:"location"`
	Format   struct {
		License     string `xml:"license"`
		Vendor      string `xml:"vendor"`
		Group       string `xml:"group"`
		BuildHost   string `xml:"buildhost"`
		SourceRPM   string `xml:"sourcerpm"`
		HeaderRange struct {
			Start int `xml:"start,attr"`
			End   int `xml:"end,attr"`
		} `xml:"header-range"`
		Provides  xmlEntries `xml:"provides"`
		Requires  xmlEntries `xml:"requires"`
		Conflicts xmlEntries `xml:"conflicts"`
		Obsoletes xmlEntries `xml:"obsoletes"`
		Files     []xmlFile  `xml:"file"`
	} `xml:"format"`
}

type xmlFilelists struct {
	XMLName  xml.Name `xml:"filelists"`
	Packages []struct {
		Pkgid   string     `xml:"pkgid,attr"`
		Name    string     `xml:"name,attr"`
		Arch    string     `xml:"arch,attr"`
		Version xmlVersion `xml:"version"`
		Files   []xmlFile  `xml:"file"`
	} `xml:"package"`
}

type xmlOther struct {
	XMLName  xml.Name `xml:"otherdata"`
	Packages []struct {
		Pkgid      string `xml:"pkgid,attr"`
		Changelogs []struct {
			Author string `xml:"author,attr"`
			Date   int64  `xml:"date,attr"`
			Text   string `xml:",chardata"`
		} `xml:"changelog"`
	} `xml:"package"`
}

type xmlRepomd struct {
	XMLName  xml.Name `xml:"repomd"`
	Revision string   `xml:"revision"`
	Data     []struct {
		Type         string      `xml:"type,attr"`
		Checksum     xmlChecksum `xml:"checksum"`
		OpenChecksum xmlChecksum `xml:"open-checksum"`
		Location     xmlLocation `xml:"location"`
		Timestamp    int64       `xml:"timestamp"`
		Size         int64       `xml:"size"`
		OpenSize     int64       `xml:"open-size"`
	} `xml:"data"`
}

// --- parse entry points ---

func toEntries(in []xmlEntry) []Entry {
	if len(in) == 0 {
		return nil
	}
	out := make([]Entry, 0, len(in))
	for _, e := range in {
		ep := 0
		if e.Epoch != "" {
			ep, _ = strconv.Atoi(e.Epoch)
		}
		out = append(out, Entry{
			Name:  e.Name,
			Flags: e.Flags,
			Epoch: ep,
			Ver:   e.Ver,
			Rel:   e.Rel,
			Pre:   e.Pre == "1" || e.Pre == "true",
		})
	}
	return out
}

func toFiles(in []xmlFile) []File {
	if len(in) == 0 {
		return nil
	}
	out := make([]File, 0, len(in))
	for _, f := range in {
		out = append(out, File{Path: f.Path, Type: f.Type})
	}
	return out
}

// ParseRepomd parses a repomd.xml document.
func ParseRepomd(data []byte) (*Repomd, error) {
	var x xmlRepomd
	if err := xml.Unmarshal(data, &x); err != nil {
		return nil, fmt.Errorf("parse repomd.xml: %w", err)
	}
	r := &Repomd{Revision: x.Revision}
	for _, d := range x.Data {
		r.Data = append(r.Data, DataEntry{
			Type:         d.Type,
			ChecksumType: d.Checksum.Type,
			Checksum:     d.Checksum.Value,
			OpenChecksum: d.OpenChecksum.Value,
			Location:     d.Location.Href,
			Timestamp:    d.Timestamp,
			Size:         d.Size,
			OpenSize:     d.OpenSize,
		})
	}
	return r, nil
}

// DataEntryByType returns the data entry of the given type, or nil.
func (r *Repomd) DataEntryByType(t string) *DataEntry {
	for i := range r.Data {
		if r.Data[i].Type == t {
			return &r.Data[i]
		}
	}
	return nil
}

// ParsePrimary parses primary.xml into packages. The package file lists are
// only the "primary" subset; full file lists come from filelists.xml and are
// merged by the Index.
func ParsePrimary(data []byte) ([]*Package, error) {
	var x xmlPrimary
	if err := xml.Unmarshal(data, &x); err != nil {
		return nil, fmt.Errorf("parse primary.xml: %w", err)
	}
	out := make([]*Package, 0, len(x.Packages))
	for _, p := range x.Packages {
		pkg := &Package{
			Name:         p.Name,
			Arch:         p.Arch,
			Epoch:        p.Version.Epoch,
			Version:      p.Version.Ver,
			Release:      p.Version.Rel,
			PkgID:        p.Checksum.Value,
			ChecksumType: p.Checksum.Type,
			Summary:      p.Summary,
			Description:  p.Desc,
			Packager:     p.Packager,
			URL:          p.URL,
			FileTime:     p.Time.File,
			BuildTime:    p.Time.Build,
			SizePackage:  p.Size.Package,
			SizeInstall:  p.Size.Installed,
			SizeArchive:  p.Size.Archive,
			Location:     p.Location.Href,
			XMLBase:      p.Location.Base,
			License:      p.Format.License,
			Vendor:       p.Format.Vendor,
			Group:        p.Format.Group,
			BuildHost:    p.Format.BuildHost,
			SourceRPM:    p.Format.SourceRPM,
			HeaderStart:  p.Format.HeaderRange.Start,
			HeaderEnd:    p.Format.HeaderRange.End,
			Provides:     toEntries(p.Format.Provides.Entries),
			Requires:     toEntries(p.Format.Requires.Entries),
			Conflicts:    toEntries(p.Format.Conflicts.Entries),
			Obsoletes:    toEntries(p.Format.Obsoletes.Entries),
			Files:        toFiles(p.Format.Files),
		}
		out = append(out, pkg)
	}
	return out, nil
}

// ParseFilelists parses filelists.xml, returning a map of pkgid to files.
func ParseFilelists(data []byte) (map[string][]File, error) {
	var x xmlFilelists
	if err := xml.Unmarshal(data, &x); err != nil {
		return nil, fmt.Errorf("parse filelists.xml: %w", err)
	}
	out := make(map[string][]File, len(x.Packages))
	for _, p := range x.Packages {
		out[p.Pkgid] = toFiles(p.Files)
	}
	return out, nil
}

// ParseOther parses other.xml, returning a map of pkgid to changelogs.
func ParseOther(data []byte) (map[string][]Changelog, error) {
	var x xmlOther
	if err := xml.Unmarshal(data, &x); err != nil {
		return nil, fmt.Errorf("parse other.xml: %w", err)
	}
	out := make(map[string][]Changelog, len(x.Packages))
	for _, p := range x.Packages {
		var cl []Changelog
		for _, c := range p.Changelogs {
			cl = append(cl, Changelog{Author: c.Author, Date: c.Date, Text: c.Text})
		}
		out[p.Pkgid] = cl
	}
	return out, nil
}
