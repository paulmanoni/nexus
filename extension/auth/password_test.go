package auth

import (
	"context"
	"strings"
	"testing"
)

func TestPasswordValidators(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := []struct {
		name    string
		v       PasswordValidator
		pw      string
		id      *Identity
		wantErr string // "" = should pass
	}{
		{"minlen too short", MinLength(8), "abc", nil, "at least 8"},
		{"minlen ok", MinLength(8), "abcdefgh", nil, ""},
		{"numeric only", NotNumericOnly(), "12345678", nil, "entirely numeric"},
		{"numeric mixed ok", NotNumericOnly(), "12345678a", nil, ""},
		{"common", NotCommon(CommonPasswords()...), "password", nil, "too common"},
		{"common ok", NotCommon(CommonPasswords()...), "xK9$mn2Pq", nil, ""},
		{"similar to user", NotSimilarToUser(), "alice123", &Identity{ID: "alice"}, "too similar"},
		{"not similar ok", NotSimilarToUser(), "xK9$mn2Pq", &Identity{ID: "alice"}, ""},
		{"nil identity passes similar", NotSimilarToUser(), "anything", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.v.Validate(ctx, tc.pw, tc.id)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want pass, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestValidatePasswordChainStopsAtFirst(t *testing.T) {
	t.Parallel()
	err := ValidatePassword(context.Background(), "123", nil, DefaultValidators()...)
	if err == nil || !strings.Contains(err.Error(), "at least 8") {
		t.Fatalf("expected MinLength failure first, got %v", err)
	}
	if err := ValidatePassword(context.Background(), "xK9$mn2Pq!", &Identity{ID: "bob"}, DefaultValidators()...); err != nil {
		t.Fatalf("strong password should pass: %v", err)
	}
}
