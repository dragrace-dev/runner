package config

import "testing"

func TestLoadNativeRiskAcceptanceDefaultsClosed(t *testing.T) {
	t.Setenv("RUNNER_NATIVE_RISK_ACCEPTED", "")
	if Load().NativeRiskAccepted {
		t.Fatal("native risk acceptance must default to false")
	}
}

func TestLoadNativeRiskAcceptanceExplicit(t *testing.T) {
	t.Setenv("RUNNER_NATIVE_RISK_ACCEPTED", "true")
	if !Load().NativeRiskAccepted {
		t.Fatal("expected explicit native risk acceptance")
	}
}

func TestLoadGPUExposureDefaultsToNone(t *testing.T) {
	t.Setenv("RUNNER_GPUS", "")
	if got := Load().GPUs; got != "none" {
		t.Fatalf("GPU exposure must default to none, got %q", got)
	}
}

func TestLoadGPUExposureReadsEnv(t *testing.T) {
	t.Setenv("RUNNER_GPUS", "0,1")
	if got := Load().GPUs; got != "0,1" {
		t.Fatalf("expected the raw RUNNER_GPUS value, got %q", got)
	}
}
