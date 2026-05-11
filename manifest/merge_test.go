package manifest

import (
	"errors"
	"reflect"
	"testing"
)

// helper: build a base manifest with one of each input type so tests
// can target specific rules without re-declaring boilerplate.
func baseManifest() Manifest {
	min32 := 32
	return Manifest{
		Environments: []Environment{
			{Name: "production"},
			{Name: "staging"},
			{Name: "preview"},
		},
		Env: []EnvVar{
			{
				Name:    "LOG_LEVEL",
				Default: "info",
				Validation: &EnvValidation{
					Enum: []string{"debug", "info", "warn", "error"},
				},
				EnvScoped: true,
			},
			{
				Name:    "FEATURE_FLAGS",
				Default: "",
			},
		},
		Secrets: []Secret{
			{
				Name:      "JWT_SIGNING_KEY",
				Required:  true,
				EnvScoped: true,
				Validation: &EnvValidation{
					Length: &Range{Min: &min32},
				},
			},
			{
				Name:        "STRIPE_API_KEY",
				Description: "for payments",
			},
		},
		Files: []File{
			{
				Name: "tls_bundle",
				Path: "/etc/ssl/app/bundle.pem",
				Mode: 0400,
			},
		},
		Services: []ServiceNeed{
			{
				Name:    "main_db",
				Kind:    "postgres",
				Version: "16",
				Size:    "small",
				Backup:  "daily",
				ExposeAs: map[string]string{
					"url":  "DATABASE_URL",
					"host": "DB_HOST",
				},
			},
		},
		Hooks: &Hooks{
			Build:     []string{"go build ./..."},
			Predeploy: []string{"./bin/app migrate"},
		},
	}
}

func TestMerge_NoOverrideForEnvironment_ReturnsBaseCopy(t *testing.T) {
	base := baseManifest()
	out, err := MergeOverrides(base, "staging")
	if err != nil {
		t.Fatalf("MergeOverrides: %v", err)
	}
	if out.Overrides != nil {
		t.Errorf("effective manifest should have nil Overrides; got %v", out.Overrides)
	}
	// Mutating the result must not leak into base.
	out.Env[0].Default = "debug"
	if base.Env[0].Default != "info" {
		t.Error("base was aliased — deepCopyManifest didn't copy")
	}
}

func TestMerge_UnknownEnvironment_Rejected(t *testing.T) {
	base := baseManifest()
	_, err := MergeOverrides(base, "doesnotexist")
	var me *MergeError
	if !errors.As(err, &me) || me.Code != ErrUnknownEnvironment {
		t.Fatalf("got %v, want code %s", err, ErrUnknownEnvironment)
	}
}

func TestMerge_EmptyEnvironment_ReturnsBaseCopy(t *testing.T) {
	base := baseManifest()
	out, err := MergeOverrides(base, "")
	if err != nil {
		t.Fatalf("MergeOverrides: %v", err)
	}
	if len(out.Env) != len(base.Env) {
		t.Errorf("Env count: got %d, want %d", len(out.Env), len(base.Env))
	}
}

func TestMerge_ScalarLock_ReplacesDefaultAndMarksSource(t *testing.T) {
	base := baseManifest()
	base.Overrides = map[string]Override{
		"production": {Env: map[string]string{"LOG_LEVEL": "warn"}},
	}
	out, err := MergeOverrides(base, "production")
	if err != nil {
		t.Fatalf("MergeOverrides: %v", err)
	}
	got, _ := findEnvVarIndex(out.Env, "LOG_LEVEL")
	if out.Env[got].Default != "warn" {
		t.Errorf("LOG_LEVEL.Default: got %q, want warn", out.Env[got].Default)
	}
	if out.Env[got].Source != "override" {
		t.Errorf("LOG_LEVEL.Source: got %q, want override", out.Env[got].Source)
	}
	// Constraints stay intact — scalar lock doesn't wipe validation.
	if out.Env[got].Validation == nil || len(out.Env[got].Validation.Enum) != 4 {
		t.Errorf("LOG_LEVEL.Validation lost; got %+v", out.Env[got].Validation)
	}
}

func TestMerge_EnvSpecDiff_AdjustsFieldsByField(t *testing.T) {
	base := baseManifest()
	stableOnly := "stable-only"
	required := true
	base.Overrides = map[string]Override{
		"production": {
			EnvSpecs: map[string]EnvVarPatch{
				"FEATURE_FLAGS": {Default: &stableOnly, Required: &required},
			},
		},
	}
	out, err := MergeOverrides(base, "production")
	if err != nil {
		t.Fatalf("MergeOverrides: %v", err)
	}
	idx, _ := findEnvVarIndex(out.Env, "FEATURE_FLAGS")
	if out.Env[idx].Default != "stable-only" {
		t.Errorf("Default: got %q, want stable-only", out.Env[idx].Default)
	}
	if !out.Env[idx].Required {
		t.Error("Required: should have been flipped to true")
	}
	// Source NOT set for spec diffs — only scalar locks lock the field.
	if out.Env[idx].Source != "" {
		t.Errorf("Source should be unset for spec diffs, got %q", out.Env[idx].Source)
	}
}

func TestMerge_ScalarAndSpecForSameKey_Rejected(t *testing.T) {
	base := baseManifest()
	def := "x"
	base.Overrides = map[string]Override{
		"production": {
			Env:      map[string]string{"LOG_LEVEL": "warn"},
			EnvSpecs: map[string]EnvVarPatch{"LOG_LEVEL": {Default: &def}},
		},
	}
	_, err := MergeOverrides(base, "production")
	var me *MergeError
	if !errors.As(err, &me) || me.Code != ErrConflictingOverride {
		t.Fatalf("got %v, want code %s", err, ErrConflictingOverride)
	}
}

func TestMerge_UnknownEnvKey_Rejected(t *testing.T) {
	base := baseManifest()
	base.Overrides = map[string]Override{
		"production": {Env: map[string]string{"NOT_DECLARED": "x"}},
	}
	_, err := MergeOverrides(base, "production")
	var me *MergeError
	if !errors.As(err, &me) || me.Code != ErrUnknownOverrideKey {
		t.Fatalf("got %v, want code %s", err, ErrUnknownOverrideKey)
	}
}

func TestMerge_SecretSpec_TightensRequired(t *testing.T) {
	base := baseManifest()
	required := true
	base.Overrides = map[string]Override{
		"production": {
			SecretSpecs: map[string]SecretPatch{
				"STRIPE_API_KEY": {Required: &required},
			},
		},
	}
	out, err := MergeOverrides(base, "production")
	if err != nil {
		t.Fatalf("MergeOverrides: %v", err)
	}
	idx, _ := findSecretIndex(out.Secrets, "STRIPE_API_KEY")
	if !out.Secrets[idx].Required {
		t.Error("STRIPE_API_KEY should be required in production")
	}
}

func TestMerge_ServiceDeepMerge_PreservesUnmentionedFields(t *testing.T) {
	base := baseManifest()
	large := "large"
	hourly := "hourly"
	base.Overrides = map[string]Override{
		"production": {
			Services: map[string]ServicePatch{
				"main_db": {
					Size:     &large,
					Backup:   &hourly,
					ExposeAs: map[string]string{"password": "DB_PASSWORD"},
				},
			},
		},
	}
	out, err := MergeOverrides(base, "production")
	if err != nil {
		t.Fatalf("MergeOverrides: %v", err)
	}
	idx, _ := findServiceIndex(out.Services, "main_db")
	s := out.Services[idx]
	if s.Kind != "postgres" || s.Version != "16" {
		t.Errorf("kind/version lost: got kind=%q version=%q", s.Kind, s.Version)
	}
	if s.Size != "large" {
		t.Errorf("Size: got %q, want large", s.Size)
	}
	if s.Backup != "hourly" {
		t.Errorf("Backup: got %q, want hourly", s.Backup)
	}
	want := map[string]string{
		"url":      "DATABASE_URL",
		"host":     "DB_HOST",
		"password": "DB_PASSWORD",
	}
	if !reflect.DeepEqual(s.ExposeAs, want) {
		t.Errorf("ExposeAs: got %v, want %v", s.ExposeAs, want)
	}
}

func TestMerge_Removed_DropsEntry(t *testing.T) {
	base := baseManifest()
	base.Overrides = map[string]Override{
		"preview": {Removed: []string{"secrets.STRIPE_API_KEY"}},
	}
	out, err := MergeOverrides(base, "preview")
	if err != nil {
		t.Fatalf("MergeOverrides: %v", err)
	}
	if _, found := findSecretIndex(out.Secrets, "STRIPE_API_KEY"); found {
		t.Error("STRIPE_API_KEY should have been removed in preview")
	}
	// JWT_SIGNING_KEY should still be there.
	if _, found := findSecretIndex(out.Secrets, "JWT_SIGNING_KEY"); !found {
		t.Error("JWT_SIGNING_KEY was wrongly removed")
	}
	// Base unaffected.
	if _, found := findSecretIndex(base.Secrets, "STRIPE_API_KEY"); !found {
		t.Error("base manifest mutated by removal")
	}
}

func TestMerge_RemovedMalformedPath_Rejected(t *testing.T) {
	base := baseManifest()
	base.Overrides = map[string]Override{
		"preview": {Removed: []string{"STRIPE_API_KEY"}}, // missing bucket
	}
	_, err := MergeOverrides(base, "preview")
	var me *MergeError
	if !errors.As(err, &me) || me.Code != ErrUnknownOverrideKey {
		t.Fatalf("got %v, want code %s", err, ErrUnknownOverrideKey)
	}
}

func TestMerge_RemovedUnknownBucket_Rejected(t *testing.T) {
	base := baseManifest()
	base.Overrides = map[string]Override{
		"preview": {Removed: []string{"notabucket.X"}},
	}
	_, err := MergeOverrides(base, "preview")
	var me *MergeError
	if !errors.As(err, &me) || me.Code != ErrUnknownOverrideKey {
		t.Fatalf("got %v, want code %s", err, ErrUnknownOverrideKey)
	}
}

func TestMerge_RemovedNotInBase_Rejected(t *testing.T) {
	base := baseManifest()
	base.Overrides = map[string]Override{
		"preview": {Removed: []string{"secrets.NEVER_DECLARED"}},
	}
	_, err := MergeOverrides(base, "preview")
	var me *MergeError
	if !errors.As(err, &me) || me.Code != ErrUnknownOverrideKey {
		t.Fatalf("got %v, want code %s", err, ErrUnknownOverrideKey)
	}
}

func TestMerge_HooksWholeReplace(t *testing.T) {
	base := baseManifest()
	base.Overrides = map[string]Override{
		"production": {
			Hooks: &Hooks{
				Build: []string{"go build -tags=prod -ldflags=-s -w ./..."},
				// No Predeploy here — confirms whole-replace semantics.
			},
		},
	}
	out, err := MergeOverrides(base, "production")
	if err != nil {
		t.Fatalf("MergeOverrides: %v", err)
	}
	if out.Hooks == nil {
		t.Fatal("Hooks should be non-nil")
	}
	if len(out.Hooks.Build) != 1 || out.Hooks.Build[0] != "go build -tags=prod -ldflags=-s -w ./..." {
		t.Errorf("Build: got %v", out.Hooks.Build)
	}
	if out.Hooks.Predeploy != nil {
		t.Errorf("Predeploy should have been wiped (whole-replace), got %v", out.Hooks.Predeploy)
	}
	// Base hooks unchanged.
	if len(base.Hooks.Predeploy) != 1 {
		t.Error("base Hooks.Predeploy mutated by merge")
	}
}

func TestMerge_DefaultIsolatedFromBase(t *testing.T) {
	// Modifying the effective Validation.Enum slice must not change base.
	base := baseManifest()
	out, err := MergeOverrides(base, "production")
	if err != nil {
		t.Fatalf("MergeOverrides: %v", err)
	}
	idx, _ := findEnvVarIndex(out.Env, "LOG_LEVEL")
	out.Env[idx].Validation.Enum[0] = "MUTATED"
	if base.Env[0].Validation.Enum[0] == "MUTATED" {
		t.Error("base manifest's Validation.Enum was aliased")
	}
}

func TestMerge_UnknownSecretSpec_Rejected(t *testing.T) {
	base := baseManifest()
	req := true
	base.Overrides = map[string]Override{
		"production": {
			SecretSpecs: map[string]SecretPatch{
				"DOES_NOT_EXIST": {Required: &req},
			},
		},
	}
	_, err := MergeOverrides(base, "production")
	var me *MergeError
	if !errors.As(err, &me) || me.Code != ErrUnknownOverrideKey {
		t.Fatalf("got %v, want code %s", err, ErrUnknownOverrideKey)
	}
}

func TestMerge_UnknownService_Rejected(t *testing.T) {
	base := baseManifest()
	large := "large"
	base.Overrides = map[string]Override{
		"production": {
			Services: map[string]ServicePatch{
				"missing_service": {Size: &large},
			},
		},
	}
	_, err := MergeOverrides(base, "production")
	var me *MergeError
	if !errors.As(err, &me) || me.Code != ErrUnknownOverrideKey {
		t.Fatalf("got %v, want code %s", err, ErrUnknownOverrideKey)
	}
}

func TestAvailableEnvironments_SortedDistinctSnapshot(t *testing.T) {
	base := baseManifest()
	got := AvailableEnvironments(base)
	want := []string{"preview", "production", "staging"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}