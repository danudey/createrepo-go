// Package repofile reads a dnf/yum .repo file: the repositories it defines,
// where each is published, and how a client is told to check its signatures.
//
// It reads what the verify command needs to check a repository the way a
// client configured by the file would: baseurl, the gpgcheck and repo_gpgcheck
// switches, and the gpgkey URLs. Variables such as $releasever and $basearch
// are substituted only from values the caller supplies; a variable without a
// value is an error, never left in place or guessed.
package repofile

import (
	"bufio"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Section is one [repository] stanza.
type Section struct {
	// ID is the [section] name.
	ID string
	// Name is the name= field.
	Name string
	// BaseURLs are the baseurl= entries, in order; dnf uses the first that
	// works.
	BaseURLs []string
	// MirrorList is the mirrorlist= or metalink= field.
	MirrorList string
	// Enabled is the enabled= switch; dnf's default is on.
	Enabled bool
	// GPGCheck is gpgcheck= (or dnf 5's pkg_gpgcheck=): every package must
	// carry a signature made by one of GPGKeys. Off when absent.
	GPGCheck bool
	// RepoGPGCheck is repo_gpgcheck=: repomd.xml must carry a signature made
	// by one of GPGKeys. Off when absent.
	RepoGPGCheck bool
	// GPGKeys are the gpgkey= URLs.
	GPGKeys []string
}

// Parse reads the sections of a .repo file. Keys are case-insensitive, a
// line starting with whitespace continues the previous value (as dnf allows
// for a list of gpgkey URLs), and lists are split on whitespace and commas.
func Parse(data []byte) ([]Section, error) {
	var sections []*Section
	var cur *Section
	var lastKey string
	values := map[string]string{}

	flush := func() error {
		if cur == nil {
			return nil
		}
		for key, value := range values {
			if err := cur.set(key, value); err != nil {
				return fmt.Errorf("[%s] %s: %w", cur.ID, key, err)
			}
		}
		sections = append(sections, cur)
		return nil
	}

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; scanner.Scan(); n++ {
		raw := scanner.Text()
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if raw[0] == ' ' || raw[0] == '\t' {
			if cur == nil || lastKey == "" {
				return nil, fmt.Errorf("line %d: continuation line with no value to continue", n)
			}
			values[lastKey] += "\n" + line
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			if err := flush(); err != nil {
				return nil, err
			}
			cur = &Section{ID: strings.TrimSpace(line[1 : len(line)-1]), Enabled: true}
			values, lastKey = map[string]string{}, ""
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: %q is neither a [section] nor key=value", n, line)
		}
		if cur == nil {
			return nil, fmt.Errorf("line %d: %q comes before any [section]", n, line)
		}
		lastKey = strings.ToLower(strings.TrimSpace(key))
		values[lastKey] = strings.TrimSpace(value)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if len(sections) == 0 {
		return nil, fmt.Errorf("no [section] found; this does not look like a .repo file")
	}
	out := make([]Section, len(sections))
	for i, s := range sections {
		out[i] = *s
	}
	return out, nil
}

func (s *Section) set(key, value string) error {
	var err error
	switch key {
	case "name":
		s.Name = value
	case "baseurl":
		s.BaseURLs = splitList(value)
	case "mirrorlist", "metalink":
		s.MirrorList = value
	case "enabled":
		s.Enabled, err = parseBool(value)
	case "gpgcheck", "pkg_gpgcheck":
		s.GPGCheck, err = parseBool(value)
	case "repo_gpgcheck":
		s.RepoGPGCheck, err = parseBool(value)
	case "gpgkey":
		s.GPGKeys = splitList(value)
	}
	return err
}

func splitList(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
}

func parseBool(v string) (bool, error) {
	switch strings.ToLower(v) {
	case "1", "yes", "true", "on":
		return true, nil
	case "0", "no", "false", "off", "":
		return false, nil
	}
	return false, fmt.Errorf("%q is not a boolean (1/0, yes/no, true/false, on/off)", v)
}

// varPattern matches $name and ${name}, dnf's variable forms.
var varPattern = regexp.MustCompile(`\$(?:\{([A-Za-z0-9_]+)\}|([A-Za-z0-9_]+))`)

// Expand substitutes vars into s. Every variable s uses must have a value: a
// missing one is an error naming all that are missing, so a URL is never
// fetched with a variable left in it.
func Expand(s string, vars map[string]string) (string, error) {
	missing := map[string]bool{}
	out := varPattern.ReplaceAllStringFunc(s, func(m string) string {
		sub := varPattern.FindStringSubmatch(m)
		name := sub[1] + sub[2]
		if v, ok := vars[name]; ok {
			return v
		}
		missing[name] = true
		return m
	})
	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for n := range missing {
			names = append(names, "$"+n)
		}
		sort.Strings(names)
		return "", &MissingVarsError{Names: names, In: s}
	}
	return out, nil
}

// MissingVarsError reports variables used without a value.
type MissingVarsError struct {
	Names []string
	In    string
}

func (e *MissingVarsError) Error() string {
	if len(e.Names) == 1 {
		return fmt.Sprintf("%q uses %s, which has no value", e.In, e.Names[0])
	}
	return fmt.Sprintf("%q uses %s, which have no values", e.In, strings.Join(e.Names, ", "))
}

// Expand returns s with vars substituted into its baseurl, mirrorlist and
// gpgkey entries.
func (s Section) Expand(vars map[string]string) (Section, error) {
	out := s
	var err error
	expandAll := func(list []string) []string {
		res := make([]string, len(list))
		for i, v := range list {
			if err == nil {
				res[i], err = Expand(v, vars)
			}
		}
		return res
	}
	out.BaseURLs = expandAll(s.BaseURLs)
	out.GPGKeys = expandAll(s.GPGKeys)
	if err == nil && s.MirrorList != "" {
		out.MirrorList, err = Expand(s.MirrorList, vars)
	}
	if err != nil {
		return s, fmt.Errorf("[%s]: %w", s.ID, err)
	}
	return out, nil
}
