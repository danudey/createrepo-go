package rpm

import "testing"

func i18nPackage(locales []string, summaries, descriptions []string) *Package {
	tags := map[int]*Tag{
		1004: {ID: 1004, Type: TagTypeI18NString, Value: summaries},
		1005: {ID: 1005, Type: TagTypeI18NString, Value: descriptions},
	}
	if locales != nil {
		tags[100] = &Tag{ID: 100, Type: TagTypeStringArray, Value: locales}
	}
	return &Package{Header: Header{Tags: tags}}
}

// A package with translated summaries holds one value per locale. rpm and
// createrepo_c report the C one; joining them all, as before, put every
// translation into the repository's <summary>.
func TestSummaryIsTheCLocaleValue(t *testing.T) {
	p := i18nPackage([]string{"C", "de", "fr"},
		[]string{"English summary", "Deutsche Zusammenfassung", "Résumé français"},
		[]string{"English description", "Deutsch", "Français"})
	if got := p.Summary(); got != "English summary" {
		t.Errorf("Summary() = %q, want the C locale's", got)
	}
	if got := p.Description(); got != "English description" {
		t.Errorf("Description() = %q, want the C locale's", got)
	}

	// C need not come first in the locale table.
	p = i18nPackage([]string{"de", "C"}, []string{"Deutsch", "English"}, []string{"d", "e"})
	if got := p.Summary(); got != "English" {
		t.Errorf("Summary() with C second = %q, want %q", got, "English")
	}

	// No locale table: the first value is the only sensible choice.
	p = i18nPackage(nil, []string{"Only summary"}, []string{"Only description"})
	if got := p.Summary(); got != "Only summary" {
		t.Errorf("Summary() without a locale table = %q", got)
	}
	if got := (&Package{}).Summary(); got != "" {
		t.Errorf("Summary() of an empty header = %q, want empty", got)
	}
}
