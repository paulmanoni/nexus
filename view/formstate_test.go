package view

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/v2"
)

type signup struct {
	Form
	FirstName string    `form:"firstName" validate:"required" span:"6"`
	Email     string    `form:"email" json:"mail" validate:"required,email" help:"Work address."`
	Password  string    `form:"password" input:"password"`
	Age       int       `form:"age" label:"Your age"`
	Joined    time.Time `form:"joined"`
	Tags      []string  `form:"tags"`
	News      bool      `form:"news"`
	Plan      string    `form:"plan" placeholder:"Pick one"`
	RoleIDs   []uint    `form:"roleIds"`
	Hidden    string
}

func (s signup) PlanChoices(ctx context.Context) []Choice {
	return []Choice{{Value: "free"}, {Value: "pro", Label: "Pro"}}
}

func (s signup) Validate(ctx context.Context) error {
	if s.FirstName == "root" {
		return nexus.Invalid().Field("firstName", "that name is taken")
	}
	return nil
}

func bound(t *testing.T) *signup {
	t.Helper()
	f := &signup{}
	f.bindOuter(reflect.ValueOf(f), "Edit", "")
	return f
}

func TestFormDeclaration(t *testing.T) {
	f := bound(t)
	f.Load(signup{FirstName: "Ada", Age: 36, Joined: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Tags: []string{"a", "b"}, News: true})

	name := f.F("firstName")
	if name.ID != "edit-firstName" || name.Label != "First Name" || name.Kind != "input" || name.Type != "text" ||
		!name.Required || name.Value != "Ada" || name.Span != 6 {
		t.Errorf("firstName: %+v", name)
	}
	if !reflect.DeepEqual(f.F("FirstName"), name) {
		t.Error("a field by its Go name")
	}
	for field, typ := range map[string]string{"email": "email", "password": "password", "age": "number", "joined": "date"} {
		if got := f.F(field); got.Kind != "input" || got.Type != typ {
			t.Errorf("%s: %s/%s, want input/%s", field, got.Kind, got.Type, typ)
		}
	}
	if e := f.F("email"); e.Help != "Work address." {
		t.Errorf("email help: %+v", e)
	}
	if a := f.F("age"); a.Label != "Your age" || a.Value != "36" {
		t.Errorf("age: %+v", a)
	}
	if j := f.F("joined"); j.Value != "2026-01-02" {
		t.Errorf("joined: %q", j.Value)
	}
	if g := f.F("tags"); g.Kind != "multiselect" || !reflect.DeepEqual(g.Values, []string{"a", "b"}) {
		t.Errorf("tags: %+v", g)
	}
	if n := f.F("news"); n.Kind != "checkbox" || !n.Checked {
		t.Errorf("news: %+v", n)
	}
	if p := f.F("plan"); !p.HasChoices || p.Placeholder != "Pick one" {
		t.Errorf("plan: %+v", p)
	}
	if cs := f.ChoicesFor(context.Background(), "plan"); len(cs) != 2 || cs[1].Label != "Pro" {
		t.Errorf("choices: %+v", cs)
	}
	if l := f.F("roleIds").Label; l != "Role IDs" {
		t.Errorf("roleIds label %q", l)
	}
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "firstName, email, password") {
			t.Errorf("unknown field: %v", r)
		}
	}()
	f.F("nope")
}

func TestFormChangeMergesOverValues(t *testing.T) {
	ctx := context.Background()
	f := bound(t)
	f.Load(signup{FirstName: "Ada", Age: 36, News: true, Password: "kept-secret"})

	// Only the sent (or listed) fields move: Password isn't rendered and
	// keeps its value; News was shown and unticked, so it goes false.
	f.change(ctx, map[string][]string{"firstName": {"Grace"}, "email": {""}}, []string{"firstName"}, []string{"firstName", "email", "news"})
	if f.FirstName != "Grace" || f.Password != "kept-secret" || f.Age != 36 || f.News {
		t.Fatalf("merged: %+v", f)
	}
	if !f.Changed("firstName") || f.Changed("age") || !f.Changed("news") {
		t.Fatalf("changed: %v", f.changed)
	}
	if f.Valid() {
		t.Fatal("email required")
	}
	// Errors show per touched field, all after a failed submit.
	if e := f.F("email").Error; e != "" {
		t.Fatalf("untouched email error shown: %q", e)
	}
	f.submitFailed(nil)
	if f.F("email").Error == "" || !f.Submitted() {
		t.Fatal("a failed submit shows every error")
	}

	// The Validate method is clean(): its errors join the tag rules'.
	f.change(ctx, map[string][]string{"firstName": {"root"}, "email": {"a@b.co"}}, nil, []string{"firstName", "email"})
	if f.Valid() || f.F("firstName").Error != "that name is taken" {
		t.Fatalf("Validate: %+v", f.errs)
	}

	// Dirty, Update, and a successful submit going back to the loaded values.
	if !f.Dirty() {
		t.Fatal("not dirty")
	}
	f.Update(func() { f.Age = 40 })
	if f.Age != 40 {
		t.Fatal("Update")
	}
	gen := f.gen
	f.submitOK()
	if f.FirstName != "Ada" || f.Age != 36 || f.Dirty() || f.gen == gen || f.Submitted() {
		t.Fatalf("after submitOK: %+v", f)
	}
}

type petDetails struct {
	Name string `form:"name"`
	Kind string `form:"kind"`
}

type embeddedDetailsForm struct {
	Form
	petDetails
}

// A form may take its fields from a struct it embeds.
func TestFormEmbeddedDetails(t *testing.T) {
	f := &embeddedDetailsForm{}
	f.bindOuter(reflect.ValueOf(f), "Edit", "")
	f.Load(embeddedDetailsForm{petDetails: petDetails{Name: "a", Kind: "b"}})
	f.change(context.Background(), map[string][]string{"name": {"x"}, "kind": {"y"}}, nil, []string{"name", "kind"})
	if f.Name != "x" || f.Kind != "y" {
		t.Fatalf("embedded fields after a change: %+v", f.petDetails)
	}
}

type blockWithLiveView struct {
	LiveView
	Edit embeddedDetailsForm
	N    Assign[int]
}

type pageWithBlock struct {
	blockWithLiveView
}

// A struct the page embeds is walked even when it embeds the LiveView
// itself (which has an Assign's methods): its forms bind and its Assigns
// are tracked.
func TestTrackerWalksBlockEmbeddingLiveView(t *testing.T) {
	p := &pageWithBlock{}
	tr := trackAssigns(reflect.ValueOf(p))
	if tr.forms["Edit"] == nil {
		t.Fatalf("the block's form isn't bound: %v", tr.forms)
	}
	if len(tr.fields) < 3 {
		t.Fatalf("tracked %d fields, want the LiveView, the form and N", len(tr.fields))
	}
	p.Edit.Load(embeddedDetailsForm{petDetails: petDetails{Name: "z"}})
	if p.Edit.Name != "z" {
		t.Fatalf("Load: %+v", p.Edit.petDetails)
	}
}
