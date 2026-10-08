// Package staticfs is the file system the routers' Static serves: a
// directory only through its index.html, never as a listing of what it
// holds — the behaviour gin's Static already had.
package staticfs

import (
	"io/fs"
	"net/http"
	"path"
)

// Dir serves the files under dir.
func Dir(dir string) http.FileSystem { return noListing{http.Dir(dir)} }

type noListing struct{ fs http.FileSystem }

func (n noListing) Open(name string) (http.File, error) {
	f, err := n.fs.Open(name)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.IsDir() {
		index, err := n.fs.Open(path.Join(name, "index.html"))
		if err != nil {
			f.Close()
			return nil, fs.ErrNotExist
		}
		index.Close()
	}
	return f, nil
}
