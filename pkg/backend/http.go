package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// errReadOnly is returned by write operations on read-only backends.
var errReadOnly = errors.New("backend: location is read-only")

// httpBackend is a read-only Backend over HTTP(S). It supports inspecting and
// reading a published repository but cannot be used as a publish target.
type httpBackend struct {
	base   string // base URL with a trailing slash
	client *http.Client
}

// httpHeaderTimeout bounds the wait for a server to start answering a request,
// and httpIdleTimeout the wait for the next bytes of a response body. Neither
// limits how long a large download may take while data keeps arriving.
var (
	httpHeaderTimeout = 60 * time.Second
	httpIdleTimeout   = 2 * time.Minute
)

func newHTTP(location string) *httpBackend {
	if !strings.HasSuffix(location, "/") {
		location += "/"
	}
	// http.DefaultClient has no timeouts at all, so a server that accepts the
	// connection and then goes quiet would hang the run indefinitely.
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = httpHeaderTimeout
	return &httpBackend{base: location, client: &http.Client{Transport: t}}
}

func (h *httpBackend) url(relpath string) string {
	return h.base + strings.TrimLeft(relpath, "/")
}

func (h *httpBackend) Get(ctx context.Context, relpath string) (io.ReadCloser, error) {
	// The request gets its own cancel so a body that stops arriving can be
	// abandoned without cancelling the caller's context.
	reqCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, h.url(relpath), nil)
	if err != nil {
		cancel()
		return nil, err
	}
	resp, err := h.client.Do(req) //nolint:bodyclose // returned inside the idleReader, whose Close closes it
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		drainClose(resp.Body)
		cancel()
		return nil, ErrNotExist
	}
	if resp.StatusCode != http.StatusOK {
		drainClose(resp.Body)
		cancel()
		return nil, fmt.Errorf("backend: GET %s: %s", h.url(relpath), resp.Status)
	}
	return newIdleReader(resp.Body, httpIdleTimeout, cancel, h.url(relpath)), nil
}

// errStalled is returned by an idleReader whose source went quiet.
var errStalled = errors.New("no data received")

// idleReader cancels its request when no bytes arrive for the timeout, which
// fails the Read waiting on them.
type idleReader struct {
	body    io.ReadCloser
	timer   *time.Timer
	timeout time.Duration
	cancel  context.CancelFunc
	name    string
	stalled atomic.Bool
}

func newIdleReader(body io.ReadCloser, timeout time.Duration, cancel context.CancelFunc, name string) *idleReader {
	r := &idleReader{body: body, timeout: timeout, cancel: cancel, name: name}
	r.timer = time.AfterFunc(timeout, func() {
		r.stalled.Store(true)
		cancel()
	})
	return r
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if n > 0 {
		r.timer.Reset(r.timeout)
	}
	if err != nil && r.stalled.Load() {
		err = fmt.Errorf("backend: GET %s: %w for %s", r.name, errStalled, r.timeout)
	}
	return n, err
}

func (r *idleReader) Close() error {
	r.timer.Stop()
	err := r.body.Close()
	r.cancel()
	return err
}

func (h *httpBackend) Stat(ctx context.Context, relpath string) (*FileInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, h.url(relpath), nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer drainClose(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotExist
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("backend: HEAD %s: %s", h.url(relpath), resp.Status)
	}
	return &FileInfo{Size: resp.ContentLength}, nil
}

func (h *httpBackend) Put(context.Context, string, io.Reader, int64) error { return errReadOnly }
func (h *httpBackend) Delete(context.Context, string) error                { return errReadOnly }
func (h *httpBackend) String() string                                      { return h.base }

// ReadOnly implements backend.ReadOnlyBackend: HTTP(S) can serve a repository
// but never publish one.
func (h *httpBackend) ReadOnly() bool { return true }
