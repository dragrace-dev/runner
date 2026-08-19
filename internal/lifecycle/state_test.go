package lifecycle

import (
	"errors"
	"testing"
)

func TestStateTracksJobAndHeartbeatLifecycle(t *testing.T) {
	state := New("local-runner", "1.2.3")
	state.SetStatus(StatusRegistering)
	state.SetRunnerID("backend-runner")
	state.SetStatus(StatusIdle)
	state.RecordHeartbeat(nil)
	state.SetBusy("job-123")

	busy := state.Snapshot()
	if !busy.Ready || busy.Status != StatusBusy || busy.CurrentJobID != "job-123" {
		t.Fatalf("unexpected busy snapshot: %#v", busy)
	}
	if busy.RunnerID != "backend-runner" || busy.LastHeartbeatAt == nil {
		t.Fatalf("runner identity or heartbeat missing: %#v", busy)
	}

	state.RecordHeartbeat(errors.New("nats unavailable"))
	state.SetStatus(StatusOffline)
	offline := state.Snapshot()
	if offline.Ready || offline.CurrentJobID != "" {
		t.Fatalf("offline runner should not be ready or retain a job: %#v", offline)
	}
	if offline.LastHeartbeatError != "nats unavailable" {
		t.Fatalf("heartbeat error not retained: %#v", offline)
	}
}
