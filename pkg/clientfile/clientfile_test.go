package clientfile

import "testing"

func TestCheckTargetScheme(t *testing.T) {
	for _, tc := range []struct {
		config, target string
		ok             bool
	}{
		{"https://example.com/x.repo", "https://mirror.example.com/el9/", true},
		{"https://example.com/x.repo", "http://mirror.example.com/el9/", true},
		{"https://example.com/x.repo", "file:///etc", false},
		{"https://example.com/x.repo", "/etc", false},
		{"http://example.com/x.list", "s3://victim-bucket/repo", false},
		{"http://example.com/x.list", "sftp://internal-host/srv", false},
		{"/etc/yum.repos.d/x.repo", "file:///srv/mirror", true},
		{"/etc/apt/sources.list", "/srv/mirror", true},
	} {
		if err := CheckTargetScheme(tc.config, tc.target); (err == nil) != tc.ok {
			t.Errorf("CheckTargetScheme(%q, %q) = %v, want ok=%v", tc.config, tc.target, err, tc.ok)
		}
	}
}
