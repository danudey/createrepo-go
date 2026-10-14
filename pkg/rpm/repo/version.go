package repo

import (
	"time"

	"github.com/danudey/createrepo-go/pkg/rpm/repodata"
)

func timeNow() int64 { return time.Now().Unix() }

// rpmVerCompare compares two rpm version (or release) strings using rpm's
// segment-based algorithm. It delegates to repodata.CompareVersions, the shared
// implementation. It returns -1, 0 or 1.
func rpmVerCompare(a, b string) int { return repodata.CompareVersions(a, b) }
