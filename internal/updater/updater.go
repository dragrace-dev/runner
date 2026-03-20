package updater

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"dragrace/internal/version"
)

// Set this at build/release time or override with RUNNER_UPDATE_PUBLIC_KEY (base64 Ed25519 public key).
const embeddedUpdatePublicKeyB64 = ""

func Update(updateURL string) error {
	if updateURL == "" {
		return fmt.Errorf("no update URL configured (set RUNNER_UPDATE_URL)")
	}

	binaryName := fmt.Sprintf("dragrace-runner-%s-%s", runtime.GOOS, runtime.GOARCH)
	downloadURL := strings.TrimRight(updateURL, "/") + "/" + binaryName
	checksumURL := downloadURL + ".sha256"
	signatureURL := checksumURL + ".sig"

	log.Printf("⬇️  Downloading update from %s ...", downloadURL)
	binaryData, err := downloadBytes(downloadURL)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}

	checksumBody, err := downloadBytes(checksumURL)
	if err != nil {
		return fmt.Errorf("failed to download checksum: %w", err)
	}
	expectedChecksum, err := parseChecksumLine(string(checksumBody))
	if err != nil {
		return fmt.Errorf("invalid checksum file: %w", err)
	}

	sum := sha256.Sum256(binaryData)
	actualChecksum := hex.EncodeToString(sum[:])
	if !strings.EqualFold(actualChecksum, expectedChecksum) {
		return fmt.Errorf("checksum mismatch")
	}

	signatureB64, err := downloadBytes(signatureURL)
	if err != nil {
		return fmt.Errorf("failed to download signature: %w", err)
	}

	pubKey, err := loadUpdatePublicKey()
	if err != nil {
		return err
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(signatureB64)))
	if err != nil {
		return fmt.Errorf("invalid signature encoding: %w", err)
	}
	// Signature is over the checksum hex string to keep the wire format stable.
	if !ed25519.Verify(pubKey, []byte(strings.ToLower(expectedChecksum)), sig) {
		return fmt.Errorf("signature verification failed")
	}

	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine executable path: %w", err)
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return fmt.Errorf("cannot resolve executable path: %w", err)
	}

	dir := filepath.Dir(execPath)
	tmpFile, err := os.CreateTemp(dir, "dragrace-runner-update-*")
	if err != nil {
		return fmt.Errorf("cannot create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		if tmpPath != "" {
			os.Remove(tmpPath)
		}
	}()

	if _, err := tmpFile.Write(binaryData); err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to write update: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to finalize update file: %w", err)
	}

	if err := os.Chmod(tmpPath, 0755); err != nil {
		return fmt.Errorf("failed to set permissions: %w", err)
	}
	if err := os.Rename(tmpPath, execPath); err != nil {
		return fmt.Errorf("failed to replace binary: %w", err)
	}
	tmpPath = ""

	log.Printf("✅ Updated successfully to latest version (was %s)", version.Version)
	log.Println("🔄 Please restart the runner to use the new version.")
	return nil
}

func downloadBytes(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func parseChecksumLine(body string) (string, error) {
	line := strings.TrimSpace(body)
	if line == "" {
		return "", fmt.Errorf("empty checksum")
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", fmt.Errorf("malformed checksum")
	}
	checksum := strings.ToLower(fields[0])
	if len(checksum) != 64 {
		return "", fmt.Errorf("checksum must be 64 hex chars")
	}
	if _, err := hex.DecodeString(checksum); err != nil {
		return "", fmt.Errorf("checksum is not valid hex")
	}
	return checksum, nil
}

func loadUpdatePublicKey() (ed25519.PublicKey, error) {
	b64 := strings.TrimSpace(os.Getenv("RUNNER_UPDATE_PUBLIC_KEY"))
	if b64 == "" {
		b64 = embeddedUpdatePublicKeyB64
	}
	if b64 == "" {
		return nil, fmt.Errorf("missing update public key (set RUNNER_UPDATE_PUBLIC_KEY)")
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("invalid update public key encoding: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid update public key size")
	}
	return ed25519.PublicKey(raw), nil
}
