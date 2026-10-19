package backend

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
)

// A server that stops sending must fail the download instead of hanging it:
// http.DefaultClient has no timeouts, so before this a stalled mirror held a
// copy or check forever.
func TestHTTPGivesUpOnAStalledServer(t *testing.T) {
	defer func(h, i time.Duration) { httpHeaderTimeout, httpIdleTimeout = h, i }(httpHeaderTimeout, httpIdleTimeout)
	httpHeaderTimeout, httpIdleTimeout = 300*time.Millisecond, 300*time.Millisecond

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/no-headers" {
			<-release
			return
		}
		w.Header().Set("Content-Length", "1000")
		io.WriteString(w, strings.Repeat("x", 100))
		w.(http.Flusher).Flush()
		<-release // the rest never comes
	}))
	defer srv.Close()
	defer close(release)
	be := newHTTP(srv.URL)
	ctx := context.Background()

	start := time.Now()
	if _, err := be.Get(ctx, "no-headers"); err == nil {
		t.Error("a server that never answered was accepted")
	}

	rc, err := be.Get(ctx, "stalls")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	_, err = io.ReadAll(rc)
	if !errors.Is(err, errStalled) {
		t.Errorf("reading a stalled body = %v, want errStalled", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("giving up took %s", d)
	}
}

// stallConn stops delivering what the client sends once stall is closed, as
// a server that hangs does.
type stallConn struct {
	net.Conn
	stall chan struct{}
	stop  chan struct{}
}

func (c *stallConn) Read(p []byte) (int, error) {
	select {
	case <-c.stall:
		<-c.stop
		return 0, io.EOF
	default:
	}
	return c.Conn.Read(p)
}

// Cancelling the context must interrupt an SFTP operation waiting on a server
// that has stopped answering. The sftp package takes no context, so before
// this Ctrl-C or a timeout could not stop a stalled transfer.
func TestSFTPCancelInterruptsAStalledOperation(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	sc := &stallConn{Conn: serverConn, stall: make(chan struct{}), stop: make(chan struct{})}
	defer close(sc.stop)
	srv, err := sftp.NewServer(sc)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	client, err := sftp.NewClientPipe(clientConn, clientConn)
	if err != nil {
		t.Fatal(err)
	}
	be := &sftpBackend{client: client, transport: clientConn, root: t.TempDir(), label: "sftp://test"}

	close(sc.stall)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := be.Stat(ctx, "repodata/repomd.xml")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Stat on a stalled server = %v, want the context's error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stat did not return after its context expired")
	}
}
