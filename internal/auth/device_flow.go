package auth

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DeviceCodeResponse from the backend
type DeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// DeviceTokenResponse from the backend
type DeviceTokenResponse struct {
	Status      string `json:"status"`
	Credentials string `json:"credentials,omitempty"`
	ClientID    string `json:"client_id,omitempty"`
}

// Login performs the device code flow to authenticate a runner.
// It displays a user code, waits for the user to authorize in browser,
// then saves the NATS .creds file locally.
func Login(backendURL string) error {
	log.Println("🔑 Authenticating runner via device code flow...")

	// 1. Request device code
	resp, err := http.Post(backendURL+"/api/device/code", "application/json", strings.NewReader("{}"))
	if err != nil {
		return fmt.Errorf("failed to request device code: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("device code request failed (%d): %s", resp.StatusCode, body)
	}

	var codeResp DeviceCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&codeResp); err != nil {
		return fmt.Errorf("failed to parse device code response: %w", err)
	}

	// 2. Display instructions
	fmt.Println()
	fmt.Println("  ┌─────────────────────────────────────────────────┐")
	fmt.Println("  │                                                 │")
	fmt.Printf("  │   To authenticate this runner, open:            │\n")
	fmt.Printf("  │   %-44s│\n", codeResp.VerificationURI)
	fmt.Println("  │                                                 │")
	fmt.Printf("  │   Enter code: %-33s│\n", codeResp.UserCode)
	fmt.Println("  │                                                 │")
	fmt.Println("  └─────────────────────────────────────────────────┘")
	fmt.Println()

	// 3. Poll for authorization
	interval := time.Duration(codeResp.Interval) * time.Second
	if interval < 3*time.Second {
		interval = 5 * time.Second
	}

	deadline := time.Now().Add(time.Duration(codeResp.ExpiresIn) * time.Second)

	for time.Now().Before(deadline) {
		fmt.Print("  ⏳ Waiting for authorization...\r")
		time.Sleep(interval)

		tokenResp, err := pollToken(backendURL, codeResp.DeviceCode)
		if err != nil {
			log.Printf("  ⚠️  Poll error: %v", err)
			continue
		}

		switch tokenResp.Status {
		case "authorized":
			// 4. Save credentials
			credsPath, err := saveCredentials(tokenResp.Credentials)
			if err != nil {
				return fmt.Errorf("failed to save credentials: %w", err)
			}
			// 5. Save client_id for NATS message headers
			if tokenResp.ClientID != "" {
				saveClientID(tokenResp.ClientID)
			}
			fmt.Printf("\n  ✅ Authenticated! Credentials saved to %s\n", credsPath)
			fmt.Println("  You can now start the runner with: ./runner")
			fmt.Println()
			return nil

		case "expired":
			return fmt.Errorf("device code expired — please try again")

		case "pending":
			// keep polling
		}
	}

	return fmt.Errorf("authorization timed out — please try again")
}

func pollToken(backendURL, deviceCode string) (*DeviceTokenResponse, error) {
	body := fmt.Sprintf(`{"device_code":"%s"}`, deviceCode)
	resp, err := http.Post(
		backendURL+"/api/device/token",
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var tokenResp DeviceTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, err
	}

	return &tokenResp, nil
}

func saveCredentials(creds string) (string, error) {
	credsDir := CredsDir()
	if err := os.MkdirAll(credsDir, 0700); err != nil {
		return "", err
	}

	credsPath := filepath.Join(credsDir, "credentials")
	if err := os.WriteFile(credsPath, []byte(creds), 0600); err != nil {
		return "", err
	}

	return credsPath, nil
}

// CredsDir returns the default credentials directory.
func CredsDir() string {
	if d := os.Getenv("DRAGRACE_CREDS_DIR"); d != "" {
		return d
	}

	home, _ := os.UserHomeDir()
	if home == "" {
		if runtime.GOOS == "windows" {
			home = os.Getenv("USERPROFILE")
		} else {
			home = "/root"
		}
	}
	return filepath.Join(home, ".dragrace")
}

// ResolveCredsFile finds the .creds file to use, in priority order:
// 1. Explicit --creds flag
// 2. ~/.dragrace/credentials (cached from device flow)
// Returns empty string if none found.
func ResolveCredsFile(explicit string) string {
	if explicit != "" {
		return explicit
	}

	cached := filepath.Join(CredsDir(), "credentials")
	if _, err := os.Stat(cached); err == nil {
		return cached
	}

	return ""
}

// saveClientID saves the client_id to disk for NATS message headers.
func saveClientID(clientID string) {
	path := filepath.Join(CredsDir(), "client_id")
	os.WriteFile(path, []byte(clientID), 0600)
}

// LoadClientID reads the saved client_id from disk.
func LoadClientID() string {
	path := filepath.Join(CredsDir(), "client_id")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}
