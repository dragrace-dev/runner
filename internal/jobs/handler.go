package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"dragrace/internal/config"
	"dragrace/internal/docker"
	"dragrace/internal/executor"
	"dragrace/internal/git"
	"dragrace/internal/gpu"
	"dragrace/internal/lifecycle"
	"dragrace/internal/metrics"
	natsclient "dragrace/internal/nats"

	"github.com/nats-io/nats.go"
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
	TimeoutSeconds int `json:"timeout_seconds"`
	// Measurement protocol. Warmups are executed and reported but do not count
	// towards the score; the backend aggregates the scored iterations.
	WarmupRuns int    `json:"warmup_runs"`
	ScoredRuns int    `json:"scored_runs"`
	CreatedAt  string `json:"created_at"`
}

var commitSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// Validate rejects the former archive URL contract before any work starts.
// Solution ref is deliberately an immutable commit SHA; challenge refs may be
// branches because challenge definitions are managed by the platform.
func (j *JobMessage) Validate() error {
	if err := validateRepositoryURL(j.Challenge.Source.URL); err != nil {
		return fmt.Errorf("invalid challenge repository: %w", err)
	}
	if strings.TrimSpace(j.Challenge.Source.Ref) == "" {
		return fmt.Errorf("challenge ref is required")
	}
	if err := validateRepositoryURL(j.Solution.Source.URL); err != nil {
		return fmt.Errorf("invalid solution repository: %w", err)
	}
	if !commitSHA.MatchString(j.Solution.Source.Ref) {
		return fmt.Errorf("solution ref must be a full 40-character commit SHA")
	}
	return nil
}

func validateRepositoryURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("must be a plain HTTP(S) repository URL")
	}
	path := strings.ToLower(u.Path)
	if strings.Contains(path, "/archive/") || strings.HasSuffix(path, ".zip") || strings.HasSuffix(path, ".tar") || strings.HasSuffix(path, ".gz") {
		return fmt.Errorf("archive URLs are not supported")
	}
	return nil
}

type Handler struct {
	natsClient  *natsclient.Client
	executor    executor.Executor
	runnerID    string
	workDir     string
	airGapped   bool          // forbid sandbox network egress regardless of challenge policy
	gpuPolicy   gpu.Policy    // this runner's resolved RUNNER_GPUS policy (#65/#66); gpuPolicy.Count is the ceiling limits.gpu is clamped to, gpuPolicy.Allows scopes GPU metrics to allocated cards (#67)
	jobActivity chan struct{} // signalled when a job is received
	state       *lifecycle.State
	jobsMu      sync.Mutex
	jobsIdle    *sync.Cond
	activeJobs  int
	stopping    bool
}

const maxPhaseLogBytes = 64 * 1024

// runIteration is one execution of the run phase. The backend decides what to
// do with them; the runner only reports what it measured.
type runIteration struct {
	Index   int                 `json:"index"`
	Kind    string              `json:"kind"`
	Metrics *metrics.RunMetrics `json:"metrics"`
}

type jobExecutionResult struct {
	Metrics       *metrics.RunMetrics    `json:"-"`
	Iterations    []runIteration         `json:"-"`
	PhaseLogs     map[string]string      `json:"phase_logs"`
	Environment   map[string]interface{} `json:"environment"`
	LogsTruncated bool                   `json:"logs_truncated"`
}

func newJobExecutionResult(job *JobMessage, runnerID string, airGapped bool) *jobExecutionResult {
	return &jobExecutionResult{
		PhaseLogs: make(map[string]string),
		Environment: map[string]interface{}{
			"runner_id":       runnerID,
			"timeout_seconds": job.TimeoutSeconds,
			"air_gapped":      airGapped,
		},
	}
}

func truncatePhaseLog(value string) (string, bool) {
	if len(value) <= maxPhaseLogBytes {
		return value, false
	}

	const suffix = "\n… log truncated …\n"
	budget := maxPhaseLogBytes - len(suffix)
	var bounded strings.Builder
	bounded.Grow(maxPhaseLogBytes)
	used := 0
	for _, character := range value {
		size := utf8.RuneLen(character)
		if used+size > budget {
			break
		}
		bounded.WriteRune(character)
		used += size
	}
	bounded.WriteString(suffix)
	return bounded.String(), true
}

func (result *jobExecutionResult) addLog(phase, value string) {
	if value == "" {
		return
	}
	if existing := result.PhaseLogs[phase]; existing != "" {
		value = existing + "\n" + value
	}
	bounded, truncated := truncatePhaseLog(value)
	result.PhaseLogs[phase] = bounded
	result.LogsTruncated = result.LogsTruncated || truncated
}

func NewHandler(nc *natsclient.Client, exec executor.Executor, runnerID string, state *lifecycle.State, airGapped bool, gpuPolicy gpu.Policy) *Handler {
	workDir := os.Getenv("DRAGRACE_WORK_DIR")
	if workDir == "" {
		workDir = "/tmp/dragrace"
	}
	handler := &Handler{
		natsClient:  nc,
		executor:    exec,
		runnerID:    runnerID,
		workDir:     workDir,
		airGapped:   airGapped,
		gpuPolicy:   gpuPolicy,
		jobActivity: make(chan struct{}, 1),
		state:       state,
	}
	handler.jobsIdle = sync.NewCond(&handler.jobsMu)
	return handler
}

// JobActivity returns a channel that is signalled each time a job is received.
func (h *Handler) JobActivity() <-chan struct{} {
	return h.jobActivity
}

func (h *Handler) StopAndWait() {
	h.jobsMu.Lock()
	h.stopping = true
	for h.activeJobs > 0 {
		h.jobsIdle.Wait()
	}
	h.jobsMu.Unlock()
}

func (h *Handler) beginJob() bool {
	h.jobsMu.Lock()
	defer h.jobsMu.Unlock()
	if h.stopping {
		return false
	}
	h.activeJobs++
	return true
}

func (h *Handler) endJob() {
	h.jobsMu.Lock()
	h.activeJobs--
	if h.activeJobs == 0 {
		h.jobsIdle.Broadcast()
	}
	h.jobsMu.Unlock()
}

func (h *Handler) HandleJobSubmit(msg *nats.Msg) {
	if !h.beginJob() {
		log.Printf("⚠️  Ignoring job received during shutdown")
		return
	}
	defer h.endJob()

	var job JobMessage
	if err := json.Unmarshal(msg.Data, &job); err != nil {
		log.Printf("❌ Failed to parse job message: %v", err)
		h.sendJobFailed("", "", "", "Failed to parse job", nil)
		return
	}
	if job.RunnerID != "" && job.RunnerID != h.runnerID {
		log.Printf("⚠️  Ignoring job %s for runner %s", job.JobID, job.RunnerID)
		return
	}
	if err := job.Validate(); err != nil {
		log.Printf("❌ Invalid job %s: %v", job.JobID, err)
		h.sendJobFailed(job.JobID, job.RunID, job.AssignmentNonce, "Invalid job contract", nil)
		return
	}

	log.Printf("📥 Job received: %s", job.JobID)
	if h.state != nil {
		h.state.SetBusy(job.JobID)
		defer h.state.SetStatus(lifecycle.StatusIdle)
	}

	// Signal idle timer reset (non-blocking)
	select {
	case h.jobActivity <- struct{}{}:
	default:
	}

	h.sendJobStarted(job.JobID, job.RunID, job.AssignmentNonce)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(job.TimeoutSeconds)*time.Second)
	defer cancel()

	result, err := h.executeJob(ctx, &job)
	if err != nil {
		log.Printf("❌ Job %s failed: %v", job.JobID, err)
		result.addLog("system", err.Error())
		h.sendJobFailed(job.JobID, job.RunID, job.AssignmentNonce, err.Error(), result)
		return
	}

	h.sendJobCompleted(job.JobID, job.RunID, job.AssignmentNonce, result)
}

func (h *Handler) executeJob(ctx context.Context, job *JobMessage) (*jobExecutionResult, error) {
	log.Printf("🚀 Executing job %s...", job.JobID)
	result := newJobExecutionResult(job, h.runnerID, h.airGapped)

	jobDir := filepath.Join(h.workDir, "jobs", job.JobID)
	challengeDir := filepath.Join(jobDir, "challenge")
	solutionDir := filepath.Join(jobDir, "solution")

	// Cleanup job directory on completion
	defer os.RemoveAll(jobDir)

	// ─── 1. Data Dir (hash-based cache invalidation) ────────────────────
	volumeName := docker.VolumeName(job.Challenge.ID, job.Challenge.Source.Ref)
	volumeExists := h.executor.DataDirExists(ctx, volumeName)

	// ─── 2. Clone Challenge Repo ───────────────────────────────────────────
	// Repository URLs and refs may identify private source. They are consumed
	// only by git and never copied into operational logs.
	log.Printf("📦 Cloning challenge source for challenge %s", job.Challenge.ID)
	if err := git.Clone(job.Challenge.Source.URL, job.Challenge.Source.Ref, challengeDir); err != nil {
		return result, fmt.Errorf("failed to clone challenge repo: %w", err)
	}

	challengeSpec, err := h.loadChallengeSpec(challengeDir)
	if err != nil {
		return result, fmt.Errorf("failed to load challenge spec: %w", err)
	}
	log.Printf("📋 Challenge: %s", challengeSpec.Challenge.Name)

	// ─── 3. Init Phase (if data dir doesn't exist) ──────────────────────
	if !volumeExists && challengeSpec.Init != nil {
		log.Printf("🔄 Running INIT phase for challenge %s", job.Challenge.ID)

		if err := h.executor.EnsureDataDir(ctx, volumeName); err != nil {
			return result, fmt.Errorf("failed to create data dir: %w", err)
		}

		logs, err := h.executor.RunScript(ctx, &executor.RunOptions{
			Image:          challengeSpec.Init.Docker,
			ScriptPath:     challengeSpec.Init.Script,
			RepoDir:        challengeDir,
			DataDir:        volumeName,
			ReadOnlyData:   false, // Init needs write access
			NetworkEnabled: !h.airGapped,
			Trusted:        true, // organizer-controlled: may need root to install challenge deps
		})
		result.addLog("init", logs)
		if err != nil {
			log.Printf("❌ Init failed: %s", logs)
			h.executor.RemoveDataDir(ctx, volumeName)
			return result, fmt.Errorf("init phase failed: %w", err)
		}
		log.Printf("✅ INIT phase completed, data dir %s created", volumeName)
	} else if volumeExists {
		log.Printf("⏭️  Skipping INIT (data dir %s exists)", volumeName)
	}

	// ─── 4. Clone Solution Repo ────────────────────────────────────────────
	log.Printf("📦 Cloning submitted solution for job %s", job.JobID)
	if err := git.Clone(job.Solution.Source.URL, job.Solution.Source.Ref, solutionDir); err != nil {
		return result, fmt.Errorf("failed to clone solution repo: %w", err)
	}

	solutionSpec, err := h.loadSolutionSpec(solutionDir)
	if err != nil {
		return result, fmt.Errorf("failed to load solution spec: %w", err)
	}
	log.Printf("🔧 Solution runtime: %s", solutionSpec.Runtime.Docker)
	result.Environment["runtime_image"] = solutionSpec.Runtime.Docker

	// ─── 5. Build Phase ────────────────────────────────────────────────────
	if solutionSpec.Build != nil {
		log.Println("🔨 Running BUILD phase")
		logs, err := h.executor.RunScript(ctx, &executor.RunOptions{
			Image:          solutionSpec.Runtime.Docker,
			ScriptPath:     solutionSpec.Build.Script,
			RepoDir:        solutionDir,
			DataDir:        volumeName,
			ReadOnlyData:   true, // Build doesn't need to write data
			NetworkEnabled: !h.airGapped,
			// Trusted defaults to false: solution-controlled code gets the strict sandbox.
		})
		result.addLog("build", logs)
		if err != nil {
			log.Printf("❌ Build failed: %s", logs)
			return result, fmt.Errorf("build phase failed: %w", err)
		}
		log.Println("✅ BUILD phase completed")
	}

	// ─── 6. Run Phase (MEASURED) ───────────────────────────────────────────
	log.Println("🏃 Running RUN phase (measuring metrics)")

	parsedLimits, err := challengeSpec.Limits.Parse()
	if err != nil {
		return result, fmt.Errorf("invalid trusted challenge limits: %w", err)
	}
	parsedLimits, err = config.ClampToRunnerCaps(parsedLimits, h.airGapped, h.gpuPolicy.Count)
	if err != nil {
		return result, err
	}
	result.Environment["network_enabled"] = parsedLimits.NetworkEnabled
	result.Environment["resource_limits"] = map[string]interface{}{
		"memory_bytes": parsedLimits.MemoryBytes,
		"cpu_nano":     parsedLimits.CPUNano,
		"timeout_ms":   parsedLimits.Timeout.Milliseconds(),
	}
	// Warmups first (to fill caches, spin up JITs), then the scored runs the
	// backend aggregates. A challenge that never configured either still gets
	// exactly one scored run, as before.
	warmups := job.WarmupRuns
	if warmups < 0 {
		warmups = 0
	}
	scored := job.ScoredRuns
	if scored < 1 {
		scored = 1
	}
	log.Printf("🏃 Running RUN phase: %d warmup, %d scored", warmups, scored)

	runOptions := &executor.RunOptions{
		Image:          solutionSpec.Runtime.Docker,
		ScriptPath:     solutionSpec.Run.Script,
		RepoDir:        solutionDir,
		DataDir:        volumeName,
		ReadOnlyData:   true,
		Stdout:         solutionSpec.Run.Stdout,
		NetworkEnabled: parsedLimits.NetworkEnabled,
		Limits: &executor.ResourceLimits{
			MemoryBytes: parsedLimits.MemoryBytes,
			CPUNano:     parsedLimits.CPUNano,
			DiskBytes:   parsedLimits.DiskBytes,
			Timeout:     int(parsedLimits.Timeout.Seconds()),
		},
	}

	for index := 0; index < warmups+scored; index++ {
		kind := "warmup"
		if index >= warmups {
			kind = "scored"
		}

		// Each iteration gets its own timeout: the limit is per execution, not
		// for the whole measurement campaign.
		iterationCtx, cancelIteration := context.WithTimeout(ctx, parsedLimits.Timeout)

		// Sample the GPU alongside the run, but only when this runner's
		// RUNNER_GPUS policy actually grants the job container a GPU (#67).
		// Skipping the collector entirely when nothing is allocated is
		// deliberate, not just an optimisation: it is what guarantees a
		// GPU-less run reports no GPU aggregate at all, rather than
		// attributing to the job whatever activity the host's cards happen
		// to show, up to and including another job's usage of a different
		// card. The collector itself has no per-container view — nvidia-smi/
		// rocm-smi/ioreg run in the runner process, on the host — so even
		// when it does run, its samples are filtered below down to the cards
		// h.gpuPolicy actually exposed, never the raw host-wide reading.
		var gpuCollector *metrics.GPUCollector
		if h.gpuPolicy.Enabled() {
			gpuCollector = metrics.NewGPUCollector(100)
			gpuCollector.Start(iterationCtx)
		}

		iterationMetrics, runLogs, err := h.executor.RunMeasured(iterationCtx, runOptions)

		var gpuAggregates *metrics.GPUAggregates
		if gpuCollector != nil {
			gpuSeries := gpuCollector.Stop()
			gpuAggregates = metrics.AllocatedGPUAggregates(gpuSeries, func(vendor metrics.GPUVendor, deviceID int) bool {
				return h.gpuPolicy.Allows(string(vendor), deviceID)
			})
		}
		cancelIteration()

		if iterationMetrics != nil {
			iterationMetrics.GPUAggregates = gpuAggregates
		}

		// Only the last iteration's logs are kept, so the bound from #23 holds
		// however many iterations a challenge asks for.
		result.addLog("run", runLogs)
		if err != nil {
			// Any failure fails the run, warmup or scored: a partial series is
			// not comparable to a complete one.
			return result, fmt.Errorf("run phase failed on %s iteration %d: %w", kind, index, err)
		}

		result.Iterations = append(result.Iterations, runIteration{
			Index:   index,
			Kind:    kind,
			Metrics: iterationMetrics,
		})
		// Representative payload for the metrics that are not aggregated.
		if kind == "scored" && result.Metrics == nil {
			result.Metrics = iterationMetrics
		}
	}

	// ─── 7. Validate Phase ─────────────────────────────────────────────────
	if challengeSpec.Validate != nil {
		log.Println("🔍 Running VALIDATE phase")
		logs, err := h.executor.RunScript(ctx, &executor.RunOptions{
			Image:          challengeSpec.Validate.Docker,
			ScriptPath:     challengeSpec.Validate.Script,
			RepoDir:        challengeDir,
			DataDir:        volumeName,
			ReadOnlyData:   true,
			NetworkEnabled: !h.airGapped,
			Trusted:        true, // organizer-controlled validation tooling
		})
		result.addLog("validation", logs)
		if err != nil {
			log.Printf("❌ Validation failed: %s", logs)
			return result, fmt.Errorf("validation failed: %w", err)
		}
		log.Println("✅ Validation passed")
	}

	log.Printf("🎉 Job %s completed successfully", job.JobID)
	return result, nil
}

func (h *Handler) loadChallengeSpec(repoDir string) (*config.ChallengeSpec, error) {
	specPaths := []string{
		filepath.Join(repoDir, "dragrace.yaml"),
		filepath.Join(repoDir, "dragrace.yml"),
	}

	var err error
	for _, p := range specPaths {
		if _, err = os.Stat(p); err == nil {
			specData, readErr := os.ReadFile(p)
			if readErr != nil {
				return nil, fmt.Errorf("failed to read challenge spec: %w", readErr)
			}
			spec, parseErr := config.ParseChallengeSpec(specData)
			if parseErr != nil {
				return nil, fmt.Errorf("failed to parse challenge spec: %w", parseErr)
			}
			return spec, nil
		}
	}
	return nil, fmt.Errorf("challenge spec not found (tried dragrace.yaml, dragrace.yml)")
}

func (h *Handler) loadSolutionSpec(repoDir string) (*config.SolutionConfig, error) {
	specPaths := []string{
		filepath.Join(repoDir, "dragrace.yaml"),
		filepath.Join(repoDir, "dragrace.yml"),
		filepath.Join(repoDir, ".dragrace.yaml"),
		filepath.Join(repoDir, ".dragrace.yml"),
	}

	var err error
	for _, p := range specPaths {
		if _, err = os.Stat(p); err == nil {
			spec, parseErr := config.ParseSolutionConfig(p)
			if parseErr != nil {
				return nil, fmt.Errorf("failed to parse solution spec: %w", parseErr)
			}
			return spec, nil
		}
	}
	return nil, fmt.Errorf("solution spec not found")
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

func (h *Handler) sendJobCompleted(jobID, runID, nonce string, result *jobExecutionResult) {
	msg := map[string]interface{}{
		"job_id":           jobID,
		"run_id":           runID,
		"assignment_nonce": nonce,
		"status":           "completed",
		"metrics":          result.Metrics,
		"iterations":       result.Iterations,
		"output_logs":      result.PhaseLogs["run"],
		"phase_logs":       result.PhaseLogs,
		"environment":      result.Environment,
		"logs_truncated":   result.LogsTruncated,
		"completed_at":     time.Now().Format(time.RFC3339),
	}
	subject := fmt.Sprintf("dragrace.dev.backend.runner.%s.job.completed", h.runnerID)
	h.natsClient.Publish(subject, msg)
}

func (h *Handler) sendJobFailed(jobID, runID, nonce, errorMsg string, result *jobExecutionResult) {
	phaseLogs := map[string]string{}
	environment := map[string]interface{}{}
	logsTruncated := false
	if result != nil {
		phaseLogs = result.PhaseLogs
		environment = result.Environment
		logsTruncated = result.LogsTruncated
	}
	msg := map[string]interface{}{
		"job_id":           jobID,
		"run_id":           runID,
		"assignment_nonce": nonce,
		"status":           "failed",
		"error_message":    errorMsg,
		"error_logs":       phaseLogs["system"],
		"phase_logs":       phaseLogs,
		"environment":      environment,
		"logs_truncated":   logsTruncated,
		"failed_at":        time.Now().Format(time.RFC3339),
	}
	subject := fmt.Sprintf("dragrace.dev.backend.runner.%s.job.failed", h.runnerID)
	h.natsClient.Publish(subject, msg)
}
