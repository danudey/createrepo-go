// Package configfile reads and writes the small JSON file each tool keeps at a
// repository's root to record the settings it was published with. The fields
// belong to each tool; this package only moves the file.
package configfile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/danudey/createrepo-go/pkg/backend"
)

// Load reads the file at path into a new T. A repository with no such file
// yields (nil, nil), so a caller can treat it as "no recorded defaults".
func Load[T any](ctx context.Context, be backend.Backend, path string) (*T, error) {
	rc, err := be.Get(ctx, path)
	if errors.Is(err, backend.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &v, nil
}

// Save writes v to path as pretty-printed JSON.
func Save(ctx context.Context, be backend.Backend, path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return be.Put(ctx, path, bytes.NewReader(data), int64(len(data)))
}
