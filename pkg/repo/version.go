package repo

import (
	"bytes"
	"io"
	"time"

	"github.com/danudey/createrepo-go/pkg/repodata"
)

func timeNow() int64 { return time.Now().Unix() }

func readAll(r io.Reader) ([]byte, error) { return io.ReadAll(r) }

// bytesReader wraps data in a seekable reader so object-store backends can sign
// and checksum it in place.
func bytesReader(data []byte) io.Reader { return bytes.NewReader(data) }

// rpmVerCompare compares two rpm version (or release) strings using rpm's
// segment-based algorithm. It delegates to repodata.CompareVersions, the shared
// implementation. It returns -1, 0 or 1.
func rpmVerCompare(a, b string) int { return repodata.CompareVersions(a, b) }
