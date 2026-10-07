package viewtest_test

import (
	"context"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/nexustest"
	"github.com/paulmanoni/nexus/v2/view"
	"github.com/paulmanoni/nexus/v2/view/ui"
	"github.com/paulmanoni/nexus/v2/view/viewtest"
)

// PetForm is the whole declaration, Django-form style: fields and widgets
// from tags, choices and clean() as methods.
type PetForm struct {
	view.Form
	Name    string `form:"name" validate:"required"`
	Species string `form:"species" label:"Kind of pet" validate:"required"`
	Breed   string `form:"breed"`
	Age     int    `form:"age" span:"6"`
	Vaccine bool   `form:"vaccinated"`
	Chip    string `form:"chip"` // not rendered: a keystroke must not zero it
}

var breeds = map[string][]string{"dog": {"collie", "boxer"}, "cat": {"siamese", "persian", "manx"}}

// Init is the form's defaults — Django's initial — for a page that never
// Loads; a Load (editing something real) replaces them.
func (f *PetForm) Init(ctx context.Context) {
	if f.Species == "" {
		f.Species = "dog"
	}
	if f.Chip == "" {
		f.Chip = "c-00"
	}
}

// formSaved is what PetForm.Save collected (the Quick page below has no
// submit method of its own: the form's Save takes it).
var formSaved struct {
	mu   sync.Mutex
	pets []PetForm
}

func (f *PetForm) Save(ctx context.Context) error {
	formSaved.mu.Lock()
	defer formSaved.mu.Unlock()
	p := *f
	p.Form = view.Form{}
	formSaved.pets = append(formSaved.pets, p)
	return nil
}

// BreedChoices depend on another field — a dependent select with no wiring.
func (f PetForm) BreedChoices(ctx context.Context) []view.Choice {
	var out []view.Choice
	for _, b := range breeds[f.Species] {
		out = append(out, view.Choice{Value: b})
	}
	return out
}

// Validate is the form's clean().
func (f PetForm) Validate(ctx context.Context) error {
	if f.Name == f.Species {
		return nexus.Invalid().Field("name", "a species is not a name")
	}
	return nil
}

type petLog struct {
	mu    sync.Mutex
	saved []PetForm
}

// Pets embeds its one form: the page is the form.
type Pets struct {
	view.LiveView
	PetForm
}

func (p *Pets) Mount(ctx context.Context) error {
	p.Load(PetForm{Name: "Rex", Species: "dog", Breed: "collie", Age: 3, Chip: "c-77"})
	return nil
}

// Recalc runs on every change: a new species drops the breed.
func (p *Pets) Recalc(ctx context.Context, f PetForm) error {
	if p.Changed("species") {
		p.Update(func() { p.Breed = "" })
	}
	return nil
}

func (p *Pets) Save(ctx context.Context, log *petLog, f PetForm) error {
	f.Form = view.Form{} // only the data matters to the log
	log.mu.Lock()
	log.saved = append(log.saved, f)
	log.mu.Unlock()
	return nil
}

func (p *Pets) Reload(ctx context.Context) error {
	return p.Mount(ctx)
}

func (p *Pets) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		io.WriteString(w, `<!DOCTYPE html><html><head><title>Pets</title>`)
		if err := view.Script().Render(ctx, w); err != nil {
			return err
		}
		io.WriteString(w, `</head><body><p id="echo">`+templ.EscapeString(p.Name)+`</p><p id="chip">`+templ.EscapeString(p.Chip)+`</p>`)
		io.WriteString(w, `<button id="reload" onclick="`+view.Send(p.Reload).Call+`">reload</button>`)
		io.WriteString(w, `<a id="away" href="/other" data-nx-nav>away</a>`)
		fields := templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
			for _, c := range []templ.Component{
				ui.Field("name"), ui.Field("species"), ui.Field("breed"),
				ui.Field("age"), ui.Field("vaccinated"), ui.Submit("Save"),
			} {
				if err := c.Render(ctx, w); err != nil {
					return err
				}
			}
			return nil
		})
		form := ui.Form(p.PetForm, p.Save, p.Recalc, view.ConfirmLeave("Leave the pet?"))
		if err := form.Render(templ.WithChildren(ctx, fields), w); err != nil {
			return err
		}
		_, err := io.WriteString(w, `</body></html>`)
		return err
	})
}

func otherPage() templ.Component {
	return templ.Raw(`<!DOCTYPE html><html><head><title>Other</title></head><body><h1 id="other">other</h1></body></html>`)
}

func TestForm(t *testing.T) {
	log := &petLog{}
	app := nexustest.New(t, config.Runtime{},
		nexus.Supply(log),
		view.Live[*Pets]("/pets").Provide(func() *Pets { return &Pets{} }),
		view.Page("GET", "/other", otherPage),
	)
	p := viewtest.Mount[*Pets](t, app)

	// The struct declared everything: labels, types, the breed select.
	p.Expect("name").Value("Rex")
	p.Expect("#petForm-age").Attr("type", "number").Value("3")
	p.Expect("label[for=petForm-species]").ContainsText("Kind of pet")
	p.Expect("#petForm-breed option").Count(3) // 2 breeds + the empty choice
	p.Expect("breed").Value("collie")

	// A changed species runs Recalc: the breed clears, its choices follow.
	p.Fill("species", "cat").Wait()
	p.Expect("#petForm-breed option").Count(4)
	p.Expect("breed").Value("")

	// A keystroke must not zero the unrendered field (merge, not replace).
	p.Fill("name", "Tom").Wait()
	p.Expect("#echo").Text("Tom")
	p.Expect("#chip").Text("c-77")

	// Errors show once a changed field is left — not while typing the first.
	p.Fill("name", "").Fill("age", "4").Wait()
	p.Expect("#petForm-name-error").Visible().ContainsText("required")
	p.Expect("name").Attr("aria-invalid", "true")

	// An invalid submit shows every error and never reaches Save — the
	// form's own Validate (clean) included.
	p.Fill("name", "cat").Click("button[type=submit]").Wait()
	p.Expect("#petForm-name-error").ContainsText("a species is not a name")
	if len(log.saved) != 0 {
		t.Fatalf("saved %+v", log.saved)
	}

	// A valid one saves the merged values — Chip included — and resets the
	// form to what it was loaded with.
	p.Fill("name", "Misha").Select("breed", "manx").Check("vaccinated").Click("button[type=submit]").Wait()
	if len(log.saved) != 1 || !reflect.DeepEqual(log.saved[0], PetForm{Name: "Misha", Species: "cat", Breed: "manx", Age: 4, Vaccine: true, Chip: "c-77"}) {
		t.Fatalf("saved %+v", log.saved)
	}
	p.Expect("name").Value("Rex")
	p.Expect("#petForm-name-error").Hidden()
	p.Expect("#petForm").NoAttr("data-nx-dirty")

	// An unticked checkbox reaches the server as false.
	p.Check("vaccinated").Uncheck("vaccinated").Fill("name", "Bo").Click("button[type=submit]").Wait()
	if len(log.saved) != 2 || log.saved[1].Vaccine {
		t.Fatalf("unticked: %+v", log.saved)
	}

	// Load replaces what was typed, even a value the server never heard.
	p.Fill("name", "draft").Blur()
	p.Eval(`document.getElementById("petForm-name").value = "ghost"`)
	p.Click("#reload").Wait()
	p.Expect("name").Value("Rex")

	if c := p.Console(); len(c) > 0 {
		t.Errorf("console: %v", c)
	}
}

func TestFormLeaveAndBusy(t *testing.T) {
	log := &petLog{}
	app := nexustest.New(t, config.Runtime{},
		nexus.Supply(log),
		view.Live[*Pets]("/pets").Provide(func() *Pets { return &Pets{} }),
		view.Page("GET", "/other", otherPage),
	)
	p := viewtest.Mount[*Pets](t, app)
	p.Eval(`window.asked = 0; window.confirm = function () { window.asked++; return false; }`)

	// Clean: leaving doesn't ask. Dirty: it does, and stays on no.
	p.Expect("#petForm").NoAttr("data-nx-dirty")
	p.Fill("name", "Tim").Wait()
	p.Expect("#petForm").Attr("data-nx-dirty", "")
	p.Click("#away")
	if p.Eval(`String(window.asked)`) != "1" || !strings.HasSuffix(p.URL(), "/pets") {
		t.Fatalf("asked %s, at %s", p.Eval(`String(window.asked)`), p.URL())
	}

	// A second submit while one is on its way is dropped.
	p.Eval(`var f = document.getElementById("petForm");
		f.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
		f.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));`)
	p.Wait()
	if len(log.saved) != 1 {
		t.Fatalf("a double submit saved %d times", len(log.saved))
	}

	// Saved: clean again, leaving goes through.
	p.Click("#away")
	p.Expect("#other").Text("other")
}

// Quick uses the whole default: a named form field, no children at all.
type Quick struct {
	view.LiveView
	Ask PetForm
}

func (q *Quick) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		io.WriteString(w, `<!DOCTYPE html><html><head><title>Quick</title>`)
		if err := view.Script().Render(ctx, w); err != nil {
			return err
		}
		io.WriteString(w, `</head><body>`)
		if err := ui.Form(q.Ask).Render(ctx, w); err != nil {
			return err
		}
		_, err := io.WriteString(w, `</body></html>`)
		return err
	})
}

func TestFormAllFieldsDefault(t *testing.T) {
	app := nexustest.New(t, config.Runtime{},
		view.Live[*Quick]("/quick").Provide(func() *Quick { return &Quick{} }),
	)
	p := viewtest.Mount[*Quick](t, app)

	// Childless ui.Form: every field renders from the struct, plus Save —
	// and Init seeded the defaults, no Mount anywhere.
	p.Expect("#ask").Exists()
	p.Expect("species").Value("dog")
	p.Expect("chip").Value("c-00")
	p.Expect("label[for=ask-species]").ContainsText("Kind of pet")
	p.Expect("#ask-breed option").Count(3) // the form's own choices + the empty one
	p.Expect("#ask button[type=submit]").ContainsText("Save")
	if _, ok := p.Attr("#ask-chip", "type"); !ok {
		t.Fatal("chip not rendered by AllFields")
	}
	// span: age sits in half a row.
	if html := p.HTML(); !strings.Contains(html, `grid-column: span 6 / span 6`) {
		t.Error("age span not honored")
	}
	// Not live: typing sends nothing; no error appears before a submit.
	p.Fill("name", "x").Fill("name", "")
	p.Expect("#ask-name-error").Hidden()

	// The form's own Save method takes the submit — gated like any other:
	// an invalid form (clean(): a name equal to the species) never saves.
	p.Fill("name", "dog").Click("#ask button[type=submit]").Wait()
	p.Expect("#ask-name-error").ContainsText("a species is not a name")
	formSaved.mu.Lock()
	n := len(formSaved.pets)
	formSaved.mu.Unlock()
	if n != 0 {
		t.Fatalf("an invalid form reached Save (%d)", n)
	}
	p.Fill("name", "Puk").Click("#ask button[type=submit]").Wait()
	formSaved.mu.Lock()
	defer formSaved.mu.Unlock()
	if len(formSaved.pets) != 1 || !reflect.DeepEqual(formSaved.pets[0], PetForm{Name: "Puk", Species: "dog", Chip: "c-00"}) {
		t.Fatalf("form Save got %+v", formSaved.pets)
	}
	p.Expect("name").Value("") // reset to the loaded (Init) values
}

type hostForm struct {
	view.Form
	Name string `form:"name" validate:"required"`
	Code string `form:"code" validate:"required"`
}

// Host renders its form with markup of its own, reading view.Errors as
// hand-written fields do.
type Host struct {
	view.LiveView
	Edit hostForm
}

func (h *Host) Mount(ctx context.Context) error {
	h.Edit.Load(hostForm{Name: "Kibo", Code: "K1"})
	return nil
}

func (h *Host) Save(ctx context.Context, f hostForm) error { return nil }

func (h *Host) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		io.WriteString(w, `<!DOCTYPE html><html><head><title>Host</title>`)
		if err := view.Script().Render(ctx, w); err != nil {
			return err
		}
		io.WriteString(w, `</head><body>`)
		fields := templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
			errs := view.Errors(ctx)
			_, err := io.WriteString(w, `<input name="name" value="`+h.Edit.Name+`"><p id="name-error">`+errs.Field("name")+`</p>`+
				`<input name="code" value="`+h.Edit.Code+`"><p id="code-error">`+errs.Field("code")+`</p>`)
			return err
		})
		if err := view.RenderForm(&h.Edit, h.Save, view.LiveValidation).Render(templ.WithChildren(ctx, fields), w); err != nil {
			return err
		}
		_, err := io.WriteString(w, `</body></html>`)
		return err
	})
}

// A live form's errors reach view.Errors as they show on the kit's fields:
// a field's once it is left, and not one the user hasn't touched.
func TestFormLiveErrorsInOwnMarkup(t *testing.T) {
	app := nexustest.New(t, config.Runtime{}, view.Live[*Host]("/host"))
	p := viewtest.Mount[*Host](t, app)
	p.Fill("name", "").Blur()
	p.Expect("#name-error").Text("required")
	p.Fill("code", "")
	p.Expect("#code-error").Text("")
	p.Blur()
	p.Expect("#code-error").Text("required")
	p.Fill("name", "Meru").Blur()
	p.Expect("#name-error").Text("")
}
