package view

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/httpx"
)

// Upload is a live page's file input, as LiveView's allow_upload is: files
// go to the server as they are chosen, with progress the page renders, and
// an event takes them when the form is submitted.
//
//	type Profile struct {
//	    Name   view.Assign[string]
//	    Avatar view.Upload
//	}
//
//	func (p *Profile) Mount(ctx context.Context) error {
//	    p.Avatar.Allow(view.UploadConfig{Accept: []string{"image/*"}, MaxSize: 5 << 20})
//	    return nil
//	}
//
//	func (p *Profile) Save(ctx context.Context, disk *Uploads, in ProfileForm) error {
//	    return p.Avatar.Consume(func(e view.UploadEntry, f *os.File) error {
//	        return disk.Put(ctx, "avatars/"+e.Name, f)
//	    })
//	}
//
//	templ (p *Profile) Render() {
//	    <form onsubmit={ view.Submit(p.Save) }>
//	        <input type="file" { p.Avatar.Input()... }/>
//	        for _, e := range p.Avatar.Entries() {
//	            <p>{ e.Name } { strconv.Itoa(e.Progress) }%
//	                if e.Err != "" { <span>{ e.Err }</span> }
//	                <button type="button" onclick={ view.CancelUpload(&p.Avatar, e.Ref) }>×</button></p>
//	        }
//	        <button disabled?={ p.Avatar.Busy() }>Save</button>
//	    </form>
//	}
//
// Choosing files checks them against the config (Accept, MaxEntries,
// MaxSize): each becomes an entry, refused ones with Err set. Accepted ones
// are sent at once, each its own HTTP request to the page's route (its gates
// apply, and it belongs to the connection that asked), and written to a
// temporary file; their Progress and Done follow as the bytes arrive, and
// the page re-renders. Consume hands the finished ones to fn and removes
// them; anything left is removed when the page ends.
//
// An Upload is page state like an Assign: it doesn't keep a page from
// tracking its changes, and the parts that render its entries re-render as
// they change. It works in live components too.
type Upload struct {
	mu      sync.Mutex
	cfg     UploadConfig
	entries []*uploadEntry

	name  string // the field's name: the input names it
	t     *tracker
	bit   uint64
	ver   uint64
	dirty atomic.Bool // changed by an upload's request, not yet seen by the page
	wake  chan<- bool // the page's: an upload changed
	comp  string      // the component type it belongs to, for the browser
}

// UploadConfig is what an Upload accepts.
type UploadConfig struct {
	// Accept lists what the input takes: extensions (".png") or MIME types
	// ("image/png", "image/*"); empty takes anything. It is also the input's
	// accept attribute.
	Accept []string
	// MaxEntries is how many files it holds at once (default 1).
	MaxEntries int
	// MaxSize is the largest file in bytes (default 8 MiB).
	MaxSize int64
}

// UploadEntry is a file chosen for an Upload.
type UploadEntry struct {
	Ref      string // its identity on the page
	Name     string // the file's name, as the browser gave it
	Type     string // its MIME type, as the browser gave it
	Size     int64
	Received int64
	Progress int // 0–100
	Done     bool
	Err      string // why it was refused or failed
}

type uploadEntry struct {
	UploadEntry
	token string
	path  string // the temporary file, once done
}

// Allow sets what the upload accepts. Call it in Mount.
func (u *Upload) Allow(cfg UploadConfig) {
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = 1
	}
	if cfg.MaxSize <= 0 {
		cfg.MaxSize = 8 << 20
	}
	u.mu.Lock()
	u.cfg = cfg
	u.mu.Unlock()
}

func (u *Upload) config() UploadConfig {
	u.mu.Lock()
	defer u.mu.Unlock()
	cfg := u.cfg
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = 1
	}
	if cfg.MaxSize <= 0 {
		cfg.MaxSize = 8 << 20
	}
	return cfg
}

// Entries are the files chosen, in order. In Render it notes that this part
// of the page depends on them.
func (u *Upload) Entries() []UploadEntry {
	u.read()
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]UploadEntry, len(u.entries))
	for i, e := range u.entries {
		out[i] = e.UploadEntry
	}
	return out
}

// Busy reports whether a file is still on its way.
func (u *Upload) Busy() bool {
	u.read()
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, e := range u.entries {
		if !e.Done && e.Err == "" {
			return true
		}
	}
	return false
}

func (u *Upload) read() {
	if u.t != nil && u.t.rec != nil {
		u.t.rec.read(u.bit)
	}
}

// Input is the file input's attributes: spread it on <input type="file">.
func (u *Upload) Input() templ.Attributes {
	cfg := u.config()
	a := templ.Attributes{"data-nx-upload": u.name}
	if len(cfg.Accept) > 0 {
		a["accept"] = strings.Join(cfg.Accept, ",")
	}
	if cfg.MaxEntries > 1 {
		a["multiple"] = true
	}
	if u.comp != "" {
		a["data-nx-upload-ct"] = u.comp
	}
	return a
}

// CancelUpload is an event for an on* attribute: it stops the entry ref of
// u — on its way or done — and removes it.
func CancelUpload(u *Upload, ref string) templ.ComponentScript {
	call := "__nx.live.cancelUpload(this," + jsonString(u.name) + "," + jsonString(ref)
	if u.comp != "" {
		call += "," + jsonString(u.comp)
	}
	return templ.ComponentScript{Call: htmlAttr(call + ")")}
}

// Consume hands each finished file to fn, open for reading, then removes it
// — from the entries and from disk. A file fn fails on stays, and its error
// is returned. Entries still on their way, and refused ones, stay.
func (u *Upload) Consume(fn func(e UploadEntry, f *os.File) error) error {
	u.mu.Lock()
	var done []*uploadEntry
	for _, e := range u.entries {
		if e.Done {
			done = append(done, e)
		}
	}
	u.mu.Unlock()
	var errs []error
	for _, e := range done {
		f, err := os.Open(e.path)
		if err == nil {
			err = fn(e.UploadEntry, f)
			f.Close()
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name, err))
			continue
		}
		u.remove(e.Ref)
	}
	return errors.Join(errs...)
}

// remove drops the entry ref and its file.
func (u *Upload) remove(ref string) {
	u.mu.Lock()
	for i, e := range u.entries {
		if e.Ref == ref {
			u.entries = append(u.entries[:i], u.entries[i+1:]...)
			uploads.Delete(e.token)
			if e.path != "" {
				os.Remove(e.path)
			}
			break
		}
	}
	u.mu.Unlock()
	u.touch()
}

// clear drops every entry and file: the page ended.
func (u *Upload) clear() {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, e := range u.entries {
		uploads.Delete(e.token)
		if e.path != "" {
			os.Remove(e.path)
		}
	}
	u.entries = nil
}

// touch marks the upload changed, on the page's goroutine.
func (u *Upload) touch() {
	if u.t != nil {
		u.ver = u.t.bump()
	} else {
		u.ver++
	}
}

// sync takes the changes an upload's request made, on the page's goroutine.
func (u *Upload) sync() bool {
	if !u.dirty.Swap(false) {
		return false
	}
	u.touch()
	return true
}

func (u *Upload) bindUpload(t *tracker, bit uint64, name string) {
	u.t, u.bit, u.name = t, bit, name
	if t.page != nil {
		u.comp = componentType(LiveKey(reflect.TypeOf(t.page)))
	}
}
func (u *Upload) bindAssign(t *tracker, bit uint64) { u.t, u.bit = t, bit }
func (u *Upload) version() uint64                   { return u.ver }

// uploadFile is a file the browser offers.
type uploadFile struct {
	Ref  string `json:"ref"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	Type string `json:"type"`
}

// uploadMsg is an __upload or __cancel_upload event's: the input's upload,
// and the files chosen or the entry to cancel.
type uploadMsg struct {
	Name  string       `json:"name"`
	Files []uploadFile `json:"files,omitempty"`
	Ref   string       `json:"ref,omitempty"`
}

// uploads are the entries on their way, by the token their request
// carries.
var uploads sync.Map // token → *uploadSlot

type uploadSlot struct {
	u     *Upload
	e     *uploadEntry
	owner string
}

// offer takes the files the browser chose: each accepted one gets a token
// for its request, by its ref.
func (u *Upload) offer(files []uploadFile, owner string, wake chan<- bool) map[string]string {
	cfg := u.config()
	tokens := map[string]string{}
	u.mu.Lock()
	u.wake = wake
	// A new choice drops the files refused before; a single-file input
	// replaces what it held.
	kept := u.entries[:0]
	for _, e := range u.entries {
		if e.Err == "" && cfg.MaxEntries > 1 {
			kept = append(kept, e)
			continue
		}
		uploads.Delete(e.token)
		if e.path != "" {
			os.Remove(e.path)
		}
	}
	u.entries = kept
	held := len(kept)
	for _, f := range files {
		e := &uploadEntry{UploadEntry: UploadEntry{Ref: f.Ref, Name: path.Base(filepath.ToSlash(f.Name)), Type: f.Type, Size: f.Size}}
		switch {
		case f.Ref == "" || len(f.Ref) > 64:
			continue
		case held >= cfg.MaxEntries:
			e.Err = fmt.Sprintf("too many files: at most %d", cfg.MaxEntries)
		case f.Size > cfg.MaxSize:
			e.Err = fmt.Sprintf("too large: at most %s", byteSize(cfg.MaxSize))
		case f.Size < 0:
			e.Err = "no size"
		case !accepts(cfg.Accept, f.Name, f.Type):
			e.Err = "not an accepted file type"
		default:
			e.token = newUploadToken()
			uploads.Store(e.token, &uploadSlot{u: u, e: e, owner: owner})
			tokens[f.Ref] = e.token
			held++
		}
		u.entries = append(u.entries, e)
	}
	u.mu.Unlock()
	u.touch()
	return tokens
}

// accepts reports whether a file matches the Accept list.
func accepts(accept []string, name, typ string) bool {
	if len(accept) == 0 {
		return true
	}
	ext := strings.ToLower(filepath.Ext(name))
	if typ == "" {
		typ = mime.TypeByExtension(ext)
	}
	typ = strings.ToLower(typ)
	for _, a := range accept {
		a = strings.ToLower(strings.TrimSpace(a))
		switch {
		case strings.HasPrefix(a, "."):
			if ext == a {
				return true
			}
		case strings.HasSuffix(a, "/*"):
			if strings.HasPrefix(typ, strings.TrimSuffix(a, "*")) {
				return true
			}
		case a == typ:
			return true
		}
	}
	return false
}

func byteSize(n int64) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%d KiB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

func newUploadToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// uploadHandler receives one file: the request of an accepted entry,
// streamed to a temporary file as the page's Progress follows.
func uploadHandler(c *httpx.Ctx) {
	v, ok := uploads.Load(c.Request.URL.Query().Get("t"))
	if !ok {
		c.String(http.StatusNotFound, "no such upload")
		return
	}
	slot := v.(*uploadSlot)
	owner, _ := nexus.RequestIdentity(c.Request.Context())
	if owner != slot.owner {
		c.String(http.StatusForbidden, "not this page's upload")
		return
	}
	uploads.Delete(c.Request.URL.Query().Get("t")) // one request per entry
	u, e := slot.u, slot.e
	fail := func(status int, msg string) {
		u.mu.Lock()
		e.Err = msg
		u.mu.Unlock()
		u.changed()
		c.String(status, msg)
	}
	f, err := os.CreateTemp("", "nexus-upload-*")
	if err != nil {
		fail(http.StatusInternalServerError, "the server could not store the file")
		return
	}
	keep := false
	defer func() {
		f.Close()
		if !keep {
			os.Remove(f.Name())
		}
	}()
	body := http.MaxBytesReader(c.Writer, c.Request.Body, e.Size)
	buf := make([]byte, 64<<10)
	var got int64
	last := -1
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				fail(http.StatusInternalServerError, "the server could not store the file")
				return
			}
			got += int64(n)
			pct := 100
			if e.Size > 0 {
				pct = int(got * 100 / e.Size)
			}
			u.mu.Lock()
			gone := !u.has(e)
			e.Received, e.Progress = got, pct
			u.mu.Unlock()
			if gone {
				c.String(http.StatusGone, "cancelled")
				return
			}
			if pct/5 != last/5 {
				last = pct
				u.changed()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			fail(http.StatusBadRequest, "the upload did not complete")
			return
		}
	}
	if got != e.Size {
		fail(http.StatusBadRequest, "the upload did not complete")
		return
	}
	u.mu.Lock()
	if !u.has(e) {
		u.mu.Unlock()
		c.String(http.StatusGone, "cancelled")
		return
	}
	e.Done, e.Progress, e.path = true, 100, f.Name()
	keep = true
	u.mu.Unlock()
	u.changed()
	c.Status(http.StatusNoContent)
}

// has reports whether e is still one of u's entries; u.mu is held.
func (u *Upload) has(e *uploadEntry) bool {
	for _, x := range u.entries {
		if x == e {
			return true
		}
	}
	return false
}

// changed tells the page an upload's request changed it: it re-renders.
func (u *Upload) changed() {
	u.dirty.Store(true)
	u.mu.Lock()
	wake := u.wake
	u.mu.Unlock()
	if wake != nil {
		select {
		case wake <- true:
		default:
		}
	}
}

// upload handles the browser's __upload (files chosen) and __cancel_upload
// events for the page's — or one of its components' — upload.
func (in *instance) upload(ev liveEvent, sock *Socket, owner, page string, render func(int, bool) liveReply) liveReply {
	target := in
	if ev.C != "" {
		c := in.comps[ev.C]
		if c == nil {
			return liveReply{Ref: ev.Ref, Error: fmt.Sprintf("no live component %q on the page", ev.C)}
		}
		target = c.in
	}
	if ev.Upload == nil {
		return liveReply{Ref: ev.Ref, Error: ev.Event + ": no upload"}
	}
	u := target.tr.uploads[ev.Upload.Name]
	if u == nil {
		return liveReply{Ref: ev.Ref, Error: fmt.Sprintf("no upload %q", ev.Upload.Name)}
	}
	if ev.Event == "__cancel_upload" {
		u.remove(ev.Upload.Ref)
		return render(ev.Ref, in.errs != nil)
	}
	if !slices.Contains(sock.uploads, u) {
		sock.uploads = append(sock.uploads, u)
	}
	tokens := u.offer(ev.Upload.Files, owner, sock.wake)
	reply := render(ev.Ref, in.errs != nil)
	if len(tokens) > 0 {
		reply.Uploads = map[string]string{}
		for ref, token := range tokens {
			reply.Uploads[ref] = strings.TrimSuffix(page, "/") + "/_upload?t=" + token
		}
	}
	return reply
}

// syncUploads takes what upload requests changed, on the page's goroutine:
// whether anything did.
func (in *instance) syncUploads() bool {
	changed := false
	for _, u := range in.tr.uploads {
		changed = u.sync() || changed
	}
	for _, c := range in.comps {
		for _, u := range c.in.tr.uploads {
			changed = u.sync() || changed
		}
	}
	return changed
}
