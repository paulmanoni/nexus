package template

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"golang.org/x/net/html"
)

// Parse parses an .nlt source byte slice. The filename argument is
// purely for error attribution; it is not opened or read.
func Parse(filename string, src []byte) (*File, error) {
	p := &parser{filename: filename, src: src, lm: newLineMap(src)}
	return p.parse()
}

// ParseFile reads an .nlt file from disk and parses it.
func ParseFile(path string) (*File, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(path, src)
}

type parser struct {
	filename string
	src      []byte
	lm       *lineMap
}

// parse runs the two-phase pipeline: split top-level <template> /
// <script> / <style> blocks, then build an AST for the template
// block's contents. Script and style bodies are returned verbatim.
func (p *parser) parse() (*File, error) {
	blocks, err := p.splitSFC()
	if err != nil {
		return nil, err
	}
	if blocks.template == nil {
		return nil, &ParseError{Pos: p.posAt(0), Msg: "missing <template> block"}
	}

	file := &File{Filename: p.filename}

	tmplChildren, err := p.parseTemplate(blocks.template)
	if err != nil {
		return nil, err
	}
	file.Template = &Template{Children: tmplChildren, Pos: p.posAt(blocks.template.tagStart)}

	if blocks.script != nil {
		lang := blocks.script.lang
		if lang == "" {
			lang = "js"
		}
		file.Script = &Script{
			Scoped: blocks.script.scoped,
			Lang:   lang,
			Body:   string(p.src[blocks.script.bodyStart:blocks.script.bodyEnd]),
			Pos:    p.posAt(blocks.script.tagStart),
		}
	}
	if blocks.style != nil {
		lang := blocks.style.lang
		if lang == "" {
			lang = "css"
		}
		file.Style = &Style{
			Scoped: blocks.style.scoped,
			Lang:   lang,
			Body:   string(p.src[blocks.style.bodyStart:blocks.style.bodyEnd]),
			Pos:    p.posAt(blocks.style.tagStart),
		}
	}
	return file, nil
}

// blockInfo holds the byte ranges + meta of one SFC block.
//
// tagStart is the offset of the leading '<' of the open tag.
// bodyStart is the offset just past the '>' of the open tag.
// bodyEnd is the offset of the '<' of the matching close tag.
type blockInfo struct {
	tagStart  int
	bodyStart int
	bodyEnd   int
	lang      string
	scoped    bool
}

type sfcBlocks struct {
	template *blockInfo
	script   *blockInfo
	style    *blockInfo
}

func (b *sfcBlocks) get(kind string) *blockInfo {
	switch kind {
	case "template":
		return b.template
	case "script":
		return b.script
	case "style":
		return b.style
	}
	return nil
}

func (b *sfcBlocks) set(kind string, bi *blockInfo) {
	switch kind {
	case "template":
		b.template = bi
	case "script":
		b.script = bi
	case "style":
		b.style = bi
	}
}

// splitSFC walks the top-level tokens of the source and records the
// ranges of the three block tags. Only whitespace text and HTML
// comments are tolerated between blocks; anything else is an error.
//
// Nesting is tracked via a name stack rather than a depth counter so
// malformed inner markup (a stray </span> inside the template block)
// surfaces as a precise mismatched-close error rather than silently
// shifting the apparent block boundary.
//
// For <template> we accept nested <template> elements (used as Vue-
// style slot/group wrappers). Void HTML elements (<br>, <img>, etc.)
// don't push the stack, mirroring HTML5 semantics. <script> and
// <style> are naturally raw-text per the tokenizer, so their bodies
// arrive as a single TextToken between the open/close pair.
func (p *parser) splitSFC() (sfcBlocks, error) {
	var blocks sfcBlocks
	tok := html.NewTokenizer(bytes.NewReader(p.src))
	offset := 0
	var stack []string
	var current *blockInfo
	var currentKind string

	for {
		tt := tok.Next()
		if tt == html.ErrorToken {
			if errors.Is(tok.Err(), io.EOF) {
				break
			}
			return blocks, &ParseError{Pos: p.posAt(offset), Msg: "tokenizer: " + tok.Err().Error()}
		}
		raw := tok.Raw()
		tokenStart := offset
		offset += len(raw)

		switch tt {
		case html.TextToken:
			if len(stack) == 0 {
				if strings.TrimSpace(string(tok.Text())) != "" {
					return blocks, &ParseError{Pos: p.posAt(tokenStart), Msg: "unexpected text between top-level blocks"}
				}
			}

		case html.StartTagToken, html.SelfClosingTagToken:
			tagName := extractTagName(raw)
			tagNameLower := strings.ToLower(tagName)

			if len(stack) == 0 {
				switch tagNameLower {
				case "template", "script", "style":
					if blocks.get(tagNameLower) != nil {
						return blocks, &ParseError{Pos: p.posAt(tokenStart), Msg: fmt.Sprintf("duplicate <%s> block", tagNameLower)}
					}
					info := &blockInfo{tagStart: tokenStart, bodyStart: offset}
					if _, has := tok.TagName(); has {
						for {
							k, v, more := tok.TagAttr()
							switch string(k) {
							case "lang":
								info.lang = string(v)
							case "scoped":
								// Both <style scoped> and <script scoped>
								// honor the attribute. Other tags (template)
								// don't have a "scoped" semantic so we
								// silently ignore it there.
								if tagNameLower == "style" || tagNameLower == "script" {
									info.scoped = true
								}
							}
							if !more {
								break
							}
						}
					}
					blocks.set(tagNameLower, info)
					if tt == html.SelfClosingTagToken {
						info.bodyEnd = info.bodyStart
					} else {
						current = info
						currentKind = tagNameLower
						stack = append(stack, tagNameLower)
					}
				default:
					return blocks, &ParseError{Pos: p.posAt(tokenStart), Msg: fmt.Sprintf("unexpected top-level <%s>; only <template>, <script>, <style> are allowed", tagName)}
				}
				continue
			}

			// Inside a block. Push non-void start tags so the matching
			// close can be verified; void elements (<br>, <img>) and
			// self-closing forms don't push.
			if tt == html.StartTagToken && !voidElements[tagNameLower] {
				stack = append(stack, tagNameLower)
			}

		case html.EndTagToken:
			tagName := strings.ToLower(extractTagName(raw))
			if voidElements[tagName] {
				continue // stray </br> etc. — ignore, mirroring HTML5
			}
			if len(stack) == 0 {
				return blocks, &ParseError{Pos: p.posAt(tokenStart), Msg: fmt.Sprintf("unexpected </%s> at top level", tagName)}
			}
			top := stack[len(stack)-1]
			if top != tagName {
				return blocks, &ParseError{Pos: p.posAt(tokenStart), Msg: fmt.Sprintf("mismatched close: </%s> while <%s> is open", tagName, top)}
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 && current != nil {
				current.bodyEnd = tokenStart
				current = nil
				currentKind = ""
			}

		case html.CommentToken, html.DoctypeToken:
			// silently skipped at any depth
		}
	}

	if len(stack) != 0 {
		return blocks, &ParseError{Pos: p.posAt(current.tagStart), Msg: fmt.Sprintf("unclosed <%s> block", currentKind)}
	}
	return blocks, nil
}

// parseTemplate walks the bytes inside a <template> block and builds
// a tree of Nodes. It is the heart of the parser; see also
// classifyAttr and splitInterpolations for the parts that decode
// attribute syntax and {{ expr }} text scanning.
func (p *parser) parseTemplate(b *blockInfo) ([]Node, error) {
	body := p.src[b.bodyStart:b.bodyEnd]
	tok := html.NewTokenizer(bytes.NewReader(body))

	// We track absolute offsets into p.src (not into the body slice)
	// so positions in errors and AST nodes refer to the original file.
	offset := b.bodyStart

	var roots []Node
	var stack []*ElementNode

	addNode := func(n Node) {
		if len(stack) == 0 {
			roots = append(roots, n)
		} else {
			top := stack[len(stack)-1]
			top.Children = append(top.Children, n)
		}
	}

	for {
		tt := tok.Next()
		if tt == html.ErrorToken {
			if errors.Is(tok.Err(), io.EOF) {
				break
			}
			return nil, &ParseError{Pos: p.posAt(offset), Msg: "tokenizer: " + tok.Err().Error()}
		}
		raw := tok.Raw()
		tokenStart := offset
		offset += len(raw)

		switch tt {
		case html.TextToken:
			text := string(tok.Text())
			nodes, err := p.splitInterpolations(text, tokenStart)
			if err != nil {
				return nil, err
			}
			for _, n := range nodes {
				addNode(n)
			}

		case html.StartTagToken, html.SelfClosingTagToken:
			tagName := extractTagName(raw)
			elem := &ElementNode{
				Tag:         tagName,
				SelfClosing: tt == html.SelfClosingTagToken,
				IsComponent: isComponentTag(tagName),
				Position:    p.posAt(tokenStart),
			}
			if _, has := tok.TagName(); has {
				for {
					k, v, more := tok.TagAttr()
					attr, err := classifyAttr(string(k), string(v), p.posAt(tokenStart))
					if err != nil {
						return nil, err
					}
					elem.Attrs = append(elem.Attrs, attr)
					if !more {
						break
					}
				}
			}
			addNode(elem)
			if !elem.SelfClosing && !voidElements[strings.ToLower(tagName)] {
				stack = append(stack, elem)
			}

		case html.EndTagToken:
			tagName := extractTagName(raw)
			if voidElements[strings.ToLower(tagName)] {
				continue
			}
			if len(stack) == 0 {
				return nil, &ParseError{Pos: p.posAt(tokenStart), Msg: fmt.Sprintf("unexpected </%s> with no matching open tag", tagName)}
			}
			top := stack[len(stack)-1]
			if !strings.EqualFold(top.Tag, tagName) {
				return nil, &ParseError{Pos: p.posAt(tokenStart), Msg: fmt.Sprintf("mismatched close tag: </%s> closing <%s>", tagName, top.Tag)}
			}
			stack = stack[:len(stack)-1]

		case html.CommentToken, html.DoctypeToken:
			// comments and doctype are dropped from the AST
		}
	}

	if len(stack) > 0 {
		top := stack[len(stack)-1]
		return nil, &ParseError{Pos: top.Position, Msg: fmt.Sprintf("unclosed <%s>", top.Tag)}
	}
	return roots, nil
}

// splitInterpolations scans an HTML text segment for {{ expr }} sites
// and emits an alternating sequence of TextNodes and
// InterpolationNodes. Whitespace inside the braces is trimmed off the
// expression. Empty or unterminated interpolations are errors.
//
// Limitation: scanning is naive — a literal "}}" inside an expression
// string (e.g. {{ "}}-not-the-end" }}) will close the interpolation
// early. The Go-side interpreter / codegen will reject the resulting
// truncated expression with a clear error, which is good enough for
// v1. A proper scanner that understands string literals can be added
// later without breaking the AST shape.
func (p *parser) splitInterpolations(text string, textStartOffset int) ([]Node, error) {
	var nodes []Node
	i := 0
	for i < len(text) {
		open := strings.Index(text[i:], "{{")
		if open < 0 {
			if i < len(text) {
				nodes = append(nodes, &TextNode{
					Text:     text[i:],
					Position: p.posAt(textStartOffset + i),
				})
			}
			return nodes, nil
		}
		if open > 0 {
			nodes = append(nodes, &TextNode{
				Text:     text[i : i+open],
				Position: p.posAt(textStartOffset + i),
			})
		}
		exprStart := i + open + 2
		close := strings.Index(text[exprStart:], "}}")
		if close < 0 {
			return nil, &ParseError{
				Pos: p.posAt(textStartOffset + i + open),
				Msg: "unterminated interpolation (missing '}}')",
			}
		}
		expr := strings.TrimSpace(text[exprStart : exprStart+close])
		if expr == "" {
			return nil, &ParseError{
				Pos: p.posAt(textStartOffset + i + open),
				Msg: "empty interpolation '{{}}'",
			}
		}
		nodes = append(nodes, &InterpolationNode{
			Expr:     expr,
			Position: p.posAt(textStartOffset + i + open),
		})
		i = exprStart + close + 2
	}
	return nodes, nil
}

// classifyAttr decides which AttrKind an attribute belongs to based
// on its name prefix. The prefix syntax mirrors Vue 3:
//
//	":x"          → AttrBind, Name="x"          (shorthand for nl-bind:x)
//	"@e"          → AttrOn,   Name="e"          (shorthand for nl-on:e)
//	"#s"          → AttrDirective, Name="slot", Arg="s"
//	"nl-bind:x"   → AttrBind, Name="x"
//	"nl-on:e"     → AttrOn,   Name="e"
//	"nl-slot:s"   → AttrDirective, Name="slot", Arg="s"
//	"nl-<dir>"    → AttrDirective, Name=<dir>   (must be in knownDirectives)
//	anything else → AttrPlain
//
// Dot-suffix modifiers (@click.prevent.stop, nl-model.lazy.number)
// are split off Name into Modifiers in the order written.
func classifyAttr(name, value string, pos Position) (Attribute, error) {
	attr := Attribute{Raw: name, Value: value, Position: pos}
	switch {
	case strings.HasPrefix(name, ":"):
		attr.Kind = AttrBind
		attr.Name = name[1:]
		return attr, nil

	case strings.HasPrefix(name, "@"):
		attr.Kind = AttrOn
		evt, mods := splitModifiers(name[1:])
		attr.Name = evt
		attr.Modifiers = mods
		if h, args, ok := parseHandlerCall(value); ok {
			attr.Value = h
			attr.CallArgs = args
		}
		return attr, nil

	case strings.HasPrefix(name, "#"):
		attr.Kind = AttrDirective
		attr.Name = "slot"
		attr.Arg = name[1:]
		return attr, nil

	case strings.HasPrefix(name, "nl-bind:"):
		attr.Kind = AttrBind
		attr.Name = name[len("nl-bind:"):]
		return attr, nil

	case strings.HasPrefix(name, "nl-on:"):
		attr.Kind = AttrOn
		evt, mods := splitModifiers(name[len("nl-on:"):])
		attr.Name = evt
		attr.Modifiers = mods
		if h, args, ok := parseHandlerCall(value); ok {
			attr.Value = h
			attr.CallArgs = args
		}
		return attr, nil

	case strings.HasPrefix(name, "nl-slot:"):
		attr.Kind = AttrDirective
		attr.Name = "slot"
		attr.Arg = name[len("nl-slot:"):]
		return attr, nil

	case strings.HasPrefix(name, "nl-"):
		attr.Kind = AttrDirective
		dir, mods := splitModifiers(name[len("nl-"):])
		attr.Name = dir
		attr.Modifiers = mods
		if !knownDirectives[dir] {
			suggestion := suggestDirective(dir)
			msg := fmt.Sprintf("unknown directive 'nl-%s'", dir)
			if suggestion != "" {
				msg += " (did you mean 'nl-" + suggestion + "'?)"
			}
			return attr, &ParseError{Pos: pos, Msg: msg}
		}
		return attr, nil

	default:
		attr.Kind = AttrPlain
		attr.Name = name
		return attr, nil
	}
}

// parseHandlerCall detects the call form "name(arg, arg, ...)" on
// an event-handler value. Returns (handler, args, true) when the
// value matches; (empty, nil, false) when the value is a bare
// identifier (the v0 form). args are kept as raw expression text;
// the lowering will wire them into a slot that evaluates against
// the current scope.
//
// Arg-list splitting honors nested parens / brackets and string
// literals so handler("a,b", foo(1, 2)) splits into two args, not
// four. The implementation is a small state machine rather than
// strings.Split — comma-bearing string literals are common in
// real handler calls.
func parseHandlerCall(v string) (handler string, args []string, ok bool) {
	s := strings.TrimSpace(v)
	open := strings.IndexByte(s, '(')
	if open < 0 || !strings.HasSuffix(s, ")") {
		return "", nil, false
	}
	handler = strings.TrimSpace(s[:open])
	if handler == "" || !isHandlerIdent(handler) {
		return "", nil, false
	}
	inner := s[open+1 : len(s)-1]
	if strings.TrimSpace(inner) == "" {
		return handler, nil, true
	}
	parts := splitTopLevelComma(inner)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return handler, out, true
}

// isHandlerIdent returns true when s is a plain Go-style
// identifier suitable for a handler name. Rejects names with
// dots / brackets so accidental "Repo.Like" doesn't pass for a
// handler reference — methods on the component struct are bare
// names by convention.
func isHandlerIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			continue
		}
		if i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

// splitTopLevelComma splits s on commas at paren/bracket depth 0,
// outside string literals. Backslash escapes inside strings are
// honored. Used by parseHandlerCall to break the inner arg text
// into individual expressions.
func splitTopLevelComma(s string) []string {
	var out []string
	depth := 0
	inStr := byte(0)
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr != 0 {
			if c == '\\' {
				i++ // skip escaped char
				continue
			}
			if c == inStr {
				inStr = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			inStr = c
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, s[start:])
	return out
}

func splitModifiers(s string) (name string, mods []string) {
	parts := strings.Split(s, ".")
	if len(parts) == 1 {
		return parts[0], nil
	}
	return parts[0], parts[1:]
}

// knownDirectives is the closed set of nl-* directives recognised by
// the parser. Owning the prefix means we can reject typos at compile
// time — see suggestDirective for the "did you mean" hint.
var knownDirectives = map[string]bool{
	"if":      true,
	"else-if": true,
	"else":    true,
	"for":     true,
	"show":    true,
	"model":   true,
	"html":    true,
	"text":    true,
	"once":    true,
	"pre":     true,
	"slot":    true,
	"hook":       true,
	"stream":     true,
	"navigate":   true,
	"island":     true,
	"island-src": true,
}

// suggestDirective returns the directive name whose edit distance
// from input is smallest, but only if it's within a small threshold —
// avoids suggesting wildly unrelated names for unrecognisable input.
func suggestDirective(input string) string {
	best := ""
	bestDist := 3
	names := make([]string, 0, len(knownDirectives))
	for k := range knownDirectives {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if d := levenshtein(input, k); d < bestDist {
			best = k
			bestDist = d
		}
	}
	return best
}

func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// extractTagName pulls the raw tag name out of a token's source bytes.
// html.Tokenizer lowercases names returned by TagName(); we want the
// original case so PascalCase component refs (<UserCard>) survive.
func extractTagName(raw []byte) string {
	s := raw
	if len(s) > 0 && s[0] == '<' {
		s = s[1:]
	}
	if len(s) > 0 && s[0] == '/' {
		s = s[1:]
	}
	end := 0
	for end < len(s) && !isASCIISpace(s[end]) && s[end] != '/' && s[end] != '>' {
		end++
	}
	return string(s[:end])
}

func isASCIISpace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\f':
		return true
	}
	return false
}

// isComponentTag is true when a tag is a component reference rather
// than a plain HTML element. Convention (lifted from Vue): the tag
// name starts with an uppercase ASCII letter. The "template"
// element is excluded so authors can use <template nl-if="x"> as an
// invisible group wrapper.
func isComponentTag(tag string) bool {
	if tag == "" || strings.EqualFold(tag, "template") {
		return false
	}
	c := tag[0]
	return c >= 'A' && c <= 'Z'
}

// voidElements lists the HTML5 void elements (no end tag). Tracked
// separately from the parser stack so well-formed <br> / <img> /
// <input> don't unbalance depth counting.
var voidElements = map[string]bool{
	"area": true, "base": true, "br": true, "col": true,
	"embed": true, "hr": true, "img": true, "input": true,
	"link": true, "meta": true, "param": true, "source": true,
	"track": true, "wbr": true,
}

// lineMap turns a byte offset into a (line, col) pair. Built once
// per source; lookups are binary search.
type lineMap struct {
	starts []int // byte offset of each line's first character; starts[0] is always 0
}

func newLineMap(src []byte) *lineMap {
	starts := []int{0}
	for i, b := range src {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	return &lineMap{starts: starts}
}

func (lm *lineMap) pos(offset int) (line, col int) {
	idx := max(sort.SearchInts(lm.starts, offset+1)-1, 0)
	return idx + 1, offset - lm.starts[idx] + 1
}

func (p *parser) posAt(offset int) Position {
	line, col := p.lm.pos(offset)
	return Position{
		Filename: p.filename,
		Line:     line,
		Col:      col,
		Offset:   offset,
	}
}
