package graph

import (
	"testing"
)

// Tests for the validation-rule constructors. The rules are
// framework-internal (ValidationRule interface from
// graphql_validation_rules.go); these tests lock in their
// metadata semantics without driving full query validation
// (which would need a ValidationContext setup beyond v1.0
// scope here).

// TestValidation_BaseRule_DefaultsEnabled: NewBaseRule
// returns an enabled rule by default — operators don't have
// to call Enable() explicitly.
func TestValidation_BaseRule_DefaultsEnabled(t *testing.T) {
	r := NewBaseRule("test-rule")
	if r.Name() != "test-rule" {
		t.Errorf("Name = %q, want test-rule", r.Name())
	}
	if !r.Enabled() {
		t.Error("new BaseRule should default to enabled")
	}
}

// TestValidation_BaseRule_EnableDisable: toggling via the
// Enable / Disable / SetEnabled methods works.
func TestValidation_BaseRule_EnableDisable(t *testing.T) {
	r := NewBaseRule("toggleable")
	r.Disable()
	if r.Enabled() {
		t.Error("Disable should set Enabled false")
	}
	r.Enable()
	if !r.Enabled() {
		t.Error("Enable should set Enabled true")
	}
	r.SetEnabled(false)
	if r.Enabled() {
		t.Error("SetEnabled(false) should disable")
	}
}

// TestValidation_BaseRule_NewError: NewError builds a
// ValidationError carrying the rule's name + the given
// message. Operators rely on this in custom rule
// implementations.
func TestValidation_BaseRule_NewError(t *testing.T) {
	r := NewBaseRule("custom")
	err := r.NewError("nope")
	if err == nil {
		t.Fatal("NewError returned nil")
	}
	if err.Rule != "custom" {
		t.Errorf("Rule = %q, want custom", err.Rule)
	}
	if err.Message != "nope" {
		t.Errorf("Message = %q, want nope", err.Message)
	}
}

// TestValidation_SecurityRule_Names: each security rule's
// constructor returns a rule with a recognizable name +
// enabled state. Locks in the surface tests + lint output
// will read.
func TestValidation_SecurityRule_Names(t *testing.T) {
	cases := []struct {
		name string
		rule ValidationRule
	}{
		{"max-depth", NewMaxDepthRule(10)},
		{"max-complexity", NewMaxComplexityRule(100)},
		{"max-aliases", NewMaxAliasesRule(5)},
		{"max-tokens", NewMaxTokensRule(1000)},
		{"no-introspection", NewNoIntrospectionRule()},
	}
	for _, c := range cases {
		if c.rule == nil {
			t.Errorf("%s: constructor returned nil", c.name)
			continue
		}
		if !c.rule.Enabled() {
			t.Errorf("%s: rule should be enabled by default", c.name)
		}
		if c.rule.Name() == "" {
			t.Errorf("%s: rule should have a non-empty name", c.name)
		}
	}
}

// TestValidation_AuthRule_Names: same shape for the auth/
// permission rule constructors. These wrap auth checks the
// framework's middleware consumes.
func TestValidation_AuthRule_Names(t *testing.T) {
	r := NewRequireAuthRule()
	if r == nil {
		t.Fatal("NewRequireAuthRule returned nil")
	}
	if !r.Enabled() {
		t.Error("RequireAuthRule should default to enabled")
	}
	if r.Name() == "" {
		t.Error("RequireAuthRule should have a non-empty name")
	}
}
