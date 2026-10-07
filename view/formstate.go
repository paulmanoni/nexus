package view

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/paulmanoni/nexus/v2"
)

// Form makes a struct a live page's form when embedded — the struct is
// the whole declaration, as a Django form class is. Its tags say what the
// fields are:
//
//	type UserForm struct {
//		view.Form
//		Name   string `form:"name" label:"Full name" validate:"required"`
//		Email  string `form:"email" validate:"required,email"`
//		RoleID uint   `form:"roleId" label:"Role"`
//	}
//
// `form:` names a field (others are skipped), `validate:` checks it,
// `label:` (else the Go name, spaced), `help:`, `placeholder:` and `span:`
// (width in a 12-column row) say how it shows, and `input:` picks the
// control where the Go type isn't enough (password, textarea, switch,
// hidden). Optional methods on the struct complete the declaration:
//
//	// Choices of a field — Django's ModelChoiceField: the field renders
//	// as a select. The receiver is the form's current values, so choices
//	// may depend on other fields; services come via view.Use.
//	func (f UserForm) RoleIDChoices(ctx context.Context) []view.Choice {
//		return view.Use[*RoleService](ctx).Choices(ctx)
//	}
//
//	// Validate — Django's clean(): runs after the tag rules, as the user
//	// types on a live form and always before the submit, which therefore
//	// only ever sees valid values.
//	func (f UserForm) Validate(ctx context.Context) error {
//		if f.Name == f.Email {
//			return nexus.Invalid().Field("email", "an e-mail is not a name")
//		}
//		return nil
//	}
//
//	// Init — Django's initial: runs once, before the page's Mount.
//	func (f *UserForm) Init(ctx context.Context) { f.Active = true }
//
//	// Save takes the submit when ui.Form is given no method.
//	func (f *UserForm) Save(ctx context.Context) error { … }
//
// The page holds the form — a field, or embedded — and its fields are the
// values, read and written directly; the kit renders it (ui.Form):
//
//	type Users struct {
//		view.LiveView
//		Edit UserForm
//	}
//
//	func (p *Users) Open(ctx context.Context, id uint) error { p.Edit.Load(userRecord(id)); return nil }
//	func (p *Users) Save(ctx context.Context, f UserForm) error { … } // f passed every rule
//
//	templ (p *Users) Render() {
//		@ui.Form(p.Edit, p.Save)
//	}
//
// The form keeps what the browser has — the values the user typed, laid
// over the loaded ones, so a field the page doesn't render keeps its
// value. A field's error shows once the user has left it, every field's
// after a submit. A successful submit resets the form to the values it was
// loaded with; Load and Reset replace what the fields show even where the
// user typed. A form is tracked like an Assign.
type Form struct {
	name string // the page field it is declared on
	comp string // the live component type, for an embedded view's form

	// self points at the user's form struct this Form is embedded in; the
	// page's tracker binds it. data is a copy of its fields as Load (or
	// the first bind) left them.
	self reflect.Value
	meta *formMeta
	data reflect.Value

	errs      *nexus.Error
	touched   map[string]bool
	changed   map[string]bool
	submitted bool
	inited    bool
	gen       uint64

	ver uint64
	t   *tracker
	bit uint64
}

// bindOuter ties the embedded Form to the struct around it (*S) and its
// place on the page. The page's tracker calls it; tests may too.
func (f *Form) bindOuter(self reflect.Value, name, comp string) {
	f.self, f.name, f.comp = self, name, comp
	f.meta = formMetaOf(self.Type().Elem())
	f.data = f.snapshot()
}

// snapshot copies the form's fields (only them — never this Form itself).
func (f *Form) snapshot() reflect.Value {
	out := reflect.New(f.self.Type().Elem()).Elem()
	src := f.self.Elem()
	for _, name := range f.meta.names {
		fm := f.meta.byName[name]
		out.FieldByIndex(fm.index).Set(src.FieldByIndex(fm.index))
	}
	return out
}

func (f *Form) bound() {
	if !f.self.IsValid() {
		panic("view.Form: not bound to a page — load values with Load, never by assigning over the form struct")
	}
}

// Load gives the form the values to edit, as fresh: no errors, nothing
// touched, and the fields show these values even where the user typed.
// v is a value of the struct the Form is embedded in.
func (f *Form) Load(v any) {
	f.bound()
	rv := reflect.ValueOf(v)
	if rv.Type() != f.self.Type().Elem() {
		panic(fmt.Sprintf("view.Form.Load: want a %s, got %T", f.self.Type().Elem(), v))
	}
	dst := f.self.Elem()
	for _, name := range f.meta.names {
		fm := f.meta.byName[name]
		dst.FieldByIndex(fm.index).Set(rv.FieldByIndex(fm.index))
	}
	f.data = f.snapshot()
	f.fresh()
}

// Reset puts back the values the form was loaded with.
func (f *Form) Reset() {
	f.bound()
	f.restore()
	f.fresh()
}

func (f *Form) restore() {
	dst := f.self.Elem()
	for _, name := range f.meta.names {
		fm := f.meta.byName[name]
		dst.FieldByIndex(fm.index).Set(f.data.FieldByIndex(fm.index))
	}
}

func (f *Form) fresh() {
	f.errs, f.touched, f.changed, f.submitted = nil, nil, nil, false
	f.gen++
	f.touch()
}

// Update changes the form's values — read them directly, change them here,
// so the page re-renders what depends on them:
//
//	p.Edit.Update(func() { p.Edit.DesignationID = 0 })
//
// The fields show the new values, except one the user is typing in.
func (f *Form) Update(fn func()) {
	f.bound()
	fn()
	f.validate(context.Background())
	f.touch()
}

// Changed reports whether the change the page is handling — the change
// method's, or the submit's — altered field.
func (f Form) Changed(field string) bool {
	return f.changed[f.field(field).name]
}

// Dirty reports whether the form's values differ from what it was loaded
// with.
func (f Form) Dirty() bool {
	f.read()
	if !f.self.IsValid() {
		return false
	}
	cur := f.self.Elem()
	for _, name := range f.meta.names {
		fm := f.meta.byName[name]
		if !reflect.DeepEqual(cur.FieldByIndex(fm.index).Interface(), f.data.FieldByIndex(fm.index).Interface()) {
			return true
		}
	}
	return false
}

// Valid reports whether the form's values pass every rule: the validate:
// tags, and the struct's Validate method when it has one.
func (f Form) Valid() bool {
	f.read()
	return !f.errs.Any()
}

// Submitted reports whether a submit failed since the form was loaded.
func (f Form) Submitted() bool {
	f.read()
	return f.submitted
}

// Error is the form's own error, not any field's — a nexus.Invalid().Global
// from Validate or the submit event.
func (f Form) Error() string {
	f.read()
	if f.errs == nil {
		return ""
	}
	if msgs := f.errs.Fields[nexus.GlobalErrorKey]; len(msgs) > 0 {
		return msgs[0]
	}
	return ""
}

// FieldNames lists the form's fields, in the struct's order.
func (f Form) FieldNames() []string {
	f.bound()
	return f.meta.names
}

// F is the field of the form named name (its form: name, or its Go name):
// what to call it, its value as the form holds it, and its error when it
// should show. An unknown name is an error naming the fields there are —
// and, for a form the view compiler can see, a generate-time one.
func (f Form) F(name string) FormField {
	f.read()
	f.bound()
	fm := f.field(name)
	id := f.id()
	out := FormField{
		Form: id, Name: fm.name, ID: id + "-" + fm.name,
		Label: fm.label, Help: fm.help, Placeholder: fm.placeholder,
		Kind: fm.kind, Type: fm.typ, Span: fm.span, Required: fm.required,
		HasChoices: fm.choices >= 0,
	}
	fv := f.self.Elem().FieldByIndex(fm.index)
	switch fm.kind {
	case "checkbox", "switch":
		out.Checked = fv.Bool()
		out.Value = "true"
	case "multiselect":
		for i := range fv.Len() {
			out.Values = append(out.Values, formValue(fv.Index(i)))
		}
	default:
		out.Value = formValue(fv)
	}
	if f.errs != nil && (f.submitted || f.touched[fm.name]) {
		for _, key := range []string{fm.name, fm.errKey} {
			if msgs := f.errs.Fields[key]; len(msgs) > 0 {
				out.Error = msgs[0]
				break
			}
		}
	}
	return out
}

// ChoicesFor are the choices of the field: what its <Field>Choices method
// says, given the form's current values; nil without one.
func (f Form) ChoicesFor(ctx context.Context, field string) []Choice {
	f.read()
	f.bound()
	fm := f.field(field)
	if fm.choices < 0 {
		return nil
	}
	out := f.self.Elem().Method(fm.choices).Call([]reflect.Value{reflect.ValueOf(ctx)})
	return out[0].Interface().([]Choice)
}

// Choice is one option of a select field — what a <Field>Choices method
// returns.
type Choice struct {
	Value    string
	Label    string // Value when empty
	Disabled bool
}

// FormField is one field of a Form, ready for a component to render.
type FormField struct {
	Form              string // the form's id
	Name              string // the form: name the field is sent by
	ID                string
	Label             string
	Help, Placeholder string
	// Kind is the control: input, textarea, checkbox, switch, multiselect —
	// and an input renders as a select when the field has choices.
	Kind     string
	Type     string // an input's type: text, email, number, date, password, hidden
	Span     int    // width in a 12-column row; 0 is the full row
	Value    string
	Values   []string // a multiselect's
	Checked  bool     // a checkbox's
	Required bool
	Error    string // shown once the user left the field, or after a submit
	// HasChoices says the form declares choices for the field
	// (<Field>Choices); a component gets them with ChoicesFor.
	HasChoices bool
}

func (f Form) field(name string) *formFieldMeta {
	f.bound()
	fm, ok := f.meta.byName[name]
	if !ok {
		panic(fmt.Sprintf("view.Form (%s): no field %q — the form's fields are %s", f.meta.typ, name, strings.Join(f.meta.names, ", ")))
	}
	return fm
}

func (f Form) id() string {
	if f.name == "" {
		return "form"
	}
	return strings.ToLower(f.name[:1]) + f.name[1:]
}

func (f Form) read() {
	if f.t != nil && f.t.rec != nil {
		f.t.rec.read(f.bit)
	}
}

func (f *Form) touch() {
	if f.t != nil {
		f.ver = f.t.bump()
	} else {
		f.ver++
	}
}

func (f *Form) bindAssign(t *tracker, bit uint64) { f.t, f.bit = t, bit }
func (f *Form) version() uint64                   { return f.ver }

// validate re-checks the form's values: the validate: tags, then the
// struct's Validate method.
func (f *Form) validate(ctx context.Context) {
	f.errs = nil
	if errs, ok := validation(nexus.Validate(f.self.Elem().Interface())); ok {
		f.errs = errs
	}
	if f.meta.validate < 0 {
		return
	}
	out := f.self.Elem().Method(f.meta.validate).Call([]reflect.Value{reflect.ValueOf(ctx)})
	if err, _ := out[0].Interface().(error); err != nil {
		if errs, ok := validation(err); ok {
			f.mergeErrs(errs)
		} else {
			f.mergeErrs(nexus.Invalid().Global(nexus.ErrorOf(err).Message))
		}
	}
}

func (f *Form) mergeErrs(errs *nexus.Error) {
	if errs == nil || !errs.Any() {
		return
	}
	if f.errs == nil {
		f.errs = nexus.Invalid()
	}
	for field, msgs := range errs.Fields {
		for _, m := range msgs {
			f.errs.Field(field, m)
		}
	}
}

// ---- what the browser sends ----------------------------------------------

// liveForm is what the live page needs of a Form.
type liveForm interface {
	// change lays the fields the browser sent over the form's values —
	// fields names every control the form had, so an unticked checkbox
	// goes false rather than keeping its old value — and re-checks them.
	change(ctx context.Context, values map[string][]string, touched, fields []string)
	// submitFailed records a failed submit: errs join the form's own.
	submitFailed(errs *nexus.Error)
	// addErrs joins a change method's validation errors to the form's.
	addErrs(errs *nexus.Error)
	// errors are the form's current validation errors, nil when none.
	errors() *nexus.Error
	// submitOK resets the form: its submit succeeded.
	submitOK()
	// valueArg is the form's value, for calling an event that takes it.
	valueArg() reflect.Value
	// anyErrs reports whether the form's values fail any rule.
	anyErrs() bool
	// initOnce runs the struct's Init method, once, before the page's
	// Mount: a fresh form's defaults (Django's initial).
	initOnce(ctx context.Context)
	// saveSelf runs the struct's Save method; false without one.
	saveSelf(ctx context.Context) (error, bool)
}

func (f *Form) valueArg() reflect.Value { return f.self.Elem() }

func (f *Form) initOnce(ctx context.Context) {
	if f.inited || f.meta == nil || f.meta.init < 0 {
		return
	}
	f.inited = true
	f.self.Method(f.meta.init).Call([]reflect.Value{reflect.ValueOf(ctx)})
	f.data = f.snapshot()
	f.touch()
}

func (f *Form) saveSelf(ctx context.Context) (error, bool) {
	if f.meta == nil || f.meta.save < 0 {
		return nil, false
	}
	out := f.self.Method(f.meta.save).Call([]reflect.Value{reflect.ValueOf(ctx)})
	err, _ := out[0].Interface().(error)
	return err, true
}
func (f *Form) anyErrs() bool        { return f.errs.Any() }
func (f *Form) errors() *nexus.Error { return f.errs }

func (f *Form) addErrs(errs *nexus.Error) {
	f.mergeErrs(errs)
	f.touch()
}

func (f *Form) change(ctx context.Context, values map[string][]string, touched, fields []string) {
	for _, n := range touched {
		if f.touched == nil {
			f.touched = map[string]bool{}
		}
		f.touched[n] = true
	}
	st := f.self.Type().Elem()
	arg, err := bindForm(st, values)
	if err != nil {
		f.errs = nexus.Invalid().Global(err.Error())
		f.touch()
		return
	}
	// Present fields replace the form's; the rest keep their values — a
	// field the page doesn't render isn't zeroed by a keystroke.
	present := map[string]bool{}
	for k := range values {
		present[k] = true
	}
	for _, k := range fields {
		present[k] = true
	}
	next := f.self.Elem()
	f.changed = map[string]bool{}
	for _, name := range f.meta.names {
		fm := f.meta.byName[name]
		if !present[name] {
			continue
		}
		nv := arg.FieldByIndex(fm.index)
		if !reflect.DeepEqual(next.FieldByIndex(fm.index).Interface(), nv.Interface()) {
			f.changed[name] = true
			next.FieldByIndex(fm.index).Set(nv)
		}
	}
	f.validate(ctx)
	f.touch()
}

func (f *Form) submitFailed(errs *nexus.Error) {
	f.mergeErrs(errs)
	f.submitted = true
	f.touch()
}

func (f *Form) submitOK() {
	f.restore()
	f.fresh()
}

// ---- T's declaration ------------------------------------------------------

type formFieldMeta struct {
	name, errKey             string
	label, help, placeholder string
	kind, typ                string
	span                     int
	index                    []int
	required                 bool
	choices                  int // the <Field>Choices method's index on T; -1 without one
}

type formMeta struct {
	typ      string
	byName   map[string]*formFieldMeta
	names    []string
	validate int // the Validate method's index; -1 without one
	init     int // Init(ctx) on the pointer type; -1 without one
	save     int // Save(ctx) error on the pointer type; -1 without one
}

var formMetas sync.Map // reflect.Type → *formMeta

var (
	choicesType = reflect.TypeFor[[]Choice]()
	formCtxType = reflect.TypeFor[context.Context]()
	formErrType = reflect.TypeFor[error]()
	timeType    = reflect.TypeFor[time.Time]()
)

func formMetaOf(t reflect.Type) *formMeta {
	if m, ok := formMetas.Load(t); ok {
		return m.(*formMeta)
	}
	m := &formMeta{typ: t.Name(), byName: map[string]*formFieldMeta{}, validate: -1, init: -1, save: -1}
	collectFormFields(m, t, t, nil)
	if v, ok := t.MethodByName("Validate"); ok &&
		v.Type.NumIn() == 2 && v.Type.In(1) == formCtxType && v.Type.NumOut() == 1 && v.Type.Out(0) == formErrType {
		m.validate = v.Index
	}
	pt := reflect.PointerTo(t)
	if v, ok := pt.MethodByName("Init"); ok &&
		v.Type.NumIn() == 2 && v.Type.In(1) == formCtxType && v.Type.NumOut() == 0 {
		m.init = v.Index
	}
	if v, ok := pt.MethodByName("Save"); ok &&
		v.Type.NumIn() == 2 && v.Type.In(1) == formCtxType && v.Type.NumOut() == 1 && v.Type.Out(0) == formErrType {
		m.save = v.Index
	}
	formMetas.Store(t, m)
	return m
}

func collectFormFields(m *formMeta, root, t reflect.Type, index []int) {
	for i := range t.NumField() {
		sf := t.Field(i)
		path := append(slices.Clone(index), i)
		if sf.Anonymous && sf.Type.Kind() == reflect.Struct {
			if sf.Type == reflect.TypeFor[Form]() {
				continue // the machinery, not a field
			}
			collectFormFields(m, root, sf.Type, path)
			continue
		}
		name, _, _ := strings.Cut(sf.Tag.Get("form"), ",")
		if !sf.IsExported() || name == "" || name == "-" {
			continue
		}
		fm := &formFieldMeta{
			name: name, errKey: validationKey(sf), index: path, choices: -1,
			label: sf.Tag.Get("label"), help: sf.Tag.Get("help"), placeholder: sf.Tag.Get("placeholder"),
			required: slices.Contains(strings.Split(sf.Tag.Get("validate"), ","), "required"),
		}
		if n, err := strconv.Atoi(sf.Tag.Get("span")); err == nil && n >= 1 && n <= 12 {
			fm.span = n
		}
		if fm.label == "" {
			fm.label = humanize(sf.Name)
		}
		fm.kind, fm.typ = controlOf(sf)
		if cm, ok := root.MethodByName(sf.Name + "Choices"); ok &&
			cm.Type.NumIn() == 2 && cm.Type.In(1) == formCtxType && cm.Type.NumOut() == 1 && cm.Type.Out(0) == choicesType {
			fm.choices = cm.Index
		}
		m.byName[name] = fm
		m.byName[sf.Name] = fm
		m.names = append(m.names, name)
	}
}

// validationKey is the key nexus.Validate files a field's messages under.
func validationKey(f reflect.StructField) string {
	for _, key := range []string{"json", "form", "query", "path", "uri", "graphql"} {
		tag, _, _ := strings.Cut(f.Tag.Get(key), ",")
		if tag = strings.TrimSpace(tag); tag != "" && tag != "-" {
			return tag
		}
	}
	return strings.ToLower(f.Name[:1]) + f.Name[1:]
}

// controlOf is a field's control and input type, from its input: tag, else
// its Go type.
func controlOf(f reflect.StructField) (kind, typ string) {
	switch in := f.Tag.Get("input"); in {
	case "textarea", "switch", "checkbox":
		return in, ""
	case "":
	default:
		return "input", in // password, hidden, or any input type
	}
	t := f.Type
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == timeType:
		return "input", "date"
	case t.Kind() == reflect.Bool:
		return "checkbox", ""
	case t.Kind() >= reflect.Int && t.Kind() <= reflect.Float64:
		return "input", "number"
	case t.Kind() == reflect.Slice && t.Elem().Kind() != reflect.Uint8:
		return "multiselect", ""
	case slices.Contains(strings.Split(f.Tag.Get("validate"), ","), "email"):
		return "input", "email"
	}
	return "input", "text"
}

// humanize turns a Go name into a label: FirstName → First Name, RoleIDs →
// Role IDs.
func humanize(name string) string {
	var b strings.Builder
	rs := []rune(name)
	for i, r := range rs {
		if i > 0 && unicode.IsUpper(r) && (unicode.IsLower(rs[i-1]) || (i+1 < len(rs) && unicode.IsLower(rs[i+1]) && rs[i+1] != 's')) {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func formValue(v reflect.Value) string {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	if v.Type() == timeType {
		t := v.Interface().(time.Time)
		if t.IsZero() {
			return ""
		}
		return t.Format("2006-01-02")
	}
	switch v.Kind() {
	case reflect.String:
		return v.String()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(v.Uint(), 10)
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'f', -1, 64)
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	}
	return fmt.Sprint(v.Interface())
}
