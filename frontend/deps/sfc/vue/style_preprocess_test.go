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

func TestPreprocessSource_CompilesInlineScss(t *testing.T) {
	if !sassOnPath() {
		t.Skip("system `sass` not installed")
	}
	// The exported entry the unbundled dev server uses — it must run the
	// same scss preprocessing the bundler's SFC plugin does, so a .vue with
	// inline <style lang="scss"> compiles instead of failing with
	// "requires a preprocessor".
	src := `<template><div/></template>` + "\n" +
		`<style scoped lang="scss">$c: #d3e3fd; .row { &:hover { color: $c; } }</style>`
	out, err := PreprocessSource(src, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, `lang="scss"`) || strings.Contains(out, "&:hover") || strings.Contains(out, "$c") {
		t.Errorf("scss not compiled by PreprocessSource; got: %q", out)
	}
	if !strings.Contains(out, ".row:hover") || !strings.Contains(out, "#d3e3fd") {
		t.Errorf("expected compiled css; got: %q", out)
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
