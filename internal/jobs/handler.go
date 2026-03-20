package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"dragrace/internal/config"
	"dragrace/internal/docker"
	"dragrace/internal/executor"
	"dragrace/internal/git"
	"dragrace/internal/metrics"
	natsclient "dragrace/internal/nats"

	"github.com/nats-io/nats.go"
	"gopkg.in/yaml.v3"
)

// GitSource represents a Git repository reference
type GitSource struct {
	URL string `json:"url"`
	Ref string `json:"ref"`
}

// JobMessage is the message received from backend via NATS
type JobMessage struct {
	JobID           string `json:"job_id"`
	RunID           string `json:"run_id"`
	RunnerID        string `json:"runner_id,omitempty"`
	AssignmentNonce string `json:"assignment_nonce,omitempty"`
	Challenge       struct {
		ID     string    `json:"id"`
		Source GitSource `json:"source"`
	} `json:"challenge"`
	Solution struct {
		Source GitSource `json:"source"`
	} `json:"solution"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	CreatedAt      string `json:"created_at"`
}

type Handler struct {
	natsClient  *natsclient.Client
	executor    executor.Executor
	runnerID    string
	workDir     string
	jobActivity chan struct{} // signalled when a job is received
}

func NewHandler(nc *natsclient.Client, exec executor.Executor, runnerID string) *Handler {
	workDir := os.Getenv("DRAGRACE_WORK_DIR")
	if workDir == "" {
		workDir = "/tmp/dragrace"
	}
	return &Handler{
		natsClient:  nc,
		executor:    exec,
		runnerID:    runnerID,
		workDir:     workDir,
		jobActivity: make(chan struct{}, 1),
	}
}

// JobActivity returns a channel that is signalled each time a job is received.
func (h *Handler) JobActivity() <-chan struct{} {
	return h.jobActivity
}

func (h *Handler) HandleJobSubmit(msg *nats.Msg) {
	var job JobMessage
	if err := json.Unmarshal(msg.Data, &job); err != nil {
		log.Printf("❌ Failed to parse job message: %v", err)
		h.sendJobFailed("", "", "", "Failed to parse job", err.Error())
		return
	}
	if job.RunnerID != "" && job.RunnerID != h.runnerID {
		log.Printf("⚠️  Ignoring job %s for runner %s", job.JobID, job.RunnerID)
		return
	}

	log.Printf("📥 Job received: %s", job.JobID)

	// Signal idle timer reset (non-blocking)
	select {
	case h.jobActivity <- struct{}{}:
	default:
	}

	h.sendJobStarted(job.JobID, job.RunID, job.AssignmentNonce)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(job.TimeoutSeconds)*time.Second)
	defer cancel()

	runMetrics, err := h.executeJob(ctx, &job)
	if err != nil {
		log.Printf("❌ Job %s failed: %v", job.JobID, err)
		h.sendJobFailed(job.JobID, job.RunID, job.AssignmentNonce, "Job failed", err.Error())
		return
	}

	h.sendJobCompleted(job.JobID, job.RunID, job.AssignmentNonce, runMetrics)
}

func (h *Handler) executeJob(ctx context.Context, job *JobMessage) (*metrics.RunMetrics, error) {
	log.Printf("🚀 Executing job %s...", job.JobID)

	jobDir := filepath.Join(h.workDir, "jobs", job.JobID)
	challengeDir := filepath.Join(jobDir, "challenge")
	solutionDir := filepath.Join(jobDir, "solution")

	// Cleanup job directory on completion
	defer os.RemoveAll(jobDir)

	// ─── 1. Data Dir (hash-based cache invalidation) ────────────────────
	volumeName := docker.VolumeName(job.Challenge.ID, job.Challenge.Source.Ref)
	volumeExists := h.executor.DataDirExists(ctx, volumeName)

	// ─── 2. Clone Challenge Repo ───────────────────────────────────────────
	log.Printf("📦 Cloning challenge repo: %s @ %s", job.Challenge.Source.URL, job.Challenge.Source.Ref)
	if err := git.Clone(job.Challenge.Source.URL, job.Challenge.Source.Ref, challengeDir); err != nil {
		return nil, fmt.Errorf("failed to clone challenge repo: %w", err)
	}

	challengeSpec, err := h.loadChallengeSpec(challengeDir)
	if err != nil {
		return nil, fmt.Errorf("failed to load challenge spec: %w", err)
	}
	log.Printf("📋 Challenge: %s", challengeSpec.Challenge.Name)

	// ─── 3. Init Phase (if data dir doesn't exist) ──────────────────────
	if !volumeExists && challengeSpec.Init != nil {
		log.Printf("🔄 Running INIT phase (first time for %s @ %s)", job.Challenge.ID, job.Challenge.Source.Ref)

		if err := h.executor.EnsureDataDir(ctx, volumeName); err != nil {
			return nil, fmt.Errorf("failed to create data dir: %w", err)
		}

		logs, err := h.executor.RunScript(ctx, &executor.RunOptions{
			Image:        challengeSpec.Init.Docker,
			ScriptPath:   challengeSpec.Init.Script,
			RepoDir:      challengeDir,
			DataDir:      volumeName,
			ReadOnlyData: false, // Init needs write access
		})
		if err != nil {
			log.Printf("❌ Init failed: %s", logs)
			h.executor.RemoveDataDir(ctx, volumeName)
			return nil, fmt.Errorf("init phase failed: %w", err)
		}
		log.Printf("✅ INIT phase completed, data dir %s created", volumeName)
	} else if volumeExists {
		log.Printf("⏭️  Skipping INIT (data dir %s exists)", volumeName)
	}

	// ─── 4. Clone Solution Repo ────────────────────────────────────────────
	log.Printf("📦 Cloning solution repo: %s @ %s", job.Solution.Source.URL, job.Solution.Source.Ref)
	if err := git.Clone(job.Solution.Source.URL, job.Solution.Source.Ref, solutionDir); err != nil {
		return nil, fmt.Errorf("failed to clone solution repo: %w", err)
	}

	solutionSpec, err := h.loadSolutionSpec(solutionDir)
	if err != nil {
		return nil, fmt.Errorf("failed to load solution spec: %w", err)
	}
	log.Printf("🔧 Solution runtime: %s", solutionSpec.Runtime.Docker)

	// ─── 5. Build Phase ────────────────────────────────────────────────────
	if solutionSpec.Build != nil {
		log.Println("🔨 Running BUILD phase")
		logs, err := h.executor.RunScript(ctx, &executor.RunOptions{
			Image:        solutionSpec.Runtime.Docker,
			ScriptPath:   solutionSpec.Build.Script,
			RepoDir:      solutionDir,
			DataDir:      volumeName,
			ReadOnlyData: true, // Build doesn't need to write data
		})
		if err != nil {
			log.Printf("❌ Build failed: %s", logs)
			return nil, fmt.Errorf("build phase failed: %w", err)
		}
		log.Println("✅ BUILD phase completed")
	}

	// ─── 6. Run Phase (MEASURED) ───────────────────────────────────────────
	log.Println("🏃 Running RUN phase (measuring metrics)")

	parsedLimits, err := challengeSpec.Limits.Parse()
	if err != nil {
		log.Printf("⚠️  Failed to parse limits, using defaults: %v", err)
		parsedLimits = &config.ParsedLimits{}
	}

	runMetrics, err := h.executor.RunMeasured(ctx, &executor.RunOptions{
		Image:        solutionSpec.Runtime.Docker,
		ScriptPath:   solutionSpec.Run.Script,
		RepoDir:      solutionDir,
		DataDir:      volumeName,
		ReadOnlyData: true,
		Stdout:       solutionSpec.Run.Stdout,
		Limits: &executor.ResourceLimits{
			MemoryBytes: parsedLimits.MemoryBytes,
			CPUNano:     parsedLimits.CPUShares * 1000000,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("run phase failed: %w", err)
	}

	// ─── 7. Validate Phase ─────────────────────────────────────────────────
	if challengeSpec.Validate != nil {
		log.Println("🔍 Running VALIDATE phase")
		logs, err := h.executor.RunScript(ctx, &executor.RunOptions{
			Image:        challengeSpec.Validate.Docker,
			ScriptPath:   challengeSpec.Validate.Script,
			RepoDir:      challengeDir,
			DataDir:      volumeName,
			ReadOnlyData: true,
		})
		if err != nil {
			log.Printf("❌ Validation failed: %s", logs)
			return nil, fmt.Errorf("validation failed: %w", err)
		}
		log.Println("✅ Validation passed")
	}

	log.Printf("🎉 Job %s completed successfully", job.JobID)
	return runMetrics, nil
}

func (h *Handler) loadChallengeSpec(repoDir string) (*config.ChallengeSpec, error) {
	specPaths := []string{
		filepath.Join(repoDir, "dragrace.yaml"),
		filepath.Join(repoDir, "dragrace.yml"),
	}

	var specData []byte
	var err error
	for _, p := range specPaths {
		specData, err = os.ReadFile(p)
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("challenge spec not found (tried dragrace.yaml, dragrace.yml)")
	}

	var spec config.ChallengeSpec
	if err := yaml.Unmarshal(specData, &spec); err != nil {
		return nil, fmt.Errorf("failed to parse challenge spec: %w", err)
	}

	return &spec, nil
}

func (h *Handler) loadSolutionSpec(repoDir string) (*config.SolutionConfig, error) {
	specPaths := []string{
		filepath.Join(repoDir, "dragrace.yaml"),
		filepath.Join(repoDir, "dragrace.yml"),
		filepath.Join(repoDir, ".dragrace.yaml"),
		filepath.Join(repoDir, ".dragrace.yml"),
	}

	var specData []byte
	var err error
	for _, p := range specPaths {
		specData, err = os.ReadFile(p)
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("solution spec not found")
	}

	var spec config.SolutionConfig
	if err := yaml.Unmarshal(specData, &spec); err != nil {
		return nil, fmt.Errorf("failed to parse solution spec: %w", err)
	}

	return &spec, nil
}

func (h *Handler) sendJobStarted(jobID, runID, nonce string) {
	msg := map[string]interface{}{
		"job_id":           jobID,
		"run_id":           runID,
		"assignment_nonce": nonce,
		"started_at":       time.Now().Format(time.RFC3339),
	}
	subject := fmt.Sprintf("dragrace.dev.backend.runner.%s.job.started", h.runnerID)
	h.natsClient.Publish(subject, msg)
}

func (h *Handler) sendJobCompleted(jobID, runID, nonce string, runMetrics *metrics.RunMetrics) {
	msg := map[string]interface{}{
		"job_id":           jobID,
		"run_id":           runID,
		"assignment_nonce": nonce,
		"status":           "completed",
		"metrics":          runMetrics,
		"completed_at":     time.Now().Format(time.RFC3339),
	}
	subject := fmt.Sprintf("dragrace.dev.backend.runner.%s.job.completed", h.runnerID)
	h.natsClient.Publish(subject, msg)
}

func (h *Handler) sendJobFailed(jobID, runID, nonce, errorMsg, errorLogs string) {
	msg := map[string]interface{}{
		"job_id":           jobID,
		"run_id":           runID,
		"assignment_nonce": nonce,
		"status":           "failed",
		"error_message":    errorMsg,
		"error_logs":       errorLogs,
		"failed_at":        time.Now().Format(time.RFC3339),
	}
	subject := fmt.Sprintf("dragrace.dev.backend.runner.%s.job.failed", h.runnerID)
	h.natsClient.Publish(subject, msg)
}
