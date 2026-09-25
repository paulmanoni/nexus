package tsgen

import "testing"

func TestLiteral(t *testing.T) {
	for in, want := range map[string]string{
		`Users/Index`:     `'Users/Index'`,
		`it's`:            `'it\'s'`,
		`a\b`:             `'a\\b'`,
		"a\nb\rc":         `'a\nb\rc'`,
		"x\u2028y\u2029z": "'x\\u2028y\\u2029z'",
	} {
		if got := Literal(in); got != want {
			t.Errorf("Literal(%q) = %s, want %s", in, got, want)
		}
	}
}
