package config

import "os"

type Config struct {
	NATSUrl    string
	RunnerID   string
	DockerHost string
	WorkDir    string
	Executor   string // "docker" or "process"
	UpdateURL  string // base URL for self-update binaries
	BackendURL string // backend HTTP URL (for device flow login)
}

func Load() *Config {
	return &Config{
		NATSUrl:    getEnv("NATS_URL", "nats://nats:4222"),
		RunnerID:   getEnv("RUNNER_ID", "runner-default"),
		DockerHost: getEnv("DOCKER_HOST", "unix:///var/run/docker.sock"),
		WorkDir:    getEnv("RUNNER_WORK_DIR", "/var/dragrace"),
		Executor:   getEnv("RUNNER_EXECUTOR", "docker"),
		UpdateURL:  getEnv("RUNNER_UPDATE_URL", ""),
		BackendURL: getEnv("BACKEND_URL", "http://localhost:3000"),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
