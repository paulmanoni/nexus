package view_test

import (
	"context"
	"io"
	"os"
	"strconv"
	"testing"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/view"
	"github.com/paulmanoni/nexus/v2/view/viewtest"
)

type photos struct {
	Saved view.Assign[string]
	Photo view.Upload
}

func (p *photos) Mount(ctx context.Context) error {
	p.Photo.Allow(view.UploadConfig{Accept: []string{"image/*"}, MaxSize: 1 << 20})
	return nil
}

func (p *photos) Save(ctx context.Context) error {
	return p.Photo.Consume(func(e view.UploadEntry, f *os.File) error {
		b, err := io.ReadAll(f)
		p.Saved.Set(e.Name + ":" + string(b))
		return err
	})
}

func (p *photos) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if err := view.Script().Render(ctx, w); err != nil {
			return err
		}
		io.WriteString(w, `<input id="photo" type="file"`)
		if err := templ.RenderAttributes(ctx, w, p.Photo.Input()); err != nil {
			return err
		}
		io.WriteString(w, `>`)
		for _, e := range p.Photo.Entries() {
			io.WriteString(w, `<p class="entry">`+e.Name+` `+strconv.Itoa(e.Progress)+`% `+strconv.FormatBool(e.Done)+`</p>`)
		}
		io.WriteString(w, `<button id="save" onclick="`+view.Send(p.Save).Call+`">save</button><p id="saved">`+p.Saved.Get()+`</p>`)
		return nil
	})
}

// In a real browser: choosing a file sends it, the page shows it done, and
// the event takes it.
func TestUploadInBrowser(t *testing.T) {
	app, stop, err := nexus.InProcess(config.Runtime{},
		view.Live[*photos]("/photos").Provide(func() *photos { return &photos{} }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	p := viewtest.Browser(t, app, "/photos")
	p.Eval(`(() => {
		const dt = new DataTransfer();
		dt.items.add(new File(["woof"], "dog.png", { type: "image/png" }));
		const input = document.getElementById("photo");
		input.files = dt.files;
		input.dispatchEvent(new Event("change", { bubbles: true }));
	})()`)
	p.Expect(".entry").Text("dog.png 100% true")
	p.Click("#save")
	p.Expect("#saved").Text("dog.png:woof")
}
