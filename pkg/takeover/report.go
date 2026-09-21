package takeover

import (
	"sort"

	"github.com/danudey/createrepo-go/pkg/repoconfig"
)

// Severity ranks a finding by what it costs the repository's clients.
type Severity string

// The severities a finding can carry. Blocking means republishing as configured
// would break something a client relies on (metadata it would lose, a signature
// that would no longer verify, a transfer that cannot succeed); advisory means
// the repository changes in a way worth knowing about but that clients survive;
// info records something observed that changes nothing.
const (
	Blocking Severity = "blocking"
	Advisory Severity = "advisory"
	Info     Severity = "info"
)

// rank orders severities most serious first.
func (s Severity) rank() int {
	switch s {
	case Blocking:
		return 0
	case Advisory:
		return 1
	case Info:
		return 2
	default:
		return 3
	}
}

// Finding is one thing the analysis established about a prospective takeover.
type Finding struct {
	Severity Severity `json:"severity"`
	// Code is a stable identifier for the finding, for scripts that act on
	// specific ones.
	Code string `json:"code"`
	// Summary is a single line stating what is the case.
	Summary string `json:"summary"`
	// Detail explains the consequence for the repository's clients.
	Detail string `json:"detail,omitempty"`
	// Fix names the flag or command that resolves it, where one does.
	Fix string `json:"fix,omitempty"`
	// Items lists the subjects the finding covers (metadata types, package
	// names, file paths), truncated to a readable number.
	Items []string `json:"items,omitempty"`
}

// MetadataEntry is one document repomd.xml references, and what a republish
// would do with it.
type MetadataEntry struct {
	Type         string `json:"type"`
	Location     string `json:"location"`
	Compression  string `json:"compression"`
	ChecksumType string `json:"checksum_type"`
	Size         int64  `json:"size"`
	// Fate is "regenerated" for the documents this tool writes itself, and
	// "deleted" for the ones it does not generate (a republish removes the
	// files the superseded repomd.xml referenced).
	Fate string `json:"fate"`
}

// Published describes the repository exactly as it stands today.
type Published struct {
	Revision        string          `json:"revision,omitempty"`
	Packages        int             `json:"packages"`
	PackageBytes    int64           `json:"package_bytes"`
	TotalBytes      int64           `json:"total_bytes"`
	TotalFiles      int             `json:"total_files"`
	Listed          bool            `json:"listed"`
	Metadata        []MetadataEntry `json:"metadata"`
	Compressions    []string        `json:"metadata_compression,omitempty"`
	ChecksumTypes   []string        `json:"package_checksum_types,omitempty"`
	PackageDirs     []string        `json:"package_directories,omitempty"`
	XMLBases        []string        `json:"xml_bases,omitempty"`
	SignedMetadata  bool            `json:"signed_metadata"`
	MetadataSigners []string        `json:"metadata_signers,omitempty"`
	PackageSigners  []string        `json:"package_signers,omitempty"`
	SampledPackages int             `json:"sampled_packages,omitempty"`
	StrayRPMs       []string        `json:"stray_rpms,omitempty"`
	StrayRPMBytes   int64           `json:"stray_rpm_bytes,omitempty"`
	StrayMetadata   []string        `json:"stray_metadata,omitempty"`
	OtherFiles      []string        `json:"other_files,omitempty"`
}

// Proposed records the settings the analysis was run with — what a republish
// would use.
type Proposed struct {
	Target          string `json:"target,omitempty"`
	Compression     string `json:"compression"`
	ChecksumType    string `json:"checksum_type"`
	LocationPrefix  string `json:"location_prefix,omitempty"`
	Relocate        bool   `json:"relocate"`
	FromPackages    bool   `json:"from_packages"`
	ChangelogLimit  int    `json:"changelog_limit"`
	PruneOlder      bool   `json:"prune_older"`
	SignMetadata    bool   `json:"sign_metadata"`
	SignPackages    bool   `json:"sign_packages"`
	SignatureFormat string `json:"signature_format,omitempty"`
	GPGKeyID        string `json:"gpg_key_id,omitempty"`
}

// Changes is what the republish would actually do, as computed from a real
// (dry) commit plan.
type Changes struct {
	PackagesBefore     int      `json:"packages_before"`
	PackagesAfter      int      `json:"packages_after"`
	Pruned             int      `json:"pruned,omitempty"`
	Relocated          int      `json:"relocated,omitempty"`
	Corrected          int      `json:"metadata_corrected,omitempty"`
	Uploads            int      `json:"uploads"`
	UploadBytes        int64    `json:"upload_bytes"`
	ServerSideMoves    int      `json:"server_side_moves"`
	MetadataWritten    []string `json:"metadata_written,omitempty"`
	DeletedFiles       []string `json:"deleted_files,omitempty"`
	DeletedBytes       int64    `json:"deleted_bytes,omitempty"`
	SignMetadata       bool     `json:"metadata_will_be_signed"`
	DependencyProblems []string `json:"dependency_problems,omitempty"`
}

// FieldDiff counts the packages whose regenerated metadata differs from the
// published metadata in one field.
type FieldDiff struct {
	Field    string   `json:"field"`
	Packages int      `json:"packages"`
	Examples []string `json:"examples,omitempty"`
}

// Report is the whole analysis: what is published, what would be published
// instead, and every finding that follows from the difference.
type Report struct {
	Location string `json:"location"`
	// Managed reports whether the repository already carries a
	// createrepo-go.json, i.e. whether this tool has published it before.
	Managed   bool              `json:"already_managed"`
	Published Published         `json:"published"`
	Proposed  Proposed          `json:"proposed"`
	Changes   Changes           `json:"changes"`
	Diff      []FieldDiff       `json:"metadata_diff,omitempty"`
	Findings  []Finding         `json:"findings"`
	Config    repoconfig.Config `json:"proposed_config"`
}

// Count returns the number of findings of the given severity.
func (r *Report) Count(s Severity) int {
	n := 0
	for _, f := range r.Findings {
		if f.Severity == s {
			n++
		}
	}
	return n
}

// OK reports whether nothing blocking was found, i.e. whether republishing with
// these settings is safe to go ahead with.
func (r *Report) OK() bool { return r.Count(Blocking) == 0 }

// add records a finding. Findings accumulate in discovery order and are sorted
// by severity when the analysis finishes.
func (r *Report) add(f Finding) { r.Findings = append(r.Findings, f) }

// sortFindings puts the most serious findings first, keeping discovery order
// within a severity so related findings stay together.
func (r *Report) sortFindings() {
	sort.SliceStable(r.Findings, func(i, j int) bool {
		return r.Findings[i].Severity.rank() < r.Findings[j].Severity.rank()
	})
}
