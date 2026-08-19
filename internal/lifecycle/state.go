package lifecycle

import (
	"sync"
	"time"
)

type Status string

const (
	StatusStarting    Status = "starting"
	StatusRegistering Status = "registering"
	StatusIdle        Status = "idle"
	StatusBusy        Status = "busy"
	StatusStopping    Status = "stopping"
	StatusOffline     Status = "offline"
)

type Snapshot struct {
	Status             Status     `json:"status"`
	Ready              bool       `json:"ready"`
	RunnerID           string     `json:"runner_id"`
	Version            string     `json:"version"`
	CurrentJobID       string     `json:"current_job_id,omitempty"`
	StartedAt          time.Time  `json:"started_at"`
	TransitionedAt     time.Time  `json:"transitioned_at"`
	LastHeartbeatAt    *time.Time `json:"last_heartbeat_at,omitempty"`
	LastHeartbeatError string     `json:"last_heartbeat_error,omitempty"`
}

type State struct {
	mu                 sync.RWMutex
	status             Status
	runnerID           string
	version            string
	currentJobID       string
	startedAt          time.Time
	transitionedAt     time.Time
	lastHeartbeatAt    *time.Time
	lastHeartbeatError string
}

func New(runnerID, runnerVersion string) *State {
	now := time.Now().UTC()
	return &State{
		status:         StatusStarting,
		runnerID:       runnerID,
		version:        runnerVersion,
		startedAt:      now,
		transitionedAt: now,
	}
}

func (s *State) SetRunnerID(runnerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runnerID = runnerID
}

func (s *State) SetStatus(status Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
	if status != StatusBusy {
		s.currentJobID = ""
	}
	s.transitionedAt = time.Now().UTC()
}

func (s *State) SetBusy(jobID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = StatusBusy
	s.currentJobID = jobID
	s.transitionedAt = time.Now().UTC()
}

func (s *State) RecordHeartbeat(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	s.lastHeartbeatAt = &now
	if err == nil {
		s.lastHeartbeatError = ""
		return
	}
	s.lastHeartbeatError = err.Error()
}

func (s *State) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Snapshot{
		Status:             s.status,
		Ready:              s.status == StatusIdle || s.status == StatusBusy,
		RunnerID:           s.runnerID,
		Version:            s.version,
		CurrentJobID:       s.currentJobID,
		StartedAt:          s.startedAt,
		TransitionedAt:     s.transitionedAt,
		LastHeartbeatAt:    s.lastHeartbeatAt,
		LastHeartbeatError: s.lastHeartbeatError,
	}
}
