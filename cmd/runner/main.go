package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"dragrace/internal/auth"
	"dragrace/internal/config"
	"dragrace/internal/docker"
	"dragrace/internal/executor"
	"dragrace/internal/gpu"
	"dragrace/internal/health"
	"dragrace/internal/jobs"
	"dragrace/internal/lifecycle"
	natsclient "dragrace/internal/nats"
	"dragrace/internal/process"
	"dragrace/internal/registration"
	"dragrace/internal/system"
	"dragrace/internal/testmode"
	"dragrace/internal/updater"
	"dragrace/internal/version"

	flag "github.com/spf13/pflag"
)

func main() {
	// Intercept subcommands that have their own flag sets BEFORE global flag.Parse().
	// This prevents conflicts (e.g. test's -c/--challenge vs global -c/--creds).
	if len(os.Args) > 1 && os.Args[1] == "test" {
		runTestMode()
		os.Exit(0)
	}

	// CLI flags (--long / -short)
	wsBackendURL := flag.StringP("ws-backend-url", "w", "", "WebSocket backend URL (overrides WS_BACKEND_URL env)")
	executorType := flag.StringP("executor", "e", "", "Executor type: docker or process (overrides RUNNER_EXECUTOR env)")
	runnerID := flag.StringP("runner-id", "r", "", "Runner ID (overrides RUNNER_ID env)")
	credsFile := flag.StringP("creds", "c", "", "Path to NATS credentials file")
	backendURL := flag.StringP("backend-url", "b", "", "Backend URL for device flow (overrides BACKEND_URL env)")
	idleTimeout := flag.IntP("idle-timeout", "i", 0, "Exit after N minutes of no job activity (0 = infinite)")
	healthAddr := flag.String("health-addr", "", "Local health endpoint listen address (overrides RUNNER_HEALTH_ADDR)")
	showVersion := flag.BoolP("version", "v", false, "Show version and exit")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "DragRace Runner v%s\n\n", version.Version)
		fmt.Fprintf(os.Stderr, "Usage: runner [options] [command]\n\n")
		fmt.Fprintf(os.Stderr, "Commands:\n")
		fmt.Fprintf(os.Stderr, "  test      Test a challenge locally (no account needed)\n")
		fmt.Fprintf(os.Stderr, "  login     Authenticate this runner (device code flow)\n")
		fmt.Fprintf(os.Stderr, "  update    Self-update to the latest version\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nEnvironment variables:\n")
		fmt.Fprintf(os.Stderr, "  WS_BACKEND_URL        WebSocket backend URL (default: wss://ws.dragrace.dev)\n")
		fmt.Fprintf(os.Stderr, "  RUNNER_EXECUTOR       Executor type: docker, process (default: docker)\n")
		fmt.Fprintf(os.Stderr, "  RUNNER_NATIVE_RISK_ACCEPTED  Required true for remote process executor\n")
		fmt.Fprintf(os.Stderr, "  RUNNER_ID             Runner identifier (default: runner-default)\n")
		fmt.Fprintf(os.Stderr, "  BACKEND_URL           Backend HTTP URL for login (default: https://dragrace.dev)\n")
		fmt.Fprintf(os.Stderr, "  DOCKER_HOST           Docker socket (default: unix:///var/run/docker.sock)\n")
		fmt.Fprintf(os.Stderr, "  RUNNER_WORK_DIR       Working directory (default: /var/dragrace)\n")
		fmt.Fprintf(os.Stderr, "  RUNNER_UPDATE_URL     Base URL for self-update binaries (default: GitHub releases)\n")
		fmt.Fprintf(os.Stderr, "  RUNNER_IDLE_TIMEOUT   Idle timeout in minutes (default: 0 = infinite)\n")
		fmt.Fprintf(os.Stderr, "  RUNNER_HEALTH_ADDR    Local health endpoint (default: 127.0.0.1:8081)\n")
		fmt.Fprintf(os.Stderr, "  RUNNER_AIRGAPPED      Forbid all sandbox network egress, overriding challenge policy (default: false)\n")
		fmt.Fprintf(os.Stderr, "  RUNNER_GPUS           GPUs exposed to job containers: none (default), all, or indexes like 0,1\n")
	}
	flag.Parse()

	// --version / -v
	if *showVersion {
		fmt.Printf("DragRace Runner v%s\n", version.Version)
		os.Exit(0)
	}

	// Load configuration
	cfg := config.Load()

	// CLI flags override env vars
	if *wsBackendURL != "" {
		cfg.WsBackendURL = *wsBackendURL
	}
	if *executorType != "" {
		cfg.Executor = *executorType
	}
	if *runnerID != "" {
		cfg.RunnerID = *runnerID
	}
	if *backendURL != "" {
		cfg.BackendURL = *backendURL
	}
	if *healthAddr != "" {
		cfg.HealthAddr = *healthAddr
	}

	// Handle subcommands
	if flag.NArg() > 0 {
		switch flag.Arg(0) {
		case "update":
			log.Printf("🔄 DragRace Runner %s — Self-Update", version.Version)
			if err := updater.Update(cfg.UpdateURL); err != nil {
				log.Fatalf("❌ Update failed: %v", err)
			}
			os.Exit(0)

		case "login":
			log.Printf("🏁 DragRace Runner v%s — Login", version.Version)
			if err := auth.Login(cfg.BackendURL); err != nil {
				log.Fatalf("❌ Login failed: %v", err)
			}
			os.Exit(0)

		case "test":
			runTestMode()
			os.Exit(0)
		}
	}

	log.Printf("🏁 DragRace Runner v%s", version.Version)
	log.Println("==========================================")

	// Resolve credentials file
	resolvedCreds := auth.ResolveCredsFile(*credsFile)
	if resolvedCreds == "" {
		log.Println("❌ No credentials found.")
		log.Println("   Run 'runner login' to authenticate, or provide --creds <path>")
		os.Exit(1)
	}
	log.Printf("🔑 Using credentials: %s", resolvedCreds)

	log.Printf("Runner ID: %s", cfg.RunnerID)
	log.Printf("WS Backend URL: %s", cfg.WsBackendURL)
	log.Printf("Executor: %s", cfg.Executor)
	if cfg.Executor == "process" && !cfg.NativeRiskAccepted {
		log.Fatal("🛑 Native process execution requires RUNNER_NATIVE_RISK_ACCEPTED=true. It runs untrusted code on the host without container isolation.")
	}
	if cfg.AirGapped {
		log.Println("🔒 Air-gapped mode: sandbox network egress is forbidden for all phases")
	}

	runnerState := lifecycle.New(cfg.RunnerID, version.Version)
	runnerState.SetStatus(lifecycle.StatusRegistering)
	healthServer := health.NewServer(cfg.HealthAddr, runnerState)
	if err := healthServer.Start(); err != nil {
		log.Fatalf("❌ Failed to start local health endpoint: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = healthServer.Shutdown(ctx)
	}()
	log.Printf("🩺 Local health endpoint: http://%s/healthz", cfg.HealthAddr)

	// Initialize NATS client with .creds authentication
	nc, err := natsclient.NewClient(cfg, resolvedCreds)
	if err != nil {
		log.Fatalf("❌ Failed to connect to NATS: %v", err)
	}
	defer func() {
		if nc != nil {
			nc.Close()
		}
	}()

	// Collect hardware info (local — no backend needed)
	log.Println("🔍 Collecting hardware configuration...")
	hwInfo, err := system.CollectHardwareInfo()
	if err != nil {
		log.Fatalf("❌ Failed to collect hardware info: %v", err)
	}

	log.Printf("📊 Hardware: %s (%d cores, %.1f GB RAM)", hwInfo.CPUModel, hwInfo.CPUCores, hwInfo.MemoryTotalGB)
	log.Printf("🔑 Fingerprint: %s", hwInfo.Fingerprint[:16]+"...")

	// Resolve the operator's GPU exposure ceiling against the GPUs actually
	// detected above (#65). This happens before registration on purpose: a
	// runner told to expose a GPU it does not have must die here, with a
	// clear message, rather than accept jobs it cannot honour.
	if gpu.Requested(cfg.GPUs) && cfg.Executor != "docker" {
		log.Fatalf("🛑 RUNNER_GPUS is only meaningful with the docker executor; the %q executor runs jobs directly on the host, where they already see every GPU. Set RUNNER_GPUS=none.", cfg.Executor)
	}
	gpuPolicy, err := gpu.Resolve(cfg.GPUs, hwInfo.GPUs)
	if err != nil {
		log.Fatalf("🛑 %v", err)
	}
	log.Printf("🎛️  GPU exposure: %s", gpuPolicy.Describe())

	// Register with retry (backend may not be ready yet)
	var regResult *registration.RegisterResponse
	maxRetries := 30
	retryDelay := 2 * time.Second

	for attempt := 1; attempt <= maxRetries; attempt++ {
		regResult, err = registration.Register(nc.Conn(), cfg, hwInfo)
		if err != nil {
			log.Printf("⏳ [%d/%d] Registration failed, retrying... (%v)", attempt, maxRetries, err)
			time.Sleep(retryDelay)
			if retryDelay < 15*time.Second {
				retryDelay = retryDelay * 3 / 2
			}
			continue
		}
		break
	}

	if regResult == nil {
		log.Fatal("❌ Failed to register after all retries. Is the backend running?")
	}

	if regResult.RunnerID != "" {
		log.Printf("📌 Backend runner ID: %s", regResult.RunnerID)
	}
	backendRunnerID := regResult.RunnerID
	if backendRunnerID == "" {
		backendRunnerID = cfg.RunnerID
	}
	runnerState.SetRunnerID(backendRunnerID)

	if regResult.Credentials != "" {
		if err := os.MkdirAll(filepath.Dir(resolvedCreds), 0700); err != nil {
			log.Fatalf("❌ Failed to prepare credentials directory: %v", err)
		}
		if err := os.WriteFile(resolvedCreds, []byte(regResult.Credentials), 0600); err != nil {
			log.Fatalf("❌ Failed to write refreshed credentials: %v", err)
		}
		nc.Close()
		nc, err = natsclient.NewClient(cfg, resolvedCreds)
		if err != nil {
			log.Fatalf("❌ Failed to reconnect with runner-scoped credentials: %v", err)
		}
		log.Println("🔐 Switched to runner-scoped NATS credentials")
	}

	// ── Version Check ──────────────────────────────────────────────────
	switch regResult.VersionStatus {
	case "incompatible":
		log.Fatalf("🛑 Runner version %s is no longer compatible (minimum: %s). Please update: ./runner update",
			version.Version, regResult.MinVersion)
	case "update_available":
		log.Printf("⚠️  New version available: %s (current: %s). Run \"./runner update\" to upgrade.",
			regResult.LatestVersion, version.Version)
	case "ok", "":
		// All good — no action needed
	}

	// Initialize executor based on config
	var exec executor.Executor
	switch cfg.Executor {
	case "process":
		log.Println("⚠️  Process executor enabled: this mode is less isolated than Docker and should be used only when your org accepts the risk.")
		exec, err = process.NewExecutor(cfg.WorkDir)
		if err != nil {
			log.Fatalf("❌ Failed to initialize process executor: %v", err)
		}
	case "docker":
		exec, err = docker.NewExecutor(cfg.DockerHost, gpuPolicy)
		if err != nil {
			log.Fatalf("❌ Failed to initialize Docker executor: %v", err)
		}
	default:
		log.Fatalf("❌ Unknown executor type: %s (use 'docker' or 'process')", cfg.Executor)
	}
	defer exec.Close()

	// Initialize job handler. gpuPolicy.Count is the ceiling a challenge's
	// declared limits.gpu is checked against (#66): the number of GPUs
	// RUNNER_GPUS actually resolved to on this host, not simply "some GPU
	// exists". The handler also uses gpuPolicy.Allows to scope GPU metrics
	// to the cards a job's container was actually granted (#67).
	handler := jobs.NewHandler(nc, exec, backendRunnerID, runnerState, cfg.AirGapped, gpuPolicy)

	// Subscribe to job submit topic
	jobSubject := fmt.Sprintf("dragrace.dev.runner.%s.job.submit", backendRunnerID)
	jobSubscription, err := nc.Subscribe(jobSubject, handler.HandleJobSubmit)
	if err != nil {
		log.Fatalf("❌ Failed to subscribe to jobs: %v", err)
	}

	log.Printf("✅ Subscribed to job submit topic: %s", jobSubject)
	log.Println("⏳ Waiting for jobs...")
	runnerState.SetStatus(lifecycle.StatusIdle)

	sendHeartbeat := func() {
		snapshot := runnerState.Snapshot()
		status := "idle"
		var currentJobID *string
		if snapshot.Status == lifecycle.StatusBusy {
			status = "busy"
			jobID := snapshot.CurrentJobID
			currentJobID = &jobID
		} else if snapshot.Status == lifecycle.StatusStopping || snapshot.Status == lifecycle.StatusOffline {
			status = "offline"
		}
		err := nc.SendHeartbeat(backendRunnerID, status, currentJobID)
		runnerState.RecordHeartbeat(err)
		if err != nil {
			log.Printf("⚠️  Failed to send heartbeat: %v", err)
		}
	}

	// Send initial heartbeat
	sendHeartbeat()
	if err := nc.SendRunnerConfig(backendRunnerID, hwInfo); err != nil {
		log.Printf("⚠️  Failed to send runner config: %v", err)
	}

	// Start heartbeat ticker
	ticker := time.NewTicker(30 * time.Second)
	heartbeatDone := make(chan struct{})
	heartbeatStopped := make(chan struct{})

	go func() {
		defer close(heartbeatStopped)
		for {
			select {
			case <-ticker.C:
				sendHeartbeat()
			case <-heartbeatDone:
				return
			}
		}
	}()

	// Wait for interrupt signal or idle timeout
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	if *idleTimeout > 0 {
		log.Printf("⏱️  Idle timeout: %d minutes", *idleTimeout)
		idleDuration := time.Duration(*idleTimeout) * time.Minute
		idleTimer := time.NewTimer(idleDuration)
		defer idleTimer.Stop()

		// Reset timer whenever a job arrives
		go func() {
			for range handler.JobActivity() {
				idleTimer.Reset(idleDuration)
			}
		}()

		select {
		case <-sigChan:
			log.Println("\n👋 Shutting down gracefully...")
		case <-idleTimer.C:
			log.Printf("💤 No jobs for %d minutes — shutting down.", *idleTimeout)
		}
	} else {
		<-sigChan
		log.Println("\n👋 Shutting down gracefully...")
	}

	if err := jobSubscription.Unsubscribe(); err != nil {
		log.Printf("⚠️  Failed to unsubscribe from jobs: %v", err)
	}
	handler.StopAndWait()
	runnerState.SetStatus(lifecycle.StatusStopping)
	ticker.Stop()
	close(heartbeatDone)
	<-heartbeatStopped

	// Send goodbye heartbeat so backend knows we're offline immediately
	runnerState.SetStatus(lifecycle.StatusOffline)
	if err := nc.SendHeartbeat(backendRunnerID, "offline", nil); err != nil {
		runnerState.RecordHeartbeat(err)
		log.Printf("⚠️  Failed to send goodbye heartbeat: %v", err)
	} else {
		runnerState.RecordHeartbeat(nil)
		log.Println("📤 Goodbye heartbeat sent")
	}
}

// runTestMode handles the "runner test" subcommand with its own flag set.
func runTestMode() {
	testFlags := flag.NewFlagSet("test", flag.ExitOnError)

	challenge := testFlags.StringP("challenge", "c", "", "Path to challenge directory (required)")
	solution := testFlags.StringP("solution", "s", "", "Path to solution directory")
	execType := testFlags.String("executor", "docker", "Executor: docker or process")
	phase := testFlags.StringP("phase", "p", "", "Run a single phase: init, build, run, validate")
	envVars := testFlags.StringArrayP("env", "E", nil, "Environment variable (KEY=VALUE), repeatable")
	noCache := testFlags.Bool("no-cache", false, "Force re-run of init (ignore cache)")
	verbose := testFlags.Bool("verbose", false, "Show full script logs")
	dataDir := testFlags.String("data-dir", "/tmp/dragrace-test", "Data directory for init cache")

	testFlags.Usage = func() {
		fmt.Fprintf(os.Stderr, "DragRace Runner v%s — Test Mode\n\n", version.Version)
		fmt.Fprintf(os.Stderr, "Test a challenge locally without an account.\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  runner test --challenge <path> [--solution <path>] [options] [-- args...]\n\n")
		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "  runner test -c ./challenges/1brc -s ./solutions/baseline\n")
		fmt.Fprintf(os.Stderr, "  runner test -c ./my-challenge                              # unified file\n")
		fmt.Fprintf(os.Stderr, "  runner test -c ./challenges/1brc --executor process -E ROW_COUNT=1000\n")
		fmt.Fprintf(os.Stderr, "  runner test -c ./challenges/1brc --phase init --no-cache\n")
		fmt.Fprintf(os.Stderr, "  runner test -c ./challenges/1brc -s ./sol -- --small\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		testFlags.PrintDefaults()
	}

	// Parse everything after "test" — pflag handles "--" separation
	testFlags.Parse(os.Args[2:]) // skip "runner" and "test"

	// Everything after -- is pass-through args
	passArgs := testFlags.Args()

	if *challenge == "" {
		testFlags.Usage()
		fmt.Fprintf(os.Stderr, "\n❌ --challenge is required\n")
		os.Exit(1)
	}

	// Parse phases
	var phases []string
	if *phase != "" {
		phases = []string{*phase}
	}

	// Parse env vars
	env := make(map[string]string)
	for _, e := range *envVars {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) != 2 {
			log.Fatalf("❌ Invalid env var format: %s (expected KEY=VALUE)", e)
		}
		env[parts[0]] = parts[1]
	}

	opts := &testmode.Options{
		ChallengeDir: *challenge,
		SolutionDir:  *solution,
		Executor:     *execType,
		Phases:       phases,
		Env:          env,
		Args:         passArgs,
		NoCache:      *noCache,
		Verbose:      *verbose,
		DataDir:      *dataDir,
	}

	if err := testmode.Run(opts); err != nil {
		log.Fatalf("❌ Test failed: %v", err)
	}
}
