// Package fsutil writes files crash-safely (temp file + fsync + rename).
package fsutil

import (
	"os"
	"path/filepath"
)

// WriteTemp writes data to a new 0640 temp file in dir and returns its name.
func WriteTemp(dir, pattern string, data []byte) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	if err := os.Chmod(name, 0o640); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

// AtomicWrite writes data to path via temp file + fsync + rename.
func AtomicWrite(path string, data []byte) error {
	tmp, err := WriteTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp", data)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	SyncDir(filepath.Dir(path))
	return nil
}

// SyncDir flushes directory entries (renames) to disk; errors are ignored.
func SyncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}
