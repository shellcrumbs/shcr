// Package sync moves encrypted event batches between machines through shared
// object storage.
//
// The design rests on one invariant: a device only ever writes under its own
// prefix. No two devices touch the same object, so there is no locking, no
// conflict resolution and no last-writer-wins to reason about. Merging is
// insert-by-event-id, which the store already treats as idempotent.
package sync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrNotExist means the key is absent. Backends must map their own 404 to this.
var ErrNotExist = errors.New("object does not exist")

// Storage is the whole surface a backend has to implement. GCS, S3 and R2 are
// all a thin wrapper over these four calls.
type Storage interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Put(ctx context.Context, key string, data []byte) error

	// List returns keys under prefix that sort strictly after `after`, in order.
	// An empty `after` lists everything.
	//
	// The bound is the point. Batch keys are named so lexical order is
	// chronological order, and a reader already knows where it stopped, so a
	// listing has no business enumerating years of history to discard all but
	// the last few entries. Both S3 (StartAfter) and GCS (startOffset) apply
	// this server-side, which keeps a steady-state pull proportional to what is
	// new rather than to everything ever written.
	List(ctx context.Context, prefix, after string) ([]string, error)

	// Children returns the immediate child prefixes under prefix — the
	// "folders". Discovering peers by listing every object in the bucket and
	// picking device ids out of the paths costs the whole bucket on every poll;
	// object stores answer this directly with a delimited list.
	Children(ctx context.Context, prefix string) ([]string, error)
}

// FileStorage keeps the "bucket" in a directory. It is the backend used by the
// tests, and a genuinely useful one in its own right: point it at a synced
// folder, a NAS mount or an rclone mount and it works, still end-to-end
// encrypted, with no cloud credentials involved.
type FileStorage struct {
	Root string
}

func NewFileStorage(root string) (*FileStorage, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &FileStorage{Root: root}, nil
}

// path maps an object key to a file inside Root. A key that is not already
// canonical is rejected rather than cleaned: normalising it would silently
// store the object somewhere other than the caller asked for, and every key
// this package generates is canonical by construction.
func (f *FileStorage) path(key string) (string, error) {
	if key == "" || strings.HasPrefix(key, "/") {
		return "", fmt.Errorf("invalid object key %q", key)
	}
	// A listing prefix may end in a slash; nothing else about the key may be
	// non-canonical.
	trimmed := strings.TrimSuffix(key, "/")
	if trimmed == "" {
		return "", fmt.Errorf("invalid object key %q", key)
	}
	if clean := filepath.Clean("/" + trimmed); clean != "/"+trimmed {
		return "", fmt.Errorf("refusing non-canonical object key %q", key)
	}
	return filepath.Join(f.Root, filepath.FromSlash(trimmed)), nil
}

func (f *FileStorage) Get(_ context.Context, key string) ([]byte, error) {
	p, err := f.path(key)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s: %w", key, ErrNotExist)
		}
		return nil, err
	}
	return b, nil
}

// Children lists the immediate subdirectories of prefix.
func (f *FileStorage) Children(_ context.Context, prefix string) ([]string, error) {
	dir, err := f.path(prefix)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".tmp-") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// Put writes through a temporary file and renames, so a reader never sees a
// half-written batch — the same atomicity a real object store gives for free.
func (f *FileStorage) Put(_ context.Context, key string, data []byte) error {
	p, err := f.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// No chmod: CreateTemp already makes the file 0600.
	return os.Rename(tmp.Name(), p)
}

func (f *FileStorage) List(_ context.Context, prefix, after string) ([]string, error) {
	base, err := f.path(prefix)
	if err != nil {
		return nil, err
	}
	// A prefix need not be a directory boundary, so walk the parent and filter.
	dir := base
	if !strings.HasSuffix(prefix, "/") {
		dir = filepath.Dir(base)
	}
	var out []string
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".tmp-") {
			return nil
		}
		rel, err := filepath.Rel(f.Root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		// A local directory has to be walked either way; the filter is what a
		// real backend applies server-side.
		if strings.HasPrefix(key, prefix) && key > after {
			out = append(out, key)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}
