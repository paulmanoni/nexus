// Command instrument records live render trees in templ output that wasn't
// compiled by nexus (templ generate): it rewrites each *_templ.go under the
// given directories with viewgen.Instrument, as nexus's view compiler does.
//
//	go run github.com/paulmanoni/nexus/v2/view/viewgen/cmd/instrument ./ui
package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/paulmanoni/nexus/v2/view/viewgen"
)

func main() {
	dirs := os.Args[1:]
	if len(dirs) == 0 {
		dirs = []string{"."}
	}
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, "_templ.go") {
				return err
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out, err := viewgen.Instrument(src)
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			if string(out) != string(src) {
				fmt.Println("instrumented", p)
				return os.WriteFile(p, out, 0o644)
			}
			return nil
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
