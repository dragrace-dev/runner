package config

import (
	"strings"
	"testing"
	"time"
)

func validChallengeYAML(extra string) []byte {
	return []byte(`
version: "1.0"
type: challenge
challenge:
  id: safe-challenge
  name: Safe challenge
limits:
  memory: "2GB"
  cpu: "2.0"
  timeout: "30s"
scoring:
  primary: elapsed_ms
  direction: minimize
` + extra)
}

func TestParseChallengeSpecRejectsUnknownAndDangerousFields(t *testing.T) {
	if _, err := ParseChallengeSpec(validChallengeYAML("dangerous_command: rm -rf /\n")); err == nil {
		t.Fatal("expected unknown field rejection")
	}

	badPath := []byte(strings.Replace(string(validChallengeYAML("")), "scoring:\n", "validate:\n  docker: alpine:3.19\n  script: ../escape.sh\nscoring:\n", 1))
	if _, err := ParseChallengeSpec(badPath); err == nil {
		t.Fatal("expected traversal path rejection")
	}
}

func TestValidateSolutionSpecRejectsUntrustedEscapes(t *testing.T) {
	for _, spec := range []SolutionConfig{
		{Version: SchemaVersion, Type: "solution", Runtime: RuntimeConfig{Docker: "alpine;rm -rf /"}, Run: RunConfig{Script: "run.sh"}},
		{Version: SchemaVersion, Type: "solution", Runtime: RuntimeConfig{Docker: "alpine:3.19"}, Run: RunConfig{Script: "../../escape.sh"}},
		{Version: "2.0", Type: "solution", Runtime: RuntimeConfig{Docker: "alpine:3.19"}, Run: RunConfig{Script: "run.sh"}},
	} {
		if err := ValidateSolutionSpec(&spec); err == nil {
			t.Fatalf("expected unsafe solution spec to be rejected: %#v", spec)
		}
	}
}

func TestClampToRunnerCaps(t *testing.T) {
	effective, err := ClampToRunnerCaps(&ParsedLimits{
		MemoryBytes:    RunnerMaxMemoryBytes * 2,
		CPUNano:        RunnerMaxCPUNano * 2,
		Timeout:        2 * RunnerMaxTimeout,
		DiskBytes:      RunnerMaxDiskBytes * 2,
		NetworkEnabled: true,
	}, false, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if effective.MemoryBytes != RunnerMaxMemoryBytes || effective.CPUNano != RunnerMaxCPUNano || effective.Timeout != RunnerMaxTimeout || effective.DiskBytes != RunnerMaxDiskBytes {
		t.Fatalf("runner caps were not enforced: %#v", effective)
	}
	if !effective.NetworkEnabled {
		t.Fatal("network request should be honored when the runner is not air-gapped")
	}
}

func TestClampToRunnerCapsForcesNetworkOffWhenAirGapped(t *testing.T) {
	effective, err := ClampToRunnerCaps(&ParsedLimits{
		MemoryBytes:    RunnerMaxMemoryBytes,
		CPUNano:        RunnerMaxCPUNano,
		Timeout:        RunnerMaxTimeout,
		NetworkEnabled: true,
	}, true, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if effective.NetworkEnabled {
		t.Fatal("air-gapped runner policy must override a challenge's network: enabled request")
	}
}

// Task #66: limits.gpu is a portable GPU count, clamped against the
// runner's resolved RUNNER_GPUS policy (#65) — but unlike memory/CPU/
// timeout/disk, an overage is refused outright rather than silently
// reduced. A degraded GPU run (or one silently dropped to zero) is not a
// smaller version of the same measurement: it is a different one, and
// scoring it as comparable would corrupt results (#26, #60).
func TestClampToRunnerCapsAllowsGPURequestWithinRunnerPolicy(t *testing.T) {
	effective, err := ClampToRunnerCaps(&ParsedLimits{
		CPUNano:  RunnerMaxCPUNano,
		GPUCount: 2,
	}, false, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if effective.GPUCount != 2 {
		t.Fatalf("expected the GPU request to be honored, got %d", effective.GPUCount)
	}
}

func TestClampToRunnerCapsRejectsGPURequestBeyondRunnerPolicy(t *testing.T) {
	_, err := ClampToRunnerCaps(&ParsedLimits{
		CPUNano:  RunnerMaxCPUNano,
		GPUCount: 1,
	}, false, 0)
	if err == nil {
		t.Fatal("expected a GPU request beyond the runner's RUNNER_GPUS policy to be rejected explicitly")
	}
	for _, want := range []string{"1", "RUNNER_GPUS"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected the error to mention %q, got: %v", want, err)
		}
	}
}

func TestClampToRunnerCapsRejectsGPURequestExceedingPartialRunnerPolicy(t *testing.T) {
	// Even a partial overage (runner exposes some GPUs, just not enough)
	// is refused, not silently reduced to what's available.
	_, err := ClampToRunnerCaps(&ParsedLimits{GPUCount: 4}, false, 2)
	if err == nil {
		t.Fatal("expected an over-subscribed GPU request to be rejected, not clamped down")
	}
}

func TestExtractSolutionRejectsAnchorsAndUnknownFields(t *testing.T) {
	anchored := writeTempYAML(t, `
version: "1.0"
type: solution
runtime: &runtime
  docker: "alpine:3.19"
run:
  script: "scripts/run.sh"
`)
	if _, err := ExtractSolutionFromFile(anchored); err == nil {
		t.Fatal("expected YAML anchor to be rejected")
	}

	unknown := writeTempYAML(t, `
version: "1.0"
type: solution
runtime:
  docker: "alpine:3.19"
run:
  script: "scripts/run.sh"
privileged: true
`)
	if _, err := ExtractSolutionFromFile(unknown); err == nil {
		t.Fatal("expected unknown solution field to be rejected")
	}
}

func TestChallengeLimitParsingRequiresStrictValues(t *testing.T) {
	if _, err := (&LimitsConfig{Memory: "2GB", CPU: "2.0", Timeout: "30s"}).Parse(); err != nil {
		t.Fatalf("expected valid limits: %v", err)
	}
	if _, err := (&LimitsConfig{Memory: "2GB garbage", CPU: "2.0", Timeout: "30s"}).Parse(); err == nil {
		t.Fatal("expected malformed size rejection")
	}
	if _, err := (&LimitsConfig{Memory: "2GB", CPU: "0", Timeout: "30s"}).Parse(); err == nil {
		t.Fatal("expected non-positive CPU rejection")
	}
	if _, err := (&LimitsConfig{Memory: "2GB", CPU: "2.0", Timeout: (-time.Second).String()}).Parse(); err == nil {
		t.Fatal("expected non-positive timeout rejection")
	}
	if _, err := (&LimitsConfig{Memory: "2GB", CPU: "2.0", Timeout: "30s", GPU: -1}).Parse(); err == nil {
		t.Fatal("expected negative gpu count rejection")
	}
	parsed, err := (&LimitsConfig{Memory: "2GB", CPU: "2.0", Timeout: "30s", GPU: 2}).Parse()
	if err != nil {
		t.Fatalf("expected a positive gpu count to parse: %v", err)
	}
	if parsed.GPUCount != 2 {
		t.Fatalf("expected GPUCount 2, got %d", parsed.GPUCount)
	}
	unset, err := (&LimitsConfig{Memory: "2GB", CPU: "2.0", Timeout: "30s"}).Parse()
	if err != nil {
		t.Fatalf("expected an omitted gpu field to parse: %v", err)
	}
	if unset.GPUCount != 0 {
		t.Fatalf("expected an omitted gpu field to default to 0, got %d", unset.GPUCount)
	}
}
