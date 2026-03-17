package config

import "os"

type Config struct {
	WsBackendURL string
	RunnerID   string
	DockerHost string
	WorkDir    string
	Executor   string // "docker" or "process"
	UpdateURL  string // base URL for self-update binaries
	BackendURL string // backend HTTP URL (for device flow login)
}

func Load() *Config {
	return &Config{
		WsBackendURL: getEnv("WS_BACKEND_URL", "wss://ws.dragrace.dev"),
		RunnerID:   getEnv("RUNNER_ID", "runner-default"),
		DockerHost: getEnv("DOCKER_HOST", "unix:///var/run/docker.sock"),
		WorkDir:    getEnv("RUNNER_WORK_DIR", "/var/dragrace"),
		Executor:   getEnv("RUNNER_EXECUTOR", "docker"),
		UpdateURL:  getEnv("RUNNER_UPDATE_URL", "https://github.com/dragrace-dev/runner/releases/latest/download"),
		BackendURL: getEnv("BACKEND_URL", "https://dragrace.dev"),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
