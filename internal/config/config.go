package config

import (
	"os"
	"strings"
)

type Config struct {
	WsBackendURL       string
	RunnerID           string
	DockerHost         string
	WorkDir            string
	Executor           string // "docker" or "process"
	UpdateURL          string // base URL for self-update binaries
	BackendURL         string // backend HTTP URL (for device flow login)
	HealthAddr         string // local health endpoint listen address
	AirGapped          bool   // forbid all sandbox network egress, regardless of challenge policy
	NativeRiskAccepted bool   // explicit operator opt-in for remote native execution
}

func Load() *Config {
	return &Config{
		WsBackendURL:       getEnv("WS_BACKEND_URL", "wss://ws.dragrace.dev"),
		RunnerID:           getEnv("RUNNER_ID", "runner-default"),
		DockerHost:         getEnv("DOCKER_HOST", "unix:///var/run/docker.sock"),
		WorkDir:            getEnv("RUNNER_WORK_DIR", "/var/dragrace"),
		Executor:           getEnv("RUNNER_EXECUTOR", "docker"),
		UpdateURL:          getEnv("RUNNER_UPDATE_URL", "https://github.com/dragrace-dev/runner/releases/latest/download"),
		BackendURL:         getEnv("BACKEND_URL", "https://dragrace.dev"),
		HealthAddr:         getEnv("RUNNER_HEALTH_ADDR", "127.0.0.1:8081"),
		AirGapped:          getEnvBool("RUNNER_AIRGAPPED", false),
		NativeRiskAccepted: getEnvBool("RUNNER_NATIVE_RISK_ACCEPTED", false),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value == "1" || strings.EqualFold(value, "true")
}
