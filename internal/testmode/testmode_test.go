package testmode

import (
	"os"
	"path/filepath"
	"testing"
)

// ── Helpers ─────────────────────────────────────────────────────────────────

// createTempChallenge creates a temporary challenge directory with a dragrace.yaml.
func createTempChallenge(t *testing.T, yaml string, scripts map[string]string) string {
	t.Helper()
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "dragrace.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	for name, content := range scripts {
		scriptDir := filepath.Dir(filepath.Join(dir, name))
		os.MkdirAll(scriptDir, 0755)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0755); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

// ── Validate Challenge Spec ─────────────────────────────────────────────────

func TestValidateChallengeSpec_Valid(t *testing.T) {
	dir := createTempChallenge(t, `
version: "1.0"
type: challenge
challenge:
  id: test-challenge
  name: Test Challenge
init:
  docker: "alpine:latest"
  script: "scripts/init.sh"
validate:
  docker: "alpine:latest"
  script: "scripts/validate.sh"
limits:
  memory: "512MB"
  cpu: "1.0"
  timeout: "60s"
scoring:
  primary: execution_time
  direction: minimize
`, map[string]string{
		"scripts/init.sh":     "#!/bin/sh\necho init",
		"scripts/validate.sh": "#!/bin/sh\necho validate",
	})

	spec, err := ValidateChallengeDir(dir)
	if err != nil {
		t.Fatalf("expected valid challenge, got error: %v", err)
	}
	if spec.Challenge.ID != "test-challenge" {
		t.Errorf("expected id 'test-challenge', got '%s'", spec.Challenge.ID)
	}
	if spec.Challenge.Name != "Test Challenge" {
		t.Errorf("expected name 'Test Challenge', got '%s'", spec.Challenge.Name)
	}
}

func TestValidateChallengeSpec_WrongType(t *testing.T) {
	dir := createTempChallenge(t, `
version: "1.0"
type: solution
challenge:
  id: test
  name: Test
`, nil)

	_, err := ValidateChallengeDir(dir)
	if err == nil {
		t.Fatal("expected error for wrong type")
	}
}

func TestValidateChallengeSpec_MissingID(t *testing.T) {
	dir := createTempChallenge(t, `
version: "1.0"
type: challenge
challenge:
  name: Test
`, nil)

	_, err := ValidateChallengeDir(dir)
	if err == nil {
		t.Fatal("expected error for missing id")
	}
}

func TestValidateChallengeSpec_MissingName(t *testing.T) {
	dir := createTempChallenge(t, `
version: "1.0"
type: challenge
challenge:
  id: test
`, nil)

	_, err := ValidateChallengeDir(dir)
	if err == nil {
		t.Fatal("expected error for missing name")
	}
}

func TestValidateChallengeSpec_ScriptNotFound(t *testing.T) {
	dir := createTempChallenge(t, `
version: "1.0"
type: challenge
challenge:
  id: test
  name: Test
init:
  docker: "alpine:latest"
  script: "scripts/nonexistent.sh"
`, nil)

	_, err := ValidateChallengeDir(dir)
	if err == nil {
		t.Fatal("expected error for missing script")
	}
}

func TestValidateChallengeSpec_NoYAML(t *testing.T) {
	dir := t.TempDir()
	_, err := ValidateChallengeDir(dir)
	if err == nil {
		t.Fatal("expected error for missing dragrace.yaml")
	}
}

// ── Validate Solution Spec ──────────────────────────────────────────────────

func TestValidateSolutionSpec_Valid(t *testing.T) {
	dir := createTempChallenge(t, `
version: "1.0"
type: solution
runtime:
  docker: "golang:1.21"
build:
  script: "scripts/build.sh"
run:
  script: "scripts/run.sh"
  stdout: "/data/output.txt"
`, map[string]string{
		"scripts/build.sh": "#!/bin/sh\necho build",
		"scripts/run.sh":   "#!/bin/sh\necho run",
	})

	spec, err := ValidateSolutionDir(dir)
	if err != nil {
		t.Fatalf("expected valid solution, got error: %v", err)
	}
	if spec.Runtime.Docker != "golang:1.21" {
		t.Errorf("expected docker 'golang:1.21', got '%s'", spec.Runtime.Docker)
	}
}

func TestValidateSolutionSpec_WrongType(t *testing.T) {
	dir := createTempChallenge(t, `
version: "1.0"
type: challenge
runtime:
  docker: "golang:1.21"
run:
  script: "scripts/run.sh"
`, map[string]string{
		"scripts/run.sh": "#!/bin/sh\necho run",
	})

	_, err := ValidateSolutionDir(dir)
	if err == nil {
		t.Fatal("expected error for wrong type")
	}
}

func TestValidateSolutionSpec_MissingRuntime(t *testing.T) {
	dir := createTempChallenge(t, `
version: "1.0"
type: solution
run:
  script: "scripts/run.sh"
`, map[string]string{
		"scripts/run.sh": "#!/bin/sh\necho run",
	})

	_, err := ValidateSolutionDir(dir)
	if err == nil {
		t.Fatal("expected error for missing runtime.docker")
	}
}

func TestValidateSolutionSpec_MissingRunScript(t *testing.T) {
	dir := createTempChallenge(t, `
version: "1.0"
type: solution
runtime:
  docker: "golang:1.21"
run:
  script: "scripts/nonexistent.sh"
`, nil)

	_, err := ValidateSolutionDir(dir)
	if err == nil {
		t.Fatal("expected error for missing run script")
	}
}

// ── Phase Selection ─────────────────────────────────────────────────────────

func TestShouldRunPhase_NoFilter(t *testing.T) {
	// Empty phases = run all
	if !ShouldRunPhase(nil, PhaseInit) {
		t.Error("expected init to run with no filter")
	}
	if !ShouldRunPhase(nil, PhaseRun) {
		t.Error("expected run to run with no filter")
	}
	if !ShouldRunPhase([]string{}, PhaseBuild) {
		t.Error("expected build to run with empty filter")
	}
}

func TestShouldRunPhase_SinglePhase(t *testing.T) {
	phases := []string{PhaseInit}
	if !ShouldRunPhase(phases, PhaseInit) {
		t.Error("expected init to run when selected")
	}
	if ShouldRunPhase(phases, PhaseBuild) {
		t.Error("expected build NOT to run when only init selected")
	}
	if ShouldRunPhase(phases, PhaseRun) {
		t.Error("expected run NOT to run when only init selected")
	}
	if ShouldRunPhase(phases, PhaseValidate) {
		t.Error("expected validate NOT to run when only init selected")
	}
}

// ── Run: requires solution ──────────────────────────────────────────────────

func TestRun_RequiresSolutionForBuildPhase(t *testing.T) {
	dir := createTempChallenge(t, `
version: "1.0"
type: challenge
challenge:
  id: test
  name: Test
limits:
  memory: "512MB"
  cpu: "1.0"
  timeout: "60s"
scoring:
  primary: execution_time
  direction: minimize
`, nil)

	err := Run(&Options{
		ChallengeDir: dir,
		SolutionDir:  "", // no solution
		Executor:     "process",
		Phases:       []string{PhaseBuild},
		DataDir:      t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error when running build without solution")
	}
}

// ── Env/Args propagation ────────────────────────────────────────────────────

func TestEnvPropagation(t *testing.T) {
	// Verify Options struct accepts env vars
	opts := &Options{
		Env: map[string]string{"FOO": "bar", "ROW_COUNT": "1000"},
	}
	if opts.Env["FOO"] != "bar" {
		t.Errorf("expected FOO=bar, got %s", opts.Env["FOO"])
	}
	if opts.Env["ROW_COUNT"] != "1000" {
		t.Errorf("expected ROW_COUNT=1000, got %s", opts.Env["ROW_COUNT"])
	}
}

func TestArgsPropagation(t *testing.T) {
	// Verify Options struct accepts args
	opts := &Options{
		Args: []string{"--small", "--stations=100"},
	}
	if len(opts.Args) != 2 {
		t.Fatalf("expected 2 args, got %d", len(opts.Args))
	}
	if opts.Args[0] != "--small" {
		t.Errorf("expected --small, got %s", opts.Args[0])
	}
	if opts.Args[1] != "--stations=100" {
		t.Errorf("expected --stations=100, got %s", opts.Args[1])
	}
}
