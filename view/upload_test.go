package view

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
)

// avatarLive is a page with an upload, its Render listing the entries.
type avatarLive struct {
	Saved  Assign[[]string]
	Avatar Upload
}

var avatarDisk struct {
	sync.Mutex
	paths []string
}

func (p *avatarLive) Mount(ctx context.Context) error {
	p.Avatar.Allow(UploadConfig{Accept: []string{"image/*", ".txt"}, MaxEntries: 2, MaxSize: 1000})
	return nil
}

func (p *avatarLive) Save(ctx context.Context) error {
	return p.Avatar.Consume(func(e UploadEntry, f *os.File) error {
		b, err := io.ReadAll(f)
		if err != nil {
			return err
		}
		avatarDisk.Lock()
		avatarDisk.paths = append(avatarDisk.paths, f.Name())
		avatarDisk.Unlock()
		p.Saved.Update(func(s *[]string) { *s = append(*s, e.Name+"="+string(b)) })
		return nil
	})
}

func (p *avatarLive) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		r := Record(ctx, w)
		_ = r.S(w, 1, `<ul>`)
		if r.Guard(ctx, w, "entries", p) {
			for _, e := range p.Avatar.Entries() {
				io.WriteString(w, "<li>"+e.Name+":"+strconv.Itoa(e.Progress)+":"+strconv.FormatBool(e.Done)+":"+e.Err+"</li>")
			}
		}
		r.Close(w)
		_ = r.S(w, 2, `</ul><p>`)
		if r.Guard(ctx, w, "saved", p) {
			io.WriteString(w, strings.Join(p.Saved.Get(), ","))
		}
		r.Close(w)
		_ = r.S(w, 3, `</p>`)
		return nil
	})
}

func bootAvatar(t *testing.T) *httptest.Server {
	t.Helper()
	dropParked()
	t.Cleanup(dropParked)
	app, stop, err := nexus.InProcess(config.Runtime{},
		Live[*avatarLive]("/avatar").Provide(func() *avatarLive { return &avatarLive{} }),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); _ = stop(context.Background()) })
	return srv
}

func post(t *testing.T, srv *httptest.Server, url, body string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+url, strings.NewReader(body))
	req.Header.Set("Origin", srv.URL)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

// Files offered are checked against the config; accepted ones go up in a
// request each, the page follows them, Consume hands them to the event and
// removes them, and a page that ends removes what's left.
func TestUpload(t *testing.T) {
	srv := bootAvatar(t)
	c := dialLive(t, srv, "/avatar/_live")
	reply(t, c)

	offer := liveEvent{Ref: 1, Event: "__upload", Upload: &uploadMsg{Name: "Avatar", Files: []uploadFile{
		{Ref: "a", Name: "cat.png", Size: 5, Type: "image/png"},
		{Ref: "b", Name: "huge.png", Size: 5000, Type: "image/png"},
		{Ref: "c", Name: "x.exe", Size: 5, Type: "application/octet-stream"},
	}}}
	if err := c.WriteJSON(offer); err != nil {
		t.Fatal(err)
	}
	r := reply(t, c)
	if len(r.Uploads) != 1 || r.Uploads["a"] == "" {
		t.Fatalf("only the accepted file gets a URL: %+v", r.Uploads)
	}
	for _, want := range []string{"<li>cat.png:0:false:</li>", "huge.png:0:false:too large: at most 1000 bytes", "x.exe:0:false:not an accepted file type"} {
		if !strings.Contains(r.HTML, want) {
			t.Fatalf("entries lack %q:\n%s", want, r.HTML)
		}
	}

	// Too few bytes fail the entry; the URL is good for one request.
	if code := post(t, srv, r.Uploads["a"], "abc"); code != http.StatusBadRequest {
		t.Fatalf("a short upload = %d", code)
	}
	if got := reply(t, c); !strings.Contains(got.HTML, "cat.png:60:false:the upload did not complete") {
		t.Fatalf("a failed upload shows: %s", got.HTML)
	}
	if code := post(t, srv, r.Uploads["a"], "meows"); code != http.StatusNotFound {
		t.Fatalf("a used URL = %d", code)
	}

	// A good one: progress, then done.
	offer.Ref, offer.Upload.Files = 2, []uploadFile{{Ref: "d", Name: "dog.png", Size: 4, Type: "image/png"}}
	if err := c.WriteJSON(offer); err != nil {
		t.Fatal(err)
	}
	r = reply(t, c)
	if code := post(t, srv, r.Uploads["d"], "woof"); code != http.StatusNoContent {
		t.Fatalf("an upload = %d", code)
	}
	for !strings.Contains(r.HTML, "dog.png:100:true:") {
		r = reply(t, c)
	}

	if err := c.WriteJSON(liveEvent{Ref: 3, Event: "Save"}); err != nil {
		t.Fatal(err)
	}
	r = reply(t, c)
	if !strings.Contains(r.HTML, "<p>dog.png=woof</p>") || strings.Contains(r.HTML, "dog.png:") {
		t.Fatalf("Consume hands the file over and drops its entry:\n%s", r.HTML)
	}
	avatarDisk.Lock()
	consumed := avatarDisk.paths[len(avatarDisk.paths)-1]
	avatarDisk.Unlock()
	if _, err := os.Stat(consumed); !os.IsNotExist(err) {
		t.Fatalf("a consumed file is removed: %v", err)
	}

	// Cancel: the entry goes, and its URL with it.
	offer.Ref, offer.Upload.Files = 4, []uploadFile{{Ref: "e", Name: "e.txt", Size: 2, Type: "text/plain"}}
	if err := c.WriteJSON(offer); err != nil {
		t.Fatal(err)
	}
	r = reply(t, c)
	url := r.Uploads["e"]
	if err := c.WriteJSON(liveEvent{Ref: 5, Event: "__cancel_upload", Upload: &uploadMsg{Name: "Avatar", Ref: "e"}}); err != nil {
		t.Fatal(err)
	}
	if r = reply(t, c); strings.Contains(r.HTML, "e.txt") {
		t.Fatalf("a cancelled entry is dropped:\n%s", r.HTML)
	}
	if code := post(t, srv, url, "hi"); code != http.StatusNotFound {
		t.Fatalf("a cancelled entry's URL = %d", code)
	}

	// A finished file nobody consumed is removed when the page ends.
	offer.Ref, offer.Upload.Files = 6, []uploadFile{{Ref: "f", Name: "f.txt", Size: 2, Type: "text/plain"}}
	if err := c.WriteJSON(offer); err != nil {
		t.Fatal(err)
	}
	r = reply(t, c)
	if code := post(t, srv, r.Uploads["f"], "hi"); code != http.StatusNoContent {
		t.Fatalf("an upload = %d", code)
	}
	for !strings.Contains(r.HTML, "f.txt:100:true:") {
		r = reply(t, c)
	}
	left, _ := filepath.Glob(filepath.Join(os.TempDir(), "nexus-upload-*"))
	if len(left) == 0 {
		t.Fatal("the finished file is kept until consumed")
	}
	c.Close()
	for deadline := time.Now().Add(5 * time.Second); parkedCount() == 0 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	dropParked()
	for _, f := range left {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("a page that ends removes its files: %s", f)
		}
	}
}

func TestAccepts(t *testing.T) {
	for _, c := range []struct {
		accept    []string
		name, typ string
		want      bool
	}{
		{nil, "a.exe", "", true},
		{[]string{"image/*"}, "a.png", "image/png", true},
		{[]string{"image/*"}, "a.png", "", true},
		{[]string{".PDF"}, "a.pdf", "", true},
		{[]string{"image/png"}, "a.jpg", "image/jpeg", false},
		{[]string{".png"}, "a.png.exe", "", false},
	} {
		if got := accepts(c.accept, c.name, c.typ); got != c.want {
			t.Errorf("accepts(%v, %q, %q) = %v", c.accept, c.name, c.typ, got)
		}
	}
}
