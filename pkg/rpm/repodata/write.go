package repodata

import (
	"bytes"
	"fmt"
	"strconv"
)

const xmlDecl = `<?xml version="1.0" encoding="UTF-8"?>` + "\n"

// escapeText escapes the minimal set of characters required inside XML text
// content, matching the rpm-md tooling (it leaves newlines and quotes intact).
func escapeText(s string) string {
	if !needsEscape(s, false) {
		return s
	}
	var b bytes.Buffer
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escapeAttr escapes a value destined for a double-quoted attribute.
func escapeAttr(s string) string {
	if !needsEscape(s, true) {
		return s
	}
	var b bytes.Buffer
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func needsEscape(s string, attr bool) bool {
	for _, r := range s {
		switch r {
		case '&', '<', '>':
			return true
		case '"':
			if attr {
				return true
			}
		}
	}
	return false
}

// WritePrimary renders the primary.xml document for the given packages.
func WritePrimary(pkgs []*Package) []byte {
	var b bytes.Buffer
	b.WriteString(xmlDecl)
	fmt.Fprintf(&b, `<metadata xmlns="%s" xmlns:rpm="%s" packages="%d">`+"\n",
		NSCommon, NSRPM, len(pkgs))
	for _, p := range pkgs {
		writePrimaryPackage(&b, p)
	}
	b.WriteString("</metadata>\n")
	return b.Bytes()
}

func writePrimaryPackage(b *bytes.Buffer, p *Package) {
	b.WriteString("<package type=\"rpm\">\n")
	fmt.Fprintf(b, "  <name>%s</name>\n", escapeText(p.Name))
	fmt.Fprintf(b, "  <arch>%s</arch>\n", escapeText(p.Arch))
	fmt.Fprintf(b, "  <version epoch=\"%d\" ver=\"%s\" rel=\"%s\"/>\n",
		p.Epoch, escapeAttr(p.Version), escapeAttr(p.Release))
	fmt.Fprintf(b, "  <checksum type=\"%s\" pkgid=\"YES\">%s</checksum>\n",
		p.ChecksumType, p.PkgID)
	fmt.Fprintf(b, "  <summary>%s</summary>\n", escapeText(p.Summary))
	fmt.Fprintf(b, "  <description>%s</description>\n", escapeText(p.Description))
	fmt.Fprintf(b, "  <packager>%s</packager>\n", escapeText(p.Packager))
	fmt.Fprintf(b, "  <url>%s</url>\n", escapeText(p.URL))
	fmt.Fprintf(b, "  <time file=\"%d\" build=\"%d\"/>\n", p.FileTime, p.BuildTime)
	fmt.Fprintf(b, "  <size package=\"%d\" installed=\"%d\" archive=\"%d\"/>\n",
		p.SizePackage, p.SizeInstall, p.SizeArchive)
	if p.XMLBase != "" {
		fmt.Fprintf(b, "  <location xml:base=\"%s\" href=\"%s\"/>\n", escapeAttr(p.XMLBase), escapeAttr(p.Location))
	} else {
		fmt.Fprintf(b, "  <location href=\"%s\"/>\n", escapeAttr(p.Location))
	}
	b.WriteString("  <format>\n")
	fmt.Fprintf(b, "    <rpm:license>%s</rpm:license>\n", escapeText(p.License))
	fmt.Fprintf(b, "    <rpm:vendor>%s</rpm:vendor>\n", escapeText(p.Vendor))
	fmt.Fprintf(b, "    <rpm:group>%s</rpm:group>\n", escapeText(p.Group))
	fmt.Fprintf(b, "    <rpm:buildhost>%s</rpm:buildhost>\n", escapeText(p.BuildHost))
	fmt.Fprintf(b, "    <rpm:sourcerpm>%s</rpm:sourcerpm>\n", escapeText(p.SourceRPM))
	fmt.Fprintf(b, "    <rpm:header-range start=\"%d\" end=\"%d\"/>\n", p.HeaderStart, p.HeaderEnd)
	writeEntries(b, "rpm:provides", p.Provides)
	writeEntries(b, "rpm:requires", p.Requires)
	writeEntries(b, "rpm:conflicts", p.Conflicts)
	writeEntries(b, "rpm:obsoletes", p.Obsoletes)
	for _, f := range p.PrimaryFiles() {
		writeFile(b, "    ", f)
	}
	b.WriteString("  </format>\n")
	b.WriteString("</package>\n")
}

func writeEntries(b *bytes.Buffer, tag string, entries []Entry) {
	if len(entries) == 0 {
		return
	}
	fmt.Fprintf(b, "    <%s>\n", tag)
	for _, e := range entries {
		b.WriteString("      <rpm:entry")
		fmt.Fprintf(b, " name=\"%s\"", escapeAttr(e.Name))
		if e.Flags != "" {
			fmt.Fprintf(b, " flags=\"%s\"", e.Flags)
			fmt.Fprintf(b, " epoch=\"%d\"", e.Epoch)
			if e.Ver != "" {
				fmt.Fprintf(b, " ver=\"%s\"", escapeAttr(e.Ver))
			}
			if e.Rel != "" {
				fmt.Fprintf(b, " rel=\"%s\"", escapeAttr(e.Rel))
			}
		}
		if e.Pre {
			b.WriteString(" pre=\"1\"")
		}
		b.WriteString("/>\n")
	}
	fmt.Fprintf(b, "    </%s>\n", tag)
}

func writeFile(b *bytes.Buffer, indent string, f File) {
	if f.Type != "" {
		fmt.Fprintf(b, "%s<file type=\"%s\">%s</file>\n", indent, f.Type, escapeText(f.Path))
	} else {
		fmt.Fprintf(b, "%s<file>%s</file>\n", indent, escapeText(f.Path))
	}
}

// WriteFilelists renders the filelists.xml document for the given packages.
func WriteFilelists(pkgs []*Package) []byte {
	var b bytes.Buffer
	b.WriteString(xmlDecl)
	fmt.Fprintf(&b, `<filelists xmlns="%s" packages="%d">`+"\n", NSFilelists, len(pkgs))
	for _, p := range pkgs {
		fmt.Fprintf(&b, "<package pkgid=\"%s\" name=\"%s\" arch=\"%s\">\n",
			p.PkgID, escapeAttr(p.Name), escapeAttr(p.Arch))
		fmt.Fprintf(&b, "  <version epoch=\"%d\" ver=\"%s\" rel=\"%s\"/>\n",
			p.Epoch, escapeAttr(p.Version), escapeAttr(p.Release))
		for _, f := range p.Files {
			writeFile(&b, "  ", f)
		}
		b.WriteString("</package>\n")
	}
	b.WriteString("</filelists>\n")
	return b.Bytes()
}

// WriteOther renders the other.xml document for the given packages.
func WriteOther(pkgs []*Package) []byte {
	var b bytes.Buffer
	b.WriteString(xmlDecl)
	fmt.Fprintf(&b, `<otherdata xmlns="%s" packages="%d">`+"\n", NSOther, len(pkgs))
	for _, p := range pkgs {
		fmt.Fprintf(&b, "<package pkgid=\"%s\" name=\"%s\" arch=\"%s\">\n",
			p.PkgID, escapeAttr(p.Name), escapeAttr(p.Arch))
		fmt.Fprintf(&b, "  <version epoch=\"%d\" ver=\"%s\" rel=\"%s\"/>\n",
			p.Epoch, escapeAttr(p.Version), escapeAttr(p.Release))
		for _, c := range p.Changelogs {
			fmt.Fprintf(&b, "  <changelog author=\"%s\" date=\"%s\">%s</changelog>\n",
				escapeAttr(c.Author), strconv.FormatInt(c.Date, 10), escapeText(c.Text))
		}
		b.WriteString("</package>\n")
	}
	b.WriteString("</otherdata>\n")
	return b.Bytes()
}

// DataEntry describes one metadata file referenced by repomd.xml.
type DataEntry struct {
	Type         string
	ChecksumType string
	Checksum     string // checksum of the compressed file
	OpenChecksum string // checksum of the uncompressed file
	Location     string // href relative to repo root
	Timestamp    int64
	Size         int64 // compressed size
	OpenSize     int64 // uncompressed size
}

// Repomd is the top-level repository index.
type Repomd struct {
	Revision string
	Data     []DataEntry
}

// WriteRepomd renders repomd.xml.
func WriteRepomd(r *Repomd) []byte {
	var b bytes.Buffer
	b.WriteString(xmlDecl)
	fmt.Fprintf(&b, `<repomd xmlns="%s" xmlns:rpm="%s">`+"\n", NSRepo, NSRPM)
	fmt.Fprintf(&b, "  <revision>%s</revision>\n", escapeText(r.Revision))
	for _, d := range r.Data {
		fmt.Fprintf(&b, "  <data type=\"%s\">\n", d.Type)
		fmt.Fprintf(&b, "    <checksum type=\"%s\">%s</checksum>\n", d.ChecksumType, d.Checksum)
		fmt.Fprintf(&b, "    <open-checksum type=\"%s\">%s</open-checksum>\n", d.ChecksumType, d.OpenChecksum)
		fmt.Fprintf(&b, "    <location href=\"%s\"/>\n", escapeAttr(d.Location))
		fmt.Fprintf(&b, "    <timestamp>%d</timestamp>\n", d.Timestamp)
		fmt.Fprintf(&b, "    <size>%d</size>\n", d.Size)
		fmt.Fprintf(&b, "    <open-size>%d</open-size>\n", d.OpenSize)
		b.WriteString("  </data>\n")
	}
	b.WriteString("</repomd>\n")
	return b.Bytes()
}
