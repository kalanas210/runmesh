package config_test

import (
	"strings"
	"testing"

	"github.com/kalanas210/runmesh/internal/config"
)

// TestPolicyDefaultsAreClosed. Every default that decides how much of the
// machine an LLM-authored step may touch has to be the restrictive one, because
// the population that runs on defaults is everybody who has not yet thought
// about it.
func TestPolicyDefaultsAreClosed(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(env(nil))
	if err != nil {
		t.Fatalf("a minimal environment was rejected: %v", err)
	}

	if cfg.PolicyAllowNetwork {
		t.Error("network egress is granted by default; a runtime that executes " +
			"model-authored plans should not reach the internet because nobody " +
			"said otherwise")
	}
	for name, got := range map[string]string{
		"default cpu":               cfg.PolicyDefaultCPU,
		"default memory":            cfg.PolicyDefaultMemory,
		"default ephemeral storage": cfg.PolicyDefaultEphemeralStorage,
		"max cpu":                   cfg.PolicyMaxCPU,
		"max memory":                cfg.PolicyMaxMemory,
		"max ephemeral storage":     cfg.PolicyMaxEphemeralStorage,
	} {
		if got == "" {
			t.Errorf("%s has no default; an unset limit in Kubernetes is not a "+
				"small share, it is as much as the node has", name)
		}
	}
	// python_execute is not registered without an image, which is the honest
	// answer: a tool that passes validation and fails every attempt is worse
	// than one that is plainly absent.
	if cfg.PythonImage != "" {
		t.Errorf("PythonImage defaults to %q; it should be empty until an "+
			"operator names an image", cfg.PythonImage)
	}
	if cfg.K8sRunAsUser == 0 || cfg.K8sRunAsGroup == 0 {
		t.Error("the task uid/gid default to 0; runAsNonRoot with uid 0 is " +
			"refused at admission, for every pod")
	}
}

// TestTaskImageFallsBackToTheDefaultImage, so a single-image deployment
// configures one variable.
func TestTaskImageFallsBackToTheDefaultImage(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(env(map[string]string{
		"RUNMESH_EXECUTOR":  "kubernetes",
		"RUNMESH_K8S_IMAGE": "runmesh/task:dev",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TaskImage != "runmesh/task:dev" {
		t.Errorf("TaskImage = %q, want the configured default image", cfg.TaskImage)
	}

	cfg, err = config.Load(env(map[string]string{
		"RUNMESH_EXECUTOR":   "kubernetes",
		"RUNMESH_K8S_IMAGE":  "runmesh/task:dev",
		"RUNMESH_TASK_IMAGE": "ghcr.io/kalanas210/task:v2",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TaskImage != "ghcr.io/kalanas210/task:v2" {
		t.Errorf("TaskImage = %q, want the override", cfg.TaskImage)
	}
}

// TestContradictoryPolicyConfigurationIsRefused.
func TestContradictoryPolicyConfigurationIsRefused(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		overrides map[string]string
		wantIn    string
	}{
		"a tool on both lists": {
			map[string]string{
				"RUNMESH_POLICY_TOOLS_ALLOW": "echo,sleep",
				"RUNMESH_POLICY_TOOLS_DENY":  "sleep",
			},
			"both",
		},
		"python allowlisted with no image": {
			map[string]string{"RUNMESH_POLICY_TOOLS_ALLOW": "python_execute"},
			"RUNMESH_PYTHON_IMAGE",
		},
		"a root task uid": {
			map[string]string{"RUNMESH_K8S_RUN_AS_USER": "0"},
			"runAsNonRoot",
		},
	} {
		_, err := config.Load(env(tc.overrides))
		if err == nil {
			t.Errorf("%s: accepted; the failure it produces is discovered from a "+
				"rejected plan or a pod that never schedules", name)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantIn) {
			t.Errorf("%s: error %q does not mention %q, so it does not say what "+
				"to change", name, err, tc.wantIn)
		}
	}
}

// TestToolListsDropEmptyEntries. A trailing comma in an allowlist would
// otherwise add "" to it, which quietly matches a tool whose name nobody set.
func TestToolListsDropEmptyEntries(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(env(map[string]string{
		"RUNMESH_POLICY_TOOLS_ALLOW": " echo , , sleep,",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"echo", "sleep"}
	if len(cfg.PolicyAllowTools) != len(want) {
		t.Fatalf("allowlist = %q, want %q", cfg.PolicyAllowTools, want)
	}
	for i, name := range want {
		if cfg.PolicyAllowTools[i] != name {
			t.Errorf("allowlist[%d] = %q, want %q", i, cfg.PolicyAllowTools[i], name)
		}
	}
}
