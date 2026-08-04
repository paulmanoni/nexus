package maskid

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/paulmanoni/nexus/internal/maskhook"
)

func codec() *AESCodec { return NewAESCodec([]byte("test-key")) }

func TestCodecRoundTrip(t *testing.T) {
	c := codec()
	for _, id := range []int64{0, 1, 2, 41, 4294967296, 1<<62 - 1, -1} {
		s := c.Mask(id)
		if len(s) != maskLen {
			t.Fatalf("Mask(%d) = %q, want %d chars", id, s, maskLen)
		}
		got, ok := c.Unmask(s)
		if !ok || got != id {
			t.Fatalf("Unmask(Mask(%d)) = %d, %v", id, got, ok)
		}
	}
}

func TestCodecIsDeterministic(t *testing.T) {
	c := codec()
	if c.Mask(7) != c.Mask(7) {
		t.Fatal("masks must be stable, or URLs and caches break")
	}
}

// Adjacent ids must not produce adjacent masks — the whole point is that
// a client can't step from one record to the next.
func TestCodecHidesAdjacency(t *testing.T) {
	c := codec()
	a, b := c.Mask(1000), c.Mask(1001)
	if a == b {
		t.Fatal("distinct ids collided")
	}
	same := 0
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] == b[i] {
			same++
		}
	}
	if same > len(a)/2 {
		t.Fatalf("masks for 1000/1001 share %d/%d characters — too much structure leaked", same, len(a))
	}
}

func TestCodecRejectsForgeries(t *testing.T) {
	c := codec()
	other := NewAESCodec([]byte("a different key"))

	for _, s := range []string{
		"",
		"41",
		"not-a-mask",
		c.Mask(41)[:maskLen-1],   // truncated
		"A" + c.Mask(41)[1:],     // tampered
		other.Mask(41),           // right shape, wrong key
		"AAAAAAAAAAAAAAAAAAAAAA", // well-formed base64url of the right length
	} {
		if id, ok := c.Unmask(s); ok {
			t.Errorf("Unmask(%q) accepted, decoded to %d", s, id)
		}
	}
}

func TestDefaultMatch(t *testing.T) {
	for _, k := range []string{"id", "ID", "ids", "userId", "user_id", "OwnerID", "categoryIds", "country_id"} {
		if !DefaultMatch(k) {
			t.Errorf("DefaultMatch(%q) = false, want true", k)
		}
	}
	// The suffix rule is case-sensitive precisely so these stay out.
	for _, k := range []string{"", "name", "valid", "paid", "android", "hybrid", "uuid", "guid", "void", "candid"} {
		if DefaultMatch(k) {
			t.Errorf("DefaultMatch(%q) = true, want false", k)
		}
	}
}

// install wires a policy directly, bypassing Module (which also registers
// an extension plugin the tests don't need).
func install(t *testing.T, cfg Config) *policy {
	t.Helper()
	p := &policy{codec: codec(), match: DefaultMatch, incl: keySet(cfg.Include), excl: keySet(cfg.Exclude)}
	if cfg.Match != nil {
		p.match = cfg.Match
	}
	maskhook.Install(maskhook.Hooks{IsID: p.isID, Mask: p.mask, Unmask: p.unmask})
	mu.Lock()
	current = p
	mu.Unlock()
	t.Cleanup(func() {
		maskhook.Uninstall()
		mu.Lock()
		current = nil
		mu.Unlock()
	})
	return p
}

func TestIncludeExclude(t *testing.T) {
	p := install(t, Config{Include: []string{"reference"}, Exclude: []string{"tenantId"}})

	if !p.isID("reference") {
		t.Error("Include should mask a field the default policy misses")
	}
	// Exclude is checked first so it beats both Include and Match.
	if p.isID("tenantId") {
		t.Error("Exclude must win over the default policy")
	}
}

func TestMaskValueWalksNestedResults(t *testing.T) {
	install(t, Config{})

	type item struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	type resp struct {
		OwnerID int    `json:"ownerId"`
		Items   []item `json:"items"`
		UUID    string `json:"uuid"`
	}

	out := maskhook.MaskValue(resp{OwnerID: 7, Items: []item{{ID: 41, Name: "a", Count: 3}}, UUID: "abc"})
	blob, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatal(err)
	}

	owner, ok := got["ownerId"].(string)
	if !ok {
		t.Fatalf("ownerId = %#v, want a masked string", got["ownerId"])
	}
	if n, ok := Unmask(owner); ok && n != 7 {
		t.Errorf("ownerId unmasked to %d, want 7", n)
	}

	first := got["items"].([]any)[0].(map[string]any)
	if _, ok := first["id"].(string); !ok {
		t.Errorf("nested items[0].id = %#v, want a masked string", first["id"])
	}
	// Non-ID fields, and a string id, are left exactly as they were.
	if first["count"].(float64) != 3 {
		t.Errorf("count was rewritten: %#v", first["count"])
	}
	if first["name"] != "a" || got["uuid"] != "abc" {
		t.Error("non-ID fields must pass through untouched")
	}
}

func TestUnmaskJSONReversesMaskValue(t *testing.T) {
	install(t, Config{})

	masked, err := json.Marshal(maskhook.MaskValue(map[string]any{
		"id":         41,
		"categoryId": 9,
		"title":      "hello",
	}))
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(maskhook.UnmaskJSON(masked), &got); err != nil {
		t.Fatal(err)
	}
	if got["id"] != float64(41) || got["categoryId"] != float64(9) {
		t.Errorf("round trip lost the ids: %#v", got)
	}
	if got["title"] != "hello" {
		t.Errorf("non-ID field changed: %#v", got["title"])
	}
}

// A 64-bit id must survive the marshal/decode round trip that a plain
// float64 decode would silently truncate.
func TestLargeIDsSurviveTheRoundTrip(t *testing.T) {
	install(t, Config{})
	const big = int64(9007199254740993) // 2^53 + 1

	out := maskhook.MaskValue(map[string]any{"id": big})
	s := out.(map[string]any)["id"].(string)
	if n, ok := Unmask(s); !ok || n != big {
		t.Fatalf("Unmask = %d, %v; want %d", n, ok, big)
	}
}

func TestUnmaskParamHandlesLists(t *testing.T) {
	install(t, Config{})
	a, b := Mask(3), Mask(5)

	if got := maskhook.UnmaskParam("ids", a+","+b); got != "3,5" {
		t.Errorf("UnmaskParam list = %q, want \"3,5\"", got)
	}
	// A value that isn't a mask passes through, so an unmigrated client
	// sending a raw id still works.
	if got := maskhook.UnmaskParam("id", "12"); got != "12" {
		t.Errorf("UnmaskParam(%q) = %q, want it unchanged", "12", got)
	}
	if got := maskhook.UnmaskParam("name", a); got != a {
		t.Errorf("a non-ID field must not be unmasked, got %q", got)
	}
}

func TestDisabledIsAPassThrough(t *testing.T) {
	maskhook.Uninstall()
	in := map[string]any{"id": 41}
	if got := maskhook.MaskValue(in); got == nil {
		t.Fatal("MaskValue returned nil while disabled")
	}
	blob, _ := json.Marshal(maskhook.MaskValue(in))
	if string(blob) != `{"id":41}` {
		t.Errorf("disabled masking rewrote the response: %s", blob)
	}
	if maskhook.Enabled() {
		t.Error("Enabled() true with nothing installed")
	}
}

// Module must install the hooks eagerly — the GraphQL schema is built
// from maskhook.Enabled() while options are still being assembled, so a
// lifecycle hook would land too late.
func TestModuleInstallsEagerly(t *testing.T) {
	maskhook.Uninstall()
	t.Cleanup(func() {
		maskhook.Uninstall()
		mu.Lock()
		current = nil
		mu.Unlock()
	})

	if opt := Module(Config{Key: "boot-key"}); opt == nil {
		t.Fatal("Module returned a nil Option")
	}
	if !maskhook.Enabled() {
		t.Fatal("Module did not install the hook")
	}
	if !maskhook.IsIDKey("ownerId") || maskhook.IsIDKey("title") {
		t.Error("the installed policy is not the default field policy")
	}
	s, ok := maskhook.MaskID("id", 41)
	if !ok {
		t.Fatal("MaskID refused an id field")
	}
	if n, ok := maskhook.UnmaskID("id", s); !ok || n != 41 {
		t.Fatalf("round trip through the installed hook = %d, %v", n, ok)
	}
}

type inScope struct {
	ID      int `json:"id"`
	OwnerID int `json:"ownerId"`
}

type outOfScope struct {
	ID      int `json:"id"`
	OwnerID int `json:"ownerId"`
}

func installScoped(t *testing.T, cfg Config) {
	t.Helper()
	p := &policy{
		codec: codec(), match: DefaultMatch,
		incl: keySet(cfg.Include), excl: keySet(cfg.Exclude),
		types: typeSet(cfg.Types), matchType: cfg.MatchType,
	}
	h := maskhook.Hooks{IsID: p.isID, Mask: p.mask, Unmask: p.unmask}
	if p.types != nil || p.matchType != nil {
		h.TypeAllowed = p.typeAllowed
	}
	maskhook.Install(h)
	mu.Lock()
	current = p
	mu.Unlock()
	t.Cleanup(func() {
		maskhook.Uninstall()
		mu.Lock()
		current = nil
		mu.Unlock()
	})
}

func idOf(t *testing.T, v any) any {
	t.Helper()
	blob, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatal(err)
	}
	return m["id"]
}

// The scope is what lets an app mask the types that stay inside it while
// leaving alone the ones whose IDs travel to another system.
func TestTypesScopesMasking(t *testing.T) {
	installScoped(t, Config{Types: []string{"inScope"}})

	if _, ok := idOf(t, maskhook.MaskValue(inScope{ID: 41})).(string); !ok {
		t.Error("an in-scope type was not masked")
	}
	if got := idOf(t, maskhook.MaskValue(outOfScope{ID: 41})); got != float64(41) {
		t.Errorf("an out-of-scope type was masked: %#v", got)
	}
	// Pointers and slices resolve to the same underlying type name.
	if _, ok := idOf(t, maskhook.MaskValue(&inScope{ID: 41})).(string); !ok {
		t.Error("a pointer to an in-scope type was not masked")
	}
	rows, ok := maskhook.MaskValue([]inScope{{ID: 41}}).([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("slice masking returned %#v", rows)
	}
	if _, ok := rows[0].(map[string]any)["id"].(string); !ok {
		t.Error("a slice of an in-scope type was not masked")
	}
}

func TestMatchTypeScopesMasking(t *testing.T) {
	installScoped(t, Config{MatchType: func(n string) bool { return n == "inScope" }})

	if _, ok := idOf(t, maskhook.MaskValue(inScope{ID: 41})).(string); !ok {
		t.Error("MatchType did not admit the type it matched")
	}
	if got := idOf(t, maskhook.MaskValue(outOfScope{ID: 41})); got != float64(41) {
		t.Errorf("MatchType masked a type it did not match: %#v", got)
	}
}

// Unmasking is deliberately unscoped: a value converts only if it decrypts,
// so an out-of-scope type's plain integer is unaffected either way. Scoping
// it too would mean an inbound mask silently failing to resolve.
func TestUnmaskingIgnoresTheScope(t *testing.T) {
	installScoped(t, Config{Types: []string{"inScope"}})
	masked := Mask(41)

	body := []byte(`{"id":"` + masked + `","ownerId":7}`)
	var got map[string]any
	if err := json.Unmarshal(maskhook.UnmaskJSON(body), &got); err != nil {
		t.Fatal(err)
	}
	if got["id"] != float64(41) {
		t.Errorf("a mask failed to unmask under a scope: %#v", got["id"])
	}
	if got["ownerId"] != float64(7) {
		t.Errorf("a plain integer was altered: %#v", got["ownerId"])
	}
}

// An unscoped config must keep masking everything.
func TestNoScopeMasksEverything(t *testing.T) {
	install(t, Config{})
	for _, v := range []any{inScope{ID: 41}, outOfScope{ID: 41}} {
		if _, ok := idOf(t, maskhook.MaskValue(v)).(string); !ok {
			t.Errorf("%T was not masked without a scope", v)
		}
	}
}

type wrapper[T any] struct {
	Data T `json:"data"`
}

// Handlers routinely return a generic envelope. Its reflect name is
// "wrapper[github.com/…/maskid.inScope]", which nobody would put in a
// scope, so the type arguments have to be unwrapped for the scope to work
// at all on the JSON-walking transports.
func TestScopeSeesThroughGenericWrappers(t *testing.T) {
	installScoped(t, Config{Types: []string{"inScope"}})

	unwrap := func(v any) any {
		t.Helper()
		blob, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(blob, &m); err != nil {
			t.Fatal(err)
		}
		return m.Data["id"]
	}

	if _, ok := unwrap(maskhook.MaskValue(&wrapper[*inScope]{Data: &inScope{ID: 41}})).(string); !ok {
		t.Error("a wrapped in-scope type was not masked")
	}
	if got := unwrap(maskhook.MaskValue(&wrapper[*outOfScope]{Data: &outOfScope{ID: 41}})); got != float64(41) {
		t.Errorf("a wrapped out-of-scope type was masked: %#v", got)
	}
	// The wrapper's own name must not admit everything it ever carries.
	if got := unwrap(maskhook.MaskValue(&wrapper[*outOfScope]{Data: &outOfScope{ID: 41}})); got == "" {
		t.Error("unexpected empty id")
	}
}

// A slice inside the envelope is the list-endpoint shape.
func TestScopeSeesThroughWrappedSlices(t *testing.T) {
	installScoped(t, Config{Types: []string{"inScope"}})

	out := maskhook.MaskValue(&wrapper[[]inScope]{Data: []inScope{{ID: 41}}})
	blob, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Data) != 1 {
		t.Fatalf("got %d rows", len(m.Data))
	}
	if _, ok := m.Data[0]["id"].(string); !ok {
		t.Errorf("a wrapped slice of an in-scope type was not masked: %#v", m.Data[0]["id"])
	}
}

type pageProps struct {
	Rows      []inScope `json:"rows"`
	RowIDs    []uint    `json:"rowIds"`
	CountryID int       `json:"countryId"`
}

type outOfScopePage struct {
	Rows   []inScope `json:"rows"`
	RowIDs []uint    `json:"rowIds"`
}

// An Inertia page carries heterogeneous props: an entity list alongside a
// bare []uint of the same entities' IDs. Judging each prop by its own root
// type masks the first and not the second, and the two silently stop
// comparing equal — the exact shape of a real bug this caught.
func TestPageScopeMasksEveryProp(t *testing.T) {
	installScoped(t, Config{Types: []string{"inScope", "pageProps"}, Exclude: []string{"countryId"}})

	props := map[string]any{
		"rows":      []inScope{{ID: 41}},
		"rowIds":    []uint{41},
		"countryId": 1,
	}
	maskhook.MaskProps(pageProps{}, props)

	rows, _ := json.Marshal(props["rows"])
	if !bytes.Contains(rows, []byte(`"id":"`)) {
		t.Errorf("entity ids not masked: %s", rows)
	}
	ids, _ := json.Marshal(props["rowIds"])
	if bytes.Contains(ids, []byte("41")) {
		t.Errorf("the sidecar id list was left raw, so it no longer matches the entity ids: %s", ids)
	}
	// An excluded reference key stays numeric even on an in-scope page.
	if props["countryId"] != json.Number("1") && props["countryId"] != float64(1) {
		t.Errorf("an excluded key was masked: %#v", props["countryId"])
	}
}

// A page out of scope keeps the per-prop behaviour, so an app returning a
// bare map or a shared props type is unaffected.
func TestOutOfScopePageFallsBackToPerProp(t *testing.T) {
	installScoped(t, Config{Types: []string{"inScope"}})

	props := map[string]any{"rows": []inScope{{ID: 41}}, "rowIds": []uint{41}}
	maskhook.MaskProps(outOfScopePage{}, props)

	rows, _ := json.Marshal(props["rows"])
	if !bytes.Contains(rows, []byte(`"id":"`)) {
		t.Errorf("an in-scope prop should still mask on an out-of-scope page: %s", rows)
	}
	ids, _ := json.Marshal(props["rowIds"])
	if !bytes.Contains(ids, []byte("41")) {
		t.Errorf("an untyped prop must not mask on an out-of-scope page: %s", ids)
	}
}
