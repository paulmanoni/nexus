package auth

import (
	"context"
	"fmt"
	"strings"
)

// PasswordValidator rejects weak passwords at registration / change time —
// the Django AUTH_PASSWORD_VALIDATORS analogue. Validate returns a non-nil
// error describing why the password is unacceptable; the error message is
// safe to show the user. id may be nil (e.g. at sign-up before a user
// exists); validators that inspect user attributes must tolerate that.
type PasswordValidator interface {
	Validate(ctx context.Context, password string, id *Identity) error
}

// ValidatePassword runs password through each validator in order and
// returns the first failure, or nil when every validator passes.
func ValidatePassword(ctx context.Context, password string, id *Identity, validators ...PasswordValidator) error {
	for _, v := range validators {
		if err := v.Validate(ctx, password, id); err != nil {
			return err
		}
	}
	return nil
}

// validatorFunc adapts a function to PasswordValidator.
type validatorFunc func(ctx context.Context, password string, id *Identity) error

func (f validatorFunc) Validate(ctx context.Context, password string, id *Identity) error {
	return f(ctx, password, id)
}

// MinLength requires at least n characters (counted as runes, so a
// multibyte password isn't unfairly rejected).
func MinLength(n int) PasswordValidator {
	return validatorFunc(func(_ context.Context, password string, _ *Identity) error {
		if len([]rune(password)) < n {
			return fmt.Errorf("password must be at least %d characters", n)
		}
		return nil
	})
}

// NotNumericOnly rejects an all-digit password (Django's NumericPasswordValidator).
func NotNumericOnly() PasswordValidator {
	return validatorFunc(func(_ context.Context, password string, _ *Identity) error {
		if password == "" {
			return nil // MinLength owns emptiness
		}
		for _, r := range password {
			if r < '0' || r > '9' {
				return nil
			}
		}
		return fmt.Errorf("password can't be entirely numeric")
	})
}

// NotSimilarToUser rejects a password that contains (or is contained by)
// the identity's ID — a cheap guard against "username as password". A nil
// identity passes (nothing to compare against).
func NotSimilarToUser() PasswordValidator {
	return validatorFunc(func(_ context.Context, password string, id *Identity) error {
		if id == nil || id.ID == "" {
			return nil
		}
		p := strings.ToLower(password)
		u := strings.ToLower(id.ID)
		if len(u) >= 3 && (strings.Contains(p, u) || strings.Contains(u, p)) {
			return fmt.Errorf("password is too similar to the username")
		}
		return nil
	})
}

// NotCommon rejects a password found in the supplied deny-list (compared
// case-insensitively). Pass your own list of leaked/common passwords, or
// use CommonPasswords() for a small built-in set of the usual offenders.
func NotCommon(list ...string) PasswordValidator {
	set := make(map[string]struct{}, len(list))
	for _, p := range list {
		set[strings.ToLower(p)] = struct{}{}
	}
	return validatorFunc(func(_ context.Context, password string, _ *Identity) error {
		if _, bad := set[strings.ToLower(password)]; bad {
			return fmt.Errorf("password is too common")
		}
		return nil
	})
}

// CommonPasswords is a small built-in deny-list for NotCommon — the
// perennial worst offenders. Supply a fuller leaked-password corpus in
// production (e.g. the SecLists top-10k) via NotCommon(list...).
func CommonPasswords() []string {
	return []string{
		"password", "123456", "123456789", "12345678", "12345", "1234567",
		"qwerty", "abc123", "password1", "111111", "letmein", "welcome",
		"admin", "iloveyou", "monkey", "dragon", "sunshine", "princess",
		"passw0rd", "football", "changeme", "654321", "superman", "qwerty123",
	}
}

// DefaultValidators returns a reasonable baseline: at least 8 characters,
// not entirely numeric, not a built-in common password, and not obviously
// the username.
func DefaultValidators() []PasswordValidator {
	return []PasswordValidator{
		MinLength(8),
		NotNumericOnly(),
		NotCommon(CommonPasswords()...),
		NotSimilarToUser(),
	}
}
