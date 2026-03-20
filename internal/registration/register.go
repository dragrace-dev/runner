package registration

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"dragrace/internal/auth"
	"dragrace/internal/config"
	"dragrace/internal/system"
	"dragrace/internal/version"

	"github.com/nats-io/nats.go"
)

// RegisterResponse is the response from the backend registration endpoint
type RegisterResponse struct {
	RunnerID      string `json:"runner_id"`
	Status        string `json:"status"`                   // "registered"
	Credentials   string `json:"credentials,omitempty"`    // optional runner-scoped NATS creds
	VersionStatus string `json:"version_status,omitempty"` // "ok", "update_available", "incompatible"
	LatestVersion string `json:"latest_version,omitempty"`
	MinVersion    string `json:"min_version,omitempty"`
	Error         string `json:"error,omitempty"`
}

// RegisterRequest is the registration payload sent to the backend.
// No scope — the runner just announces itself as available.
// Scopes are assigned by admins via the backend API.
type RegisterRequest struct {
	Fingerprint   string                 `json:"fingerprint"`
	Hostname      string                 `json:"hostname,omitempty"`
	HardwareInfo  map[string]interface{} `json:"hardware_info,omitempty"`
	RunnerVersion string                 `json:"runner_version,omitempty"`
}

// Register sends a registration message to the backend and returns the result.
// The runner registers as "available" — scope assignment is the backend's job.
func Register(nc *nats.Conn, cfg *config.Config, hwInfo *system.HardwareInfo) (*RegisterResponse, error) {
	log.Println("📝 Registering runner with backend...")

	req := RegisterRequest{
		Fingerprint:   hwInfo.Fingerprint,
		Hostname:      hwInfo.Hostname,
		RunnerVersion: version.Version,
	}

	// Convert hardware info to a map
	hwBytes, _ := json.Marshal(hwInfo)
	var hwMap map[string]interface{}
	json.Unmarshal(hwBytes, &hwMap)
	req.HardwareInfo = hwMap

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal registration: %w", err)
	}

	// Build NATS message with headers
	natsMsg := &nats.Msg{
		Subject: "dragrace.dev.backend.runner.register",
		Data:    payload,
		Header:  nats.Header{},
	}

	// Add client_id header for backend to identify this runner's API key
	clientID := auth.LoadClientID()
	if clientID != "" {
		natsMsg.Header.Set("X-Runner-Client-ID", clientID)
	}

	msg, err := nc.RequestMsg(natsMsg, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("registration request failed: %w", err)
	}

	var resp RegisterResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse registration response: %w", err)
	}

	if resp.Error != "" {
		return nil, fmt.Errorf("registration failed: %s", resp.Error)
	}

	log.Printf("✅ Registered as runner %s", resp.RunnerID)
	return &resp, nil
}
