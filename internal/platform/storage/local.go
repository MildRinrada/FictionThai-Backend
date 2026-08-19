package storage

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Local stores objects as files under a base directory - the development
// backend (docs/14 §56). The directory lives OUTSIDE any web-served source
// tree (docs/11 §29); nothing serves it directly, the API streams objects
// through the metadata check.
type Local struct {
	base string
}

// NewLocal builds the filesystem backend, creating the base directory.
func NewLocal(base string) (*Local, error) {
	abs, err := filepath.Abs(base)
	if err != nil {
		return nil, fmt.Errorf("resolve storage path: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("create storage directory: %w", err)
	}
	return &Local{base: abs}, nil
}

// resolve maps a key to a file path, refusing anything that would escape the
// base directory. Keys are generated upstream, so this firing at all would
// mean a bug - but path traversal must be impossible here regardless of who
// the caller is (docs/11 §28).
func (l *Local) resolve(key string) (string, error) {
	if key == "" || strings.Contains(key, "..") || strings.ContainsAny(key, "\\:") {
		return "", ErrNotFound
	}
	path := filepath.Join(l.base, filepath.FromSlash(key))
	if !strings.HasPrefix(path, l.base+string(filepath.Separator)) {
		return "", ErrNotFound
	}
	return path, nil
}

// Put writes the object via a temp file + rename, so a crash mid-write can
// never leave a half-written object under the real key.
func (l *Local) Put(key string, contents io.Reader) error {
	path, err := l.resolve(key)
	if err != nil {
		return fmt.Errorf("store object %q: %w", key, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("store object %q: %w", key, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".upload-*")
	if err != nil {
		return fmt.Errorf("store object %q: %w", key, err)
	}
	tmpName := tmp.Name()

	if _, err := io.Copy(tmp, contents); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("store object %q: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("store object %q: %w", key, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("store object %q: %w", key, err)
	}
	return nil
}

// Open returns the object's bytes.
func (l *Local) Open(key string) (io.ReadCloser, error) {
	path, err := l.resolve(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open object %q: %w", key, err)
	}
	return f, nil
}

// Delete removes the object; an absent key is a success (idempotent).
func (l *Local) Delete(key string) error {
	path, err := l.resolve(key)
	if err != nil {
		return nil
	}
	err = os.Remove(path)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("delete object %q: %w", key, err)
}
