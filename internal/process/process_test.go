package process

import (
	"context"
	"dragrace/internal/executor"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// ── Helpers ─────────────────────────────────────────────────────────────────

func TestNewExecutorRequiresProcessInspector(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("native executor is only supported on Linux and macOS")
	}
	t.Setenv("PATH", "")
	if _, err := NewExecutor(t.TempDir()); err == nil || !contains(err.Error(), "requires ps") {
		t.Fatalf("expected missing process inspector to fail closed, got %v", err)
	}
}

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

func TestRunScript_DoesNotExposeRunnerSecrets(t *testing.T) {
	exec, _ := newTestExecutor(t)
	defer exec.Close()
	t.Setenv("RUNNER_CLIENT_SECRET", "must-not-leak")

	repoDir := t.TempDir()
	script := createScript(t, repoDir, "env.sh", `#!/bin/sh
echo "secret=${RUNNER_CLIENT_SECRET:-missing}"
`)
	logs, err := exec.RunScript(context.Background(), &executor.RunOptions{
		ScriptPath: script,
		RepoDir:    repoDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(logs, "secret=missing") || contains(logs, "must-not-leak") {
		t.Fatalf("runner secret leaked into native child environment: %q", logs)
	}
}

func TestRunScript_RejectsScriptSymlinkEscape(t *testing.T) {
	exec, _ := newTestExecutor(t)
	defer exec.Close()

	repoDir := t.TempDir()
	outsideDir := t.TempDir()
	outside := createScript(t, outsideDir, "outside.sh", "#!/bin/sh\necho escaped\n")
	if err := os.Symlink(filepath.Join(outsideDir, outside), filepath.Join(repoDir, "escape.sh")); err != nil {
		t.Fatal(err)
	}

	_, err := exec.RunScript(context.Background(), &executor.RunOptions{
		ScriptPath: "escape.sh",
		RepoDir:    repoDir,
	})
	if err == nil || !contains(err.Error(), "escapes repository") {
		t.Fatalf("expected symlink escape rejection, got %v", err)
	}
}

func TestRunScript_TimeoutKillsWholeProcessGroup(t *testing.T) {
	exec, _ := newTestExecutor(t)
	defer exec.Close()
	repoDir := t.TempDir()
	pidFile := filepath.Join(repoDir, "child.pid")
	script := createScript(t, repoDir, "children.sh", fmt.Sprintf(`#!/bin/sh
sleep 30 &
echo $! > %s
wait
`, strconv.Quote(pidFile)))

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := exec.RunScript(ctx, &executor.RunOptions{ScriptPath: script, RepoDir: repoDir})
		result <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(pidFile); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	err := <-result
	var limitErr *LimitError
	if errors.As(err, &limitErr) || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation classification, got %v", err)
	}

	pidBytes, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, _ := strconv.Atoi(string(bytesTrimSpace(pidBytes)))
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) && syscall.Kill(pid, 0) == nil {
		time.Sleep(20 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatalf("child process %d survived process-group timeout", pid)
	}
}

func TestRunScript_ClassifiesDeadline(t *testing.T) {
	exec, _ := newTestExecutor(t)
	defer exec.Close()
	repoDir := t.TempDir()
	script := createScript(t, repoDir, "timeout.sh", "#!/bin/sh\nsleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := exec.RunScript(ctx, &executor.RunOptions{ScriptPath: script, RepoDir: repoDir})
	var limitErr *LimitError
	if !errors.As(err, &limitErr) || limitErr.Kind != LimitTimeout {
		t.Fatalf("expected timeout classification, got %v", err)
	}
}

func TestRunScript_ClassifiesProcessLimit(t *testing.T) {
	exec, _ := newTestExecutor(t)
	defer exec.Close()
	repoDir := t.TempDir()
	script := createScript(t, repoDir, "fork.sh", `#!/bin/sh
sleep 30 & sleep 30 & sleep 30 & sleep 30 & wait
`)

	_, err := exec.RunScript(context.Background(), &executor.RunOptions{
		ScriptPath: script,
		RepoDir:    repoDir,
		Limits:     &executor.ResourceLimits{PidsLimit: 3},
	})
	var limitErr *LimitError
	if !errors.As(err, &limitErr) || limitErr.Kind != LimitProcesses {
		t.Fatalf("expected process limit classification, got %v", err)
	}
}

func TestRunScript_ClassifiesMemoryLimit(t *testing.T) {
	testNativeHelperLimit(t, "memory", &executor.ResourceLimits{MemoryBytes: 32 * 1024 * 1024}, LimitMemory)
}

func TestRunScript_ClassifiesCPULimit(t *testing.T) {
	testNativeHelperLimit(t, "cpu", &executor.ResourceLimits{CPUNano: 100_000_000}, LimitCPU)
}

func TestRunScript_ClassifiesFilesystemLimit(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("native executor is only supported on Linux and macOS")
	}
	exec, _ := newTestExecutor(t)
	defer exec.Close()
	repoDir := t.TempDir()
	script := createScript(t, repoDir, "large-file.sh", `#!/bin/sh
set -e
dd if=/dev/zero of=large.bin bs=1048576 count=4 2>/dev/null
`)

	_, err := exec.RunScript(context.Background(), &executor.RunOptions{
		ScriptPath: script,
		RepoDir:    repoDir,
		Limits:     &executor.ResourceLimits{DiskBytes: 512 * 1024},
	})
	var limitErr *LimitError
	if !errors.As(err, &limitErr) || limitErr.Kind != LimitFilesystem {
		t.Fatalf("expected filesystem limit classification, got %v", err)
	}
}

func testNativeHelperLimit(t *testing.T, mode string, limits *executor.ResourceLimits, expected LimitKind) {
	t.Helper()
	exec, _ := newTestExecutor(t)
	defer exec.Close()
	repoDir := t.TempDir()
	script := createScript(t, repoDir, "helper.sh", fmt.Sprintf(`#!/bin/sh
exec %s -test.run=TestNativeLimitHelper
`, strconv.Quote(os.Args[0])))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := exec.RunScript(ctx, &executor.RunOptions{
		ScriptPath: script,
		RepoDir:    repoDir,
		Env: map[string]string{
			"DRAGRACE_NATIVE_LIMIT_HELPER": mode,
		},
		Limits: limits,
	})
	var limitErr *LimitError
	if !errors.As(err, &limitErr) || limitErr.Kind != expected {
		t.Fatalf("expected %s limit classification, got %v", expected, err)
	}
}

func TestNativeLimitHelper(t *testing.T) {
	switch os.Getenv("DRAGRACE_NATIVE_LIMIT_HELPER") {
	case "memory":
		chunks := make([][]byte, 0, 128)
		for {
			chunk := make([]byte, 1024*1024)
			for i := range chunk {
				chunk[i] = 1
			}
			chunks = append(chunks, chunk)
		}
	case "cpu":
		for {
		}
	}
}

func bytesTrimSpace(value []byte) []byte {
	start, end := 0, len(value)
	for start < end && (value[start] == ' ' || value[start] == '\n' || value[start] == '\r' || value[start] == '\t') {
		start++
	}
	for end > start && (value[end-1] == ' ' || value[end-1] == '\n' || value[end-1] == '\r' || value[end-1] == '\t') {
		end--
	}
	return value[start:end]
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

func TestDataDir_RejectsTraversal(t *testing.T) {
	exec, _ := newTestExecutor(t)
	defer exec.Close()
	if err := exec.EnsureDataDir(context.Background(), "../escape"); err == nil {
		t.Fatal("expected data directory traversal to be rejected")
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

	metrics, logs, err := exec.RunMeasured(context.Background(), &executor.RunOptions{
		ScriptPath: script,
		RepoDir:    repoDir,
	})
	if err != nil {
		t.Fatalf("RunMeasured failed: %v", err)
	}
	if !contains(logs, "done") {
		t.Errorf("expected measured stdout in logs, got: %s", logs)
	}

	if metrics.Aggregates.ExecutionTimeMs <= 0 {
		t.Errorf("expected ExecutionTimeMs > 0, got %d", metrics.Aggregates.ExecutionTimeMs)
	}
}

func TestRunMeasured_RejectsStdoutTraversal(t *testing.T) {
	exec, _ := newTestExecutor(t)
	defer exec.Close()
	repoDir := t.TempDir()
	script := createScript(t, repoDir, "output.sh", "#!/bin/sh\necho result\n")

	_, _, err := exec.RunMeasured(context.Background(), &executor.RunOptions{
		ScriptPath: script,
		RepoDir:    repoDir,
		Stdout:     "../outside.txt",
	})
	if err == nil || !contains(err.Error(), "invalid stdout file") {
		t.Fatalf("expected stdout traversal rejection, got %v", err)
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
