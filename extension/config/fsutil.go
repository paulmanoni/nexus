package config

import (
	"os"
	"path/filepath"
)

// atomicWrite writes data to path via a same-directory tempfile
// + rename. Prevents a half-written file from existing during
// the seal-transition or cache update; either the rename
// succeeds and the new file is canonical, or it fails and the
// previous state is intact.
//
// Shared by client.go (sealed cache write) and any future
// caller that needs the same "write completely or not at all"
// semantics for a config artifact.
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
