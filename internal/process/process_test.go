package process

import (
	"context"
	"dragrace/internal/executor"
	"os"
	"path/filepath"
	"testing"
)

// ── Helpers ─────────────────────────────────────────────────────────────────

func createScript(t *testing.T, dir, name, content string) string {
	t.Helper()
	scriptDir := filepath.Dir(filepath.Join(dir, name))
	os.MkdirAll(scriptDir, 0755)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatal(err)
	}
	return name
}

func newTestExecutor(t *testing.T) (*Executor, string) {
	t.Helper()
	dataDir := t.TempDir()
	exec, err := NewExecutor(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	return exec, dataDir
}

// ── RunScript: Env Vars ─────────────────────────────────────────────────────

func TestRunScript_EnvVars(t *testing.T) {
	exec, _ := newTestExecutor(t)
	defer exec.Close()

	repoDir := t.TempDir()
	script := createScript(t, repoDir, "test.sh", `#!/bin/sh
echo "MY_VAR=$MY_VAR"
echo "ANOTHER=$ANOTHER"
`)

	logs, err := exec.RunScript(context.Background(), &executor.RunOptions{
		ScriptPath: script,
		RepoDir:    repoDir,
		Env: map[string]string{
			"MY_VAR":  "hello",
			"ANOTHER": "world",
		},
	})
	if err != nil {
		t.Fatalf("RunScript failed: %v\nlogs: %s", err, logs)
	}
	if !contains(logs, "MY_VAR=hello") {
		t.Errorf("expected MY_VAR=hello in logs, got: %s", logs)
	}
	if !contains(logs, "ANOTHER=world") {
		t.Errorf("expected ANOTHER=world in logs, got: %s", logs)
	}
}

// ── RunScript: Args ─────────────────────────────────────────────────────────

func TestRunScript_Args(t *testing.T) {
	exec, _ := newTestExecutor(t)
	defer exec.Close()

	repoDir := t.TempDir()
	script := createScript(t, repoDir, "test.sh", `#!/bin/sh
echo "ARGS=$@"
echo "ARG1=$1"
echo "ARG2=$2"
`)

	logs, err := exec.RunScript(context.Background(), &executor.RunOptions{
		ScriptPath: script,
		RepoDir:    repoDir,
		Args:       []string{"--flag1", "value2"},
	})
	if err != nil {
		t.Fatalf("RunScript failed: %v\nlogs: %s", err, logs)
	}
	if !contains(logs, "ARG1=--flag1") {
		t.Errorf("expected ARG1=--flag1 in logs, got: %s", logs)
	}
	if !contains(logs, "ARG2=value2") {
		t.Errorf("expected ARG2=value2 in logs, got: %s", logs)
	}
}

// ── RunScript: Exit Code ────────────────────────────────────────────────────

func TestRunScript_ExitCode(t *testing.T) {
	exec, _ := newTestExecutor(t)
	defer exec.Close()

	repoDir := t.TempDir()
	script := createScript(t, repoDir, "fail.sh", `#!/bin/sh
echo "about to fail"
exit 42
`)

	_, err := exec.RunScript(context.Background(), &executor.RunOptions{
		ScriptPath: script,
		RepoDir:    repoDir,
	})
	if err == nil {
		t.Fatal("expected error for non-zero exit code")
	}
}

func TestRunScript_Success(t *testing.T) {
	exec, _ := newTestExecutor(t)
	defer exec.Close()

	repoDir := t.TempDir()
	script := createScript(t, repoDir, "ok.sh", `#!/bin/sh
echo "all good"
exit 0
`)

	logs, err := exec.RunScript(context.Background(), &executor.RunOptions{
		ScriptPath: script,
		RepoDir:    repoDir,
	})
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if !contains(logs, "all good") {
		t.Errorf("expected 'all good' in logs, got: %s", logs)
	}
}

// ── DataDir ─────────────────────────────────────────────────────────────────

func TestDataDir_CreateAndExists(t *testing.T) {
	exec, _ := newTestExecutor(t)
	defer exec.Close()
	ctx := context.Background()

	name := "test-data-dir"

	// Should not exist yet
	if exec.DataDirExists(ctx, name) {
		t.Fatal("data dir should not exist yet")
	}

	// Create it
	if err := exec.EnsureDataDir(ctx, name); err != nil {
		t.Fatalf("EnsureDataDir failed: %v", err)
	}

	// Should exist now
	if !exec.DataDirExists(ctx, name) {
		t.Fatal("data dir should exist after EnsureDataDir")
	}

	// Create again (idempotent)
	if err := exec.EnsureDataDir(ctx, name); err != nil {
		t.Fatalf("EnsureDataDir (second call) failed: %v", err)
	}

	// Remove
	if err := exec.RemoveDataDir(ctx, name); err != nil {
		t.Fatalf("RemoveDataDir failed: %v", err)
	}

	// Should not exist anymore
	if exec.DataDirExists(ctx, name) {
		t.Fatal("data dir should not exist after RemoveDataDir")
	}
}

// ── RunMeasured: Metrics ────────────────────────────────────────────────────

func TestRunMeasured_CollectsMetrics(t *testing.T) {
	exec, _ := newTestExecutor(t)
	defer exec.Close()

	repoDir := t.TempDir()
	script := createScript(t, repoDir, "compute.sh", `#!/bin/sh
# Do some work so metrics have something to measure
i=0
while [ $i -lt 100 ]; do
    i=$((i + 1))
done
echo "done"
`)

	metrics, err := exec.RunMeasured(context.Background(), &executor.RunOptions{
		ScriptPath: script,
		RepoDir:    repoDir,
	})
	if err != nil {
		t.Fatalf("RunMeasured failed: %v", err)
	}

	if metrics.Aggregates.ExecutionTimeMs <= 0 {
		t.Errorf("expected ExecutionTimeMs > 0, got %d", metrics.Aggregates.ExecutionTimeMs)
	}
}

// ── RunScript: Env + Args combined ──────────────────────────────────────────

func TestRunScript_EnvAndArgsCombined(t *testing.T) {
	exec, _ := newTestExecutor(t)
	defer exec.Close()

	repoDir := t.TempDir()
	script := createScript(t, repoDir, "combined.sh", `#!/bin/sh
echo "GREETING=$GREETING ARGS=$@"
`)

	logs, err := exec.RunScript(context.Background(), &executor.RunOptions{
		ScriptPath: script,
		RepoDir:    repoDir,
		Env:        map[string]string{"GREETING": "hello"},
		Args:       []string{"world", "foo bar"},
	})
	if err != nil {
		t.Fatalf("RunScript failed: %v\nlogs: %s", err, logs)
	}
	if !contains(logs, "GREETING=hello") {
		t.Errorf("expected GREETING=hello in logs, got: %s", logs)
	}
}

// ── Utility ─────────────────────────────────────────────────────────────────

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstring(s, substr))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
