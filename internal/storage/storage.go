// Package storage stores source videos and proxies by key.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Storage is the minimal blob interface the pipeline needs. Keys are
// slash-separated, e.g. "sources/<job>.mp4" or "proxies/<job>/<fence>.mp4".
type Storage interface {
	// Put writes r under key and returns the number of bytes written.
	// A failed Put leaves nothing behind under key.
	Put(ctx context.Context, key string, r io.Reader) (int64, error)
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
	// DeletePrefix removes every key under prefix + "/". Removing nothing is
	// not an error.
	DeletePrefix(ctx context.Context, prefix string) error
}

// Local stores blobs as files under Root.
type Local struct {
	Root string
}

func NewLocal(root string) (*Local, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create storage root: %w", err)
	}
	return &Local{Root: root}, nil
}

func (l *Local) path(key string) (string, error) {
	if !filepath.IsLocal(key) {
		return "", fmt.Errorf("invalid storage key %q", key)
	}
	return filepath.Join(l.Root, filepath.FromSlash(key)), nil
}

// Put writes to a temp file in the same directory, fsyncs it, then renames it
// into place. A crash mid-write leaves only a stray temp file, never a
// truncated blob under the real key.
func (l *Local) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	dst, err := l.path(key)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename

	n, err := io.Copy(tmp, r)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return n, err
	}
	return n, os.Rename(tmp.Name(), dst)
}

// LocalFile returns a path on local disk holding key's content, for tools
// like ffmpeg that need a file. Local storage returns its own file without
// copying; other storage is downloaded into dir.
func LocalFile(ctx context.Context, s Storage, key, dir string) (string, error) {
	if l, ok := s.(*Local); ok {
		return l.path(key)
	}
	r, err := s.Open(ctx, key)
	if err != nil {
		return "", err
	}
	defer r.Close()
	dst := filepath.Join(dir, filepath.Base(filepath.FromSlash(key)))
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return dst, err
}

func (l *Local) Open(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

func (l *Local) Delete(_ context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (l *Local) DeletePrefix(_ context.Context, prefix string) error {
	p, err := l.path(prefix)
	if err != nil {
		return err
	}
	if p == filepath.Clean(l.Root) {
		return fmt.Errorf("refusing to delete the storage root")
	}
	return os.RemoveAll(p)
}
