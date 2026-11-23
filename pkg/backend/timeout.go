package backend

import (
	"context"
	"errors"
	"io"
	"time"
)

// WithTimeout returns be with every read bounded by d: a Stat or Hash must
// finish within d, a Get must start answering within d, and a download fails
// once no bytes have arrived for d. A download that keeps making progress is
// never cut off, however large. d <= 0 returns be unchanged.
//
// It is for read-only use (verify). The wrapper offers the read capabilities
// be may have (RemoteHasher, ContentHasher, FileStore) and answers as be
// would when it lacks one; writes pass through unbounded.
func WithTimeout(be Backend, d time.Duration) Backend {
	if d <= 0 {
		return be
	}
	return &timeoutBackend{Backend: be, d: d}
}

type timeoutBackend struct {
	Backend
	d time.Duration
}

func (t *timeoutBackend) Get(ctx context.Context, relpath string) (io.ReadCloser, error) {
	ctx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(t.d, cancel)
	rc, err := t.Backend.Get(ctx, relpath)
	if !timer.Stop() && err != nil && !errors.Is(err, ErrNotExist) {
		cancel()
		return nil, timedOut("GET", relpath, t.d, err)
	}
	if err != nil {
		cancel()
		return nil, err
	}
	return newIdleReader(rc, t.d, cancel, relpath), nil
}

func (t *timeoutBackend) Stat(ctx context.Context, relpath string) (*FileInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, t.d)
	defer cancel()
	fi, err := t.Backend.Stat(ctx, relpath)
	if err != nil && ctx.Err() != nil {
		return nil, timedOut("stat", relpath, t.d, err)
	}
	return fi, err
}

// Hash answers as be does, and ("", false, nil) — cannot tell without
// downloading — when be is no RemoteHasher.
func (t *timeoutBackend) Hash(ctx context.Context, relpath, algo string) (string, bool, error) {
	h, ok := t.Backend.(RemoteHasher)
	if !ok {
		return "", false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, t.d)
	defer cancel()
	sum, found, err := h.Hash(ctx, relpath, algo)
	if err != nil && ctx.Err() != nil {
		return "", false, timedOut("hash", relpath, t.d, err)
	}
	return sum, found, err
}

func (t *timeoutBackend) HashesContent() bool { return HashesContent(t.Backend) }

// LocalPath answers "" (not a local file) when be keeps no local files.
func (t *timeoutBackend) LocalPath(relpath string) string {
	p, _ := LocalPath(t.Backend, relpath)
	return p
}

func (t *timeoutBackend) Close() error { return Close(t.Backend) }

func timedOut(op, relpath string, d time.Duration, err error) error {
	return &TimeoutError{Op: op, Path: relpath, After: d, Err: err}
}

// TimeoutError is an operation WithTimeout gave up on.
type TimeoutError struct {
	Op, Path string
	After    time.Duration
	Err      error
}

func (e *TimeoutError) Error() string {
	return "backend: " + e.Op + " " + e.Path + ": no answer within " + e.After.String() + " (--timeout)"
}

func (e *TimeoutError) Unwrap() error { return e.Err }
