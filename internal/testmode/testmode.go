package testmode

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"dragrace/internal/config"
	"dragrace/internal/docker"
	"dragrace/internal/executor"
	"dragrace/internal/process"
)

// Options configures a local test run.
type Options struct {
	ChallengeDir string            // path to the challenge repository
	SolutionDir  string            // path to the solution repository (optional)
	Executor     string            // "docker" or "process"
	Phases       []string          // phases to run (empty = all applicable)
	Env          map[string]string // extra environment variables
	Args         []string          // pass-through arguments for scripts
	NoCache      bool              // force re-run init
	Verbose      bool              // show full script logs
	DataDir      string            // data directory for init cache
}

// phase constants
const (
	PhaseInit     = "init"
	PhaseBuild    = "build"
	PhaseRun      = "run"
	PhaseValidate = "validate"
)

var allPhases = []string{PhaseInit, PhaseBuild, PhaseRun, PhaseValidate}

// ShouldRunPhase checks whether a given phase should be executed.
func ShouldRunPhase(phases []string, phase string) bool {
	if len(phases) == 0 {
		return true // no filter = run all
	}
	for _, p := range phases {
		if p == phase {
			return true
		}
	}
	return false
}

// Run executes a local test of a challenge (and optionally a solution).
func Run(opts *Options) error {
	// ── Header ──────────────────────────────────────────────────────────
	fmt.Println()
	fmt.Println("🏁 DragRace Test Mode")
	fmt.Println("══════════════════════════════════════════")

	// ── Resolve paths ───────────────────────────────────────────────────
	challengeDir, err := filepath.Abs(opts.ChallengeDir)
	if err != nil {
		return fmt.Errorf("invalid challenge path: %w", err)
	}

	// ── Validate challenge spec ─────────────────────────────────────────
	challengeSpec, err := ValidateChallengeDir(challengeDir)
	if err != nil {
		return fmt.Errorf("challenge validation failed: %w", err)
	}
	fmt.Printf("📋 Challenge: %s (%s)\n", challengeSpec.Challenge.Name, challengeSpec.Challenge.ID)

	// ── Validate solution spec (if provided) ────────────────────────────
	var solutionSpec *config.SolutionConfig
	var solutionDir string

	if opts.SolutionDir != "" {
		solutionDir, err = filepath.Abs(opts.SolutionDir)
		if err != nil {
			return fmt.Errorf("invalid solution path: %w", err)
		}

		solutionSpec, err = ValidateSolutionDir(solutionDir)
		if err != nil {
			return fmt.Errorf("solution validation failed: %w", err)
		}
		fmt.Printf("📋 Solution:  %s\n", filepath.Base(solutionDir))
	}

	fmt.Printf("🔧 Executor:  %s\n", opts.Executor)
	if len(opts.Env) > 0 {
		fmt.Printf("📦 Env vars:  %d\n", len(opts.Env))
	}
	if len(opts.Args) > 0 {
		fmt.Printf("📎 Args:      %v\n", opts.Args)
	}
	fmt.Println()

	// ── Check if solution is required ───────────────────────────────────
	needsSolution := ShouldRunPhase(opts.Phases, PhaseBuild) || ShouldRunPhase(opts.Phases, PhaseRun)
	if needsSolution && solutionSpec == nil {
		// Only error if explicitly requesting build/run, or if running all phases
		if len(opts.Phases) > 0 {
			return fmt.Errorf("--solution is required for phases: build, run")
		}
		// Running all phases without solution: only run init + validate
		log.Println("ℹ️  No --solution provided, running init and validate only")
	}

	// ── Initialize executor ─────────────────────────────────────────────
	var exec executor.Executor
	switch opts.Executor {
	case "process":
		exec, err = process.NewExecutor(opts.DataDir)
	case "docker":
		exec, err = docker.NewExecutor("")
	default:
		return fmt.Errorf("unknown executor: %s (use 'docker' or 'process')", opts.Executor)
	}
	if err != nil {
		return fmt.Errorf("failed to initialize %s executor: %w", opts.Executor, err)
	}
	defer exec.Close()

	// ── Data volume name ────────────────────────────────────────────────
	volumeName := fmt.Sprintf("dragrace-test-%s", challengeSpec.Challenge.ID)
	ctx := context.Background()

	// ── INIT phase ──────────────────────────────────────────────────────
	if ShouldRunPhase(opts.Phases, PhaseInit) && challengeSpec.Init != nil {
		fmt.Println("── INIT ─────────────────────────────────")

		// Check cache
		if !opts.NoCache && exec.DataDirExists(ctx, volumeName) {
			fmt.Println("  ⏭️  Skipping (cached). Use --no-cache to force.")
		} else {
			if opts.NoCache {
				exec.RemoveDataDir(ctx, volumeName)
			}

			if err := exec.EnsureDataDir(ctx, volumeName); err != nil {
				return fmt.Errorf("failed to create data dir: %w", err)
			}

			image := challengeSpec.Init.Docker
			if opts.Executor == "process" {
				image = "" // process executor ignores image
			}

			fmt.Printf("  📜 %s\n", challengeSpec.Init.Script)
			if image != "" {
				fmt.Printf("  🐳 %s\n", image)
			}

			start := time.Now()
			logs, err := exec.RunScript(ctx, &executor.RunOptions{
				Image:        image,
				ScriptPath:   challengeSpec.Init.Script,
				RepoDir:      challengeDir,
				DataDir:      volumeName,
				ReadOnlyData: false,
				Env:          opts.Env,
				Args:         opts.Args,
			})
			elapsed := time.Since(start)

			if err != nil {
				if opts.Verbose || true { // Always show logs on failure
					fmt.Printf("  📝 Logs:\n%s\n", logs)
				}
				exec.RemoveDataDir(ctx, volumeName)
				return fmt.Errorf("INIT failed: %w", err)
			}

			if opts.Verbose {
				fmt.Printf("  📝 Logs:\n%s\n", logs)
			}
			fmt.Printf("  ✅ completed (%.1fs)\n", elapsed.Seconds())
		}
		fmt.Println()
	}

	// ── BUILD phase ─────────────────────────────────────────────────────
	if ShouldRunPhase(opts.Phases, PhaseBuild) && solutionSpec != nil && solutionSpec.Build != nil {
		fmt.Println("── BUILD ────────────────────────────────")

		image := solutionSpec.Runtime.Docker
		if opts.Executor == "process" {
			image = ""
		}

		fmt.Printf("  📜 %s\n", solutionSpec.Build.Script)
		if image != "" {
			fmt.Printf("  🐳 %s\n", image)
		}

		start := time.Now()
		logs, err := exec.RunScript(ctx, &executor.RunOptions{
			Image:        image,
			ScriptPath:   solutionSpec.Build.Script,
			RepoDir:      solutionDir,
			DataDir:      volumeName,
			ReadOnlyData: true,
			Env:          opts.Env,
			Args:         opts.Args,
		})
		elapsed := time.Since(start)

		if err != nil {
			if opts.Verbose || true {
				fmt.Printf("  📝 Logs:\n%s\n", logs)
			}
			return fmt.Errorf("BUILD failed: %w", err)
		}

		if opts.Verbose {
			fmt.Printf("  📝 Logs:\n%s\n", logs)
		}
		fmt.Printf("  ✅ completed (%.1fs)\n", elapsed.Seconds())
		fmt.Println()
	}

	// ── RUN phase (measured) ────────────────────────────────────────────
	if ShouldRunPhase(opts.Phases, PhaseRun) && solutionSpec != nil {
		fmt.Println("── RUN (measured) ───────────────────────")

		image := solutionSpec.Runtime.Docker
		if opts.Executor == "process" {
			image = ""
		}

		fmt.Printf("  📜 %s\n", solutionSpec.Run.Script)
		if image != "" {
			fmt.Printf("  🐳 %s\n", image)
		}

		parsedLimits, err := challengeSpec.Limits.Parse()
		if err != nil {
			log.Printf("  ⚠️  Failed to parse limits, using defaults: %v", err)
			parsedLimits = &config.ParsedLimits{}
		}

		runMetrics, err := exec.RunMeasured(ctx, &executor.RunOptions{
			Image:        image,
			ScriptPath:   solutionSpec.Run.Script,
			RepoDir:      solutionDir,
			DataDir:      volumeName,
			ReadOnlyData: true,
			Stdout:       solutionSpec.Run.Stdout,
			Env:          opts.Env,
			Args:         opts.Args,
			Limits: &executor.ResourceLimits{
				MemoryBytes: parsedLimits.MemoryBytes,
				CPUNano:     parsedLimits.CPUShares * 1_000_000,
			},
		})
		if err != nil {
			return fmt.Errorf("RUN failed: %w", err)
		}

		fmt.Println("  ✅ completed")
		fmt.Printf("  📊 Time: %d ms | Mem peak: %.0f MB | CPU avg: %.1f%%\n",
			runMetrics.Aggregates.ExecutionTimeMs,
			runMetrics.Aggregates.MemoryPeakMB,
			runMetrics.Aggregates.CPUPercentAvg,
		)
		fmt.Println()
	}

	// ── VALIDATE phase ──────────────────────────────────────────────────
	if ShouldRunPhase(opts.Phases, PhaseValidate) && challengeSpec.Validate != nil {
		fmt.Println("── VALIDATE ─────────────────────────────")

		image := challengeSpec.Validate.Docker
		if opts.Executor == "process" {
			image = ""
		}

		fmt.Printf("  📜 %s\n", challengeSpec.Validate.Script)
		if image != "" {
			fmt.Printf("  🐳 %s\n", image)
		}

		// For validate, use the solution dir as repo if available (it has the output),
		// otherwise use the challenge dir.
		validateRepoDir := challengeDir
		if solutionDir != "" {
			validateRepoDir = solutionDir
		}

		logs, err := exec.RunScript(ctx, &executor.RunOptions{
			Image:        image,
			ScriptPath:   challengeSpec.Validate.Script,
			RepoDir:      validateRepoDir,
			DataDir:      volumeName,
			ReadOnlyData: true,
			Env:          opts.Env,
			Args:         opts.Args,
		})

		if err != nil {
			fmt.Println("  ❌ FAIL")
			if opts.Verbose || true {
				fmt.Printf("  📝 Logs:\n%s\n", logs)
			}
			return fmt.Errorf("VALIDATE failed: %w", err)
		}

		if opts.Verbose {
			fmt.Printf("  📝 Logs:\n%s\n", logs)
		}
		fmt.Println("  ✅ PASS")
		fmt.Println()
	}

	// ── Summary ─────────────────────────────────────────────────────────
	fmt.Println("══════════════════════════════════════════")
	fmt.Println("✅ All phases completed successfully")
	fmt.Println()

	return nil
}
