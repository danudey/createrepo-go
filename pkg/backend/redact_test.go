package backend

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"sftp://deploy:s3cret@repo.example.com/srv/repo": "sftp://deploy:xxxxx@repo.example.com/srv/repo",
		"https://mirror:s3cret@example.com/el9/":         "https://mirror:xxxxx@example.com/el9/",
		"sftp://deploy@repo.example.com/srv/repo":        "sftp://deploy@repo.example.com/srv/repo",
		"s3://bucket/prefix":                             "s3://bucket/prefix",
		"/srv/repo":                                      "/srv/repo",
		"relative/repo":                                  "relative/repo",
	} {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHTTPStringHidesThePassword(t *testing.T) {
	if s := newHTTP("https://mirror:s3cret@example.com/el9").String(); strings.Contains(s, "s3cret") {
		t.Errorf("String() = %q, which holds the password", s)
	}
}
