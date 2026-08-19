package health

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"dragrace/internal/lifecycle"
)

func TestHealthReflectsRunnerReadiness(t *testing.T) {
	state := lifecycle.New("runner-1", "1.0.0")
	server := NewServer("127.0.0.1:0", state)

	starting := httptest.NewRecorder()
	server.handleHealth(starting, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if starting.Code != http.StatusServiceUnavailable {
		t.Fatalf("starting status = %d, want 503", starting.Code)
	}

	state.SetStatus(lifecycle.StatusIdle)
	ready := httptest.NewRecorder()
	server.handleHealth(ready, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if ready.Code != http.StatusOK {
		t.Fatalf("idle status = %d, want 200", ready.Code)
	}
}
