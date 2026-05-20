package frontend

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/paulmanoni/nexus"
)

// Write writes the rendered files to outDir, preserving byte-equal
// files' mtime so a no-op generation pass doesn't churn the IDE's
// file watcher.
//
// Phase-1 strategy is per-file write-if-changed, not "render to temp
// dir + atomic rename". A full atomic swap would either delete files
// the user added inside Generate/ (their gitignored notes, scratch
// .ts files) or require a stricter contract about ownership that
// phase 1 hasn't committed to. The conservative choice: only touch
// the files we render; leave everything else alone.
//
// Returns the number of files written (mtime-preserving no-ops not
// counted) and the number of files skipped because their bytes were
// already current. stdout receives one line per file change — pass
// io.Discard to silence.
func Write(outDir string, files []nexus.GeneratedFile, stdout io.Writer) (changed, unchanged int, err error) {
	if stdout == nil {
		stdout = io.Discard
	}
	if err := os.MkdirAll(outDir, 0750); err != nil {
		return 0, 0, fmt.Errorf("frontend.Write: mkdir %s: %w", outDir, err)
	}
	for _, f := range files {
		if strings.HasPrefix(f.Path, "/") || strings.Contains(f.Path, "..") {
			return changed, unchanged, fmt.Errorf("frontend.Write: rejected unsafe path %q", f.Path)
		}
		dst := filepath.Join(outDir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0750); err != nil {
			return changed, unchanged, fmt.Errorf("frontend.Write: mkdir parent of %s: %w", dst, err)
		}
		same, err := byteEqual(dst, f.Body)
		if err != nil {
			return changed, unchanged, err
		}
		if same {
			unchanged++
			fmt.Fprintf(stdout, "unchanged %s\n", dst)
			continue
		}
		if err := os.WriteFile(dst, f.Body, 0600); err != nil {
			return changed, unchanged, fmt.Errorf("frontend.Write: write %s: %w", dst, err)
		}
		changed++
		fmt.Fprintf(stdout, "wrote     %s\n", dst)
	}
	return changed, unchanged, nil
}

// byteEqual compares an on-disk file to a candidate body. Returns
// (true, nil) when the file exists and matches; (false, nil) when it
// doesn't exist or differs. Errors only on unexpected I/O failures
// (permission denied, hardware faults). Uses a hash compare on large
// files so we don't read both copies fully into memory for the common
// no-op case.
func byteEqual(path string, body []byte) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if info.Size() != int64(len(body)) {
		return false, nil
	}
	if info.Size() < 64*1024 {
		cur, err := os.ReadFile(path)
		if err != nil {
			return false, err
		}
		return string(cur) == string(body), nil
	}
	// Large file — hash compare avoids two full copies in memory.
	h := sha256.New()
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	want := sha256.Sum256(body)
	got := h.Sum(nil)
	if len(got) != len(want) {
		return false, nil
	}
	for i := range got {
		if got[i] != want[i] {
			return false, nil
		}
	}
	return true, nil
}
