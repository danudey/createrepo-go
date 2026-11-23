package backend

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWithTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slow-start":
			<-release
		case "/steady":
			// Slower in total than the timeout, but never idle that long.
			w.Header().Set("Content-Length", "5")
			for range 5 {
				io.WriteString(w, "x")
				w.(http.Flusher).Flush()
				time.Sleep(60 * time.Millisecond)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	defer close(release)
	be := WithTimeout(newHTTP(srv.URL), 150*time.Millisecond)
	ctx := context.Background()

	if _, err := be.Stat(ctx, "slow-start"); !errors.As(err, new(*TimeoutError)) {
		t.Errorf("Stat of a server that never answers = %v, want a TimeoutError", err)
	}
	if _, err := be.Get(ctx, "slow-start"); !errors.As(err, new(*TimeoutError)) {
		t.Errorf("Get of a server that never answers = %v, want a TimeoutError", err)
	}
	if _, err := be.Get(ctx, "missing"); !errors.Is(err, ErrNotExist) {
		t.Errorf("Get of a missing object = %v, want ErrNotExist", err)
	}
	rc, err := be.Get(ctx, "steady")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || string(data) != strings.Repeat("x", 5) {
		t.Errorf("a download that keeps making progress was cut off: %q, %v", data, err)
	}
	if h, ok, err := be.(RemoteHasher).Hash(ctx, "steady", AlgoSHA256); h != "" || ok || err != nil {
		t.Errorf("Hash on a backend that cannot hash = %q, %v, %v", h, ok, err)
	}
	if WithTimeout(be, 0) != be {
		t.Error("a zero timeout should leave the backend unwrapped")
	}
}
