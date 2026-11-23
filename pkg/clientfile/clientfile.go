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
	"time"
)

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
		return io.ReadAll(resp.Body)
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
	if !isHTTPURL(configFile) || isHTTPURL(target) {
		return nil
	}
	return fmt.Errorf("%s names %q; a configuration file fetched over HTTP may only name http(s) repositories", configFile, target)
}

// isHTTPURL reports whether s is an http or https URL.
func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https")
}
