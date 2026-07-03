package storage

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// LocalDisk stores objects as files under Root on the OS filesystem. It
// has zero dependencies and is the sensible default for development and
// single-node deployments.
type LocalDisk struct {
	// Root is the base directory. Created on first write if absent.
	Root string

	// PublicBaseURL, when set, is the URL prefix under which Root is
	// served (e.g. "https://cdn.example.com/uploads" or "/media"). URL
	// and SignedURL join it with the key. Empty → URL returns an error.
	PublicBaseURL string
}

// NewLocalDisk returns a LocalDisk rooted at dir.
func NewLocalDisk(dir string) *LocalDisk { return &LocalDisk{Root: dir} }

// resolve maps a storage key to an absolute filesystem path, REJECTING
// any key that attempts to escape Root. Rejection (rather than silently
// cleaning ".." away) is deliberate: rewriting "../a" to "a" would let two
// distinct keys collide on the same file. This is the security boundary
// for the local backend.
func (l *LocalDisk) resolve(key string) (string, error) {
	k := strings.TrimPrefix(key, "/")
	if k == "" || k == "." {
		return "", errors.New("storage: empty key")
	}
	// Explicitly refuse any ".." segment — the traversal vector.
	for _, seg := range strings.Split(k, "/") {
		if seg == ".." {
			return "", errors.New("storage: key must not contain \"..\"")
		}
	}
	full := filepath.Join(l.Root, filepath.FromSlash(k))
	// Defense in depth: confirm the result is still within Root even if
	// filepath.Join's cleaning behaved unexpectedly on this OS.
	rootAbs, err := filepath.Abs(l.Root)
	if err != nil {
		return "", err
	}
	fullAbs, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	if fullAbs != rootAbs && !strings.HasPrefix(fullAbs, rootAbs+string(os.PathSeparator)) {
		return "", errors.New("storage: key escapes root")
	}
	return full, nil
}

func (l *LocalDisk) Put(ctx context.Context, key string, r io.Reader, opts ...PutOption) error {
	_ = applyPutOptions(opts) // content-type/public are S3 concerns; local encodes nothing extra
	full, err := l.resolve(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	// Write to a temp file in the same dir then rename, so a reader never
	// sees a half-written object (atomic on POSIX).
	tmp, err := os.CreateTemp(filepath.Dir(full), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, full)
}

func (l *LocalDisk) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	full, err := l.resolve(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotExist
		}
		return nil, err
	}
	return f, nil
}

func (l *LocalDisk) Exists(ctx context.Context, key string) (bool, error) {
	full, err := l.resolve(key)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(full)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func (l *LocalDisk) Delete(ctx context.Context, key string) error {
	full, err := l.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrNotExist
		}
		return err
	}
	return nil
}

func (l *LocalDisk) Stat(ctx context.Context, key string) (Object, error) {
	full, err := l.resolve(key)
	if err != nil {
		return Object{}, err
	}
	fi, err := os.Stat(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Object{}, ErrNotExist
		}
		return Object{}, err
	}
	return Object{
		Key:          strings.TrimPrefix(key, "/"),
		Size:         fi.Size(),
		ContentType:  mime.TypeByExtension(filepath.Ext(full)),
		LastModified: fi.ModTime(),
	}, nil
}

func (l *LocalDisk) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	err := filepath.WalkDir(l.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(l.Root, p)
		if rerr != nil {
			return rerr
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, strings.TrimPrefix(prefix, "/")) {
			return nil
		}
		fi, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		out = append(out, Object{
			Key:          key,
			Size:         fi.Size(),
			ContentType:  mime.TypeByExtension(filepath.Ext(p)),
			LastModified: fi.ModTime(),
		})
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return out, nil
}

func (l *LocalDisk) URL(key string) (string, error) {
	if l.PublicBaseURL == "" {
		return "", errors.New("storage: local disk has no PublicBaseURL configured")
	}
	return strings.TrimRight(l.PublicBaseURL, "/") + "/" + strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(key, "/")), "/"), nil
}

// SignedURL for the local backend has no cryptographic signing — it
// returns the public URL when one is configured. Time-limited access to a
// local file is the caller's own concern (e.g. an authenticated handler).
func (l *LocalDisk) SignedURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	return l.URL(key)
}

// Driver / DiskDetails / Ping satisfy the described interface for the
// dashboard resource.
func (l *LocalDisk) Driver() string { return "local" }
func (l *LocalDisk) DiskDetails() map[string]any {
	return map[string]any{"driver": "local", "root": l.Root}
}

// Ping confirms Root exists and is a directory — a cheap, network-free
// health probe.
func (l *LocalDisk) Ping(ctx context.Context) error {
	fi, err := os.Stat(l.Root)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return errors.New("storage: Root is not a directory")
	}
	return nil
}
