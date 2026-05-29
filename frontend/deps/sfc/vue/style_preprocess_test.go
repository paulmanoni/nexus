package vue

import (
	"os/exec"
	"strings"
	"testing"
)

func sassOnPath() bool {
	_, err := exec.LookPath("sass")
	return err == nil
}

func TestPreprocessSFCStyles_PlainCSSUntouched(t *testing.T) {
	src := "<template><p>x</p></template>\n<style scoped>.a{color:red}</style>"
	out, err := preprocessSFCStyles(src, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != src {
		t.Errorf("plain-CSS SFC should be unchanged.\n got: %q\nwant: %q", out, src)
	}
}

func TestPreprocessSFCStyles_NonSassLangLeftForGuard(t *testing.T) {
	// less/stylus aren't handled here — left intact so the adapter
	// guard rejects them with a clear message.
	src := `<style lang="less">.a{.b{color:red}}</style>`
	out, err := preprocessSFCStyles(src, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != src {
		t.Errorf("non-sass lang should be left untouched, got: %q", out)
	}
}

func TestPreprocessSFCStyles_ScssCompiledAndLangStripped(t *testing.T) {
	if !sassOnPath() {
		t.Skip("system `sass` not installed")
	}
	src := `<style scoped lang="scss">.row { &:hover { color: #d3e3fd; } }</style>`
	out, err := preprocessSFCStyles(src, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "&:hover") {
		t.Errorf("scss nesting not expanded; got: %q", out)
	}
	if !strings.Contains(out, ".row:hover") {
		t.Errorf("expected expanded `.row:hover`; got: %q", out)
	}
	if strings.Contains(out, `lang="scss"`) {
		t.Errorf("lang attribute should be stripped; got: %q", out)
	}
	if !strings.Contains(out, "scoped") {
		t.Errorf("scoped attribute should be preserved; got: %q", out)
	}
	if !strings.Contains(out, "#d3e3fd") {
		t.Errorf("declaration lost in compile; got: %q", out)
	}
}