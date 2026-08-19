package jobs

import (
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestStopAndWaitRejectsNewJobsAfterActiveJobFinishes(t *testing.T) {
	handler := &Handler{}
	handler.jobsIdle = sync.NewCond(&handler.jobsMu)
	if !handler.beginJob() {
		t.Fatal("first job should be accepted")
	}

	stopped := make(chan struct{})
	go func() {
		handler.StopAndWait()
		close(stopped)
	}()

	deadline := time.Now().Add(time.Second)
	for {
		handler.jobsMu.Lock()
		stopping := handler.stopping
		handler.jobsMu.Unlock()
		if stopping {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shutdown did not enter stopping state")
		}
		runtime.Gosched()
	}
	select {
	case <-stopped:
		t.Fatal("shutdown returned while a job was active")
	default:
	}

	handler.endJob()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after the job ended")
	}
	if handler.beginJob() {
		t.Fatal("jobs must be rejected after shutdown begins")
	}
}
