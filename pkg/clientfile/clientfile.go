// Package clientfile handles the client configuration files verify accepts in
// place of a repository: a dnf .repo file or an apt sources file, read from
// disk or fetched by URL. A file fetched by URL was written by someone else,
// so what it may point at is limited (see CheckTargetScheme).
package clientfile

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxConfigFile bounds a configuration file fetched over HTTP.
const maxConfigFile = 1 << 20

// FetchOverHTTP returns a function that downloads a client configuration file
// (a .repo file, an apt sources file) over HTTP(S).
func FetchOverHTTP(ctx context.Context, timeout time.Duration, userAgent string) func(string) ([]byte, error) {
	client := &http.Client{Timeout: timeout}
	return func(rawURL string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("HTTP %s", resp.Status)
		}
		// A configuration file is a few kilobytes; a server that sends more
		// is not serving one.
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxConfigFile+1))
		if err != nil {
			return nil, err
		}
		if len(data) > maxConfigFile {
			return nil, fmt.Errorf("%s is over %d bytes, too large for a configuration file", rawURL, maxConfigFile)
		}
		return data, nil
	}
}

// CheckTargetScheme refuses a repository location taken from a configuration
// file that was itself downloaded. The file's author chose that location, not
// the person running check, so it may only name another http(s) URL: a file://,
// s3:// or sftp:// location would have check read local files or spend the
// operator's cloud and SSH credentials on whatever the file pointed at. A
// configuration file read from local disk is the operator's own and may name
// anything.
func CheckTargetScheme(configFile, target string) error {
	if !IsHTTPURL(configFile) || IsHTTPURL(target) {
		return nil
	}
	return fmt.Errorf("%s names %q; a configuration file fetched over HTTP may only name http(s) repositories", configFile, target)
}

// IsHTTPURL reports whether s is an http or https URL as the backend reads
// it. Whatever decides to fetch a configuration file over HTTP must use it
// too, so that a file fetched is always one CheckTargetScheme treats as
// fetched: a literal, lowercase "http://" or "https://" prefix (backend.Open
// splits on "://" without folding case) followed by a host. url.Parse alone
// would accept "HTTP://..." or "http:relative/path", which the backend
// opens as something other than a web server.
func IsHTTPURL(s string) bool {
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && u.Opaque == "" && u.Host != ""
}
