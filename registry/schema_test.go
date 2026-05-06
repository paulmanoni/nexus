package registry

import "testing"

// TestSanitizeTypeName covers every shape Go's reflect produces for
// generic / pointer / sliced / package-qualified type names. Each
// case must yield a valid TypeScript identifier (letters, digits,
// underscore) so the generated .d.ts type-checks downstream.
//
// User-reported regressions documented inline.
func TestSanitizeTypeName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Plain — unchanged
		{"plain", "Pet", "Pet"},
		{"underscored", "Pet_v2", "Pet_v2"},

		// Pointer prefix stripped
		{"pointer", "*Pet", "Pet"},

		// Package path stripped
		{"pkg.Name", "main.Pet", "Pet"},
		{"slash.Name", "portal_admin/migrations.Pet", "Pet"},
		{"slash.dotted.Name", "portal_admin/migrations/dbmigrations.RunState", "RunState"},

		// Slice prefix → "...List"
		{"slice plain", "[]Pet", "PetList"},
		{"slice qualified", "[]portal_admin/migrations.Pet", "PetList"},
		{"slice pointer", "[]*Pet", "PetList"},

		// Generics
		{"generic plain", "Response[Pet]", "ResponseOfPet"},
		{"generic pointer", "Response[*Pet]", "ResponseOfPet"},
		{"generic qualified", "Response[*portal_admin/migrations/dbmigrations.RunState]", "ResponseOfRunState"},
		{"generic slice", "Response[[]portal_admin/migrations.MigrationMeta]", "ResponseOfMigrationMetaList"},
		{"generic state dto",
			"Response[[]portal_admin/migrations.MigrationStateDTO]",
			"ResponseOfMigrationStateDTOList"},

		// Page-style framework type
		{"Page main.Pet", "Page[main.Pet]", "PageOfPet"},
		{"Page slice", "Page[[]Pet]", "PageOfPetList"},

		// Multi-arg generic
		{"Map[K,V]", "Map[K,V]", "MapOfKAndV"},
		{"Map spaced", "Map[K, V]", "MapOfKAndV"},
		{"Map[Pet,*pkg.User]", "Map[Pet,*pkg.User]", "MapOfPetAndUser"},

		// Nested generics
		{"nested", "Page[Response[Pet]]", "PageOfResponseOfPet"},
		{"nested slice", "Page[Response[[]Pet]]", "PageOfResponseOfPetList"},

		// Defensive: empty / pointer-only edge cases
		{"empty", "", ""},
		{"pointer only", "*", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sanitizeTypeName(c.in)
			if got != c.want {
				t.Errorf("sanitizeTypeName(%q): got %q, want %q", c.in, got, c.want)
			}
		})
	}
}
