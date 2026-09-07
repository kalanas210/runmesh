package config_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/kalanas210/runmesh/internal/config"
)

const otherKey = "fedcba9876543210fedcba9876543210"

// TestUnscopedKeyKeepsWeek1Behaviour is the compatibility guarantee. Every
// deployment configured before scopes existed uses `id=key`, and an upgrade
// that silently demoted those keys to "may do nothing" would take a fleet down
// at exactly the moment nobody is looking for an authorisation change.
func TestUnscopedKeyKeepsWeek1Behaviour(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(env(map[string]string{"RUNMESH_API_KEYS": "ci=" + goodKey}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	key, ok := cfg.APIKeys[config.KeyDigest(goodKey)]
	if !ok {
		t.Fatal("the key was not parsed")
	}
	if !key.Unscoped {
		t.Error("a key configured without scopes was not marked unscoped")
	}
	for _, s := range config.AllScopes {
		if !key.Allows(s) {
			t.Errorf("an unscoped key was refused %s", s)
		}
	}
	if got := cfg.UnscopedKeyIDs(); !slices.Equal(got, []string{"ci"}) {
		t.Errorf("UnscopedKeyIDs = %v, want [ci]: the boot log has to be able to name them", got)
	}
}

func TestScopedKeyGrantsOnlyWhatItNames(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(env(map[string]string{
		"RUNMESH_API_KEYS": "reader:jobs.read=" + goodKey +
			",planner:jobs.read+jobs.write=" + otherKey,
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	reader := cfg.APIKeys[config.KeyDigest(goodKey)]
	if reader.Unscoped {
		t.Error("a key that named a scope was marked unscoped")
	}
	if !reader.Allows(config.ScopeJobsRead) {
		t.Error("the reader key was refused the scope it names")
	}
	for _, denied := range []config.Scope{config.ScopeJobsWrite, config.ScopeJobsCancel, config.ScopeAdmin} {
		if reader.Allows(denied) {
			t.Errorf("a jobs.read key was granted %s", denied)
		}
	}

	planner := cfg.APIKeys[config.KeyDigest(otherKey)]
	if !planner.Allows(config.ScopeJobsRead) || !planner.Allows(config.ScopeJobsWrite) {
		t.Error("the planner key was refused a scope it names")
	}
	// The distinction the vocabulary exists for: an agent that submits work must
	// not thereby be able to stop everybody else's.
	if planner.Allows(config.ScopeJobsCancel) {
		t.Error("a jobs.read+jobs.write key was granted jobs.cancel")
	}
	if got, want := planner.ScopeList(), []string{"jobs.read", "jobs.write"}; !slices.Equal(got, want) {
		t.Errorf("ScopeList = %v, want %v", got, want)
	}
	if len(cfg.UnscopedKeyIDs()) != 0 {
		t.Errorf("UnscopedKeyIDs = %v, want none", cfg.UnscopedKeyIDs())
	}
}

// TestAdminImpliesEveryScope: an operator key must not need reissuing every
// time a scope is added, or scopes stop being added.
func TestAdminImpliesEveryScope(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(env(map[string]string{"RUNMESH_API_KEYS": "ops:admin=" + goodKey}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	key := cfg.APIKeys[config.KeyDigest(goodKey)]
	for _, s := range config.AllScopes {
		if !key.Allows(s) {
			t.Errorf("an admin key was refused %s", s)
		}
	}
	if key.Unscoped {
		t.Error("an admin key was marked unscoped; it is explicitly scoped to everything")
	}
}

// TestMistypedScopeIsRejectedAtBoot: a typo in a scope name would otherwise
// produce a key that authenticates and can do nothing, and the symptom would be
// a 403 in production rather than an error at boot.
func TestMistypedScopeIsRejectedAtBoot(t *testing.T) {
	t.Parallel()

	_, err := config.Load(env(map[string]string{"RUNMESH_API_KEYS": "ci:jobs.reed=" + goodKey}))
	if err == nil {
		t.Fatal("Load accepted an unknown scope name")
	}
	if !strings.Contains(err.Error(), "jobs.reed") || !strings.Contains(err.Error(), "jobs.read") {
		t.Errorf("error = %v, want it to quote the typo and list the valid scopes", err)
	}
}

func TestEmptyScopeListIsRejected(t *testing.T) {
	t.Parallel()

	_, err := config.Load(env(map[string]string{"RUNMESH_API_KEYS": "ci:=" + goodKey}))
	if err == nil {
		t.Fatal("Load accepted a key with a colon and no scopes")
	}
	if !strings.Contains(err.Error(), "names no scopes") {
		t.Errorf("error = %v, want it to explain how to grant scopes", err)
	}
}
