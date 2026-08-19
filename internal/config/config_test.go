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
