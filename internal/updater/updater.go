package updater

import (
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

// Update downloads the latest runner binary and replaces the current executable.
// updateURL should be the base URL where binaries are hosted.
// Expected binary naming: dragrace-runner-{os}-{arch} (e.g. dragrace-runner-linux-amd64)
func Update(updateURL string) error {
	if updateURL == "" {
		return fmt.Errorf("no update URL configured (set RUNNER_UPDATE_URL)")
	}

	// Build download URL
	binaryName := fmt.Sprintf("dragrace-runner-%s-%s", runtime.GOOS, runtime.GOARCH)
	downloadURL := strings.TrimRight(updateURL, "/") + "/" + binaryName

	log.Printf("⬇️  Downloading update from %s ...", downloadURL)

	// Download to temp file
	resp, err := http.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	// Get current executable path
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine executable path: %w", err)
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return fmt.Errorf("cannot resolve executable path: %w", err)
	}

	// Write to temp file in same directory (for atomic rename)
	dir := filepath.Dir(execPath)
	tmpFile, err := os.CreateTemp(dir, "dragrace-runner-update-*")
	if err != nil {
		return fmt.Errorf("cannot create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	// Clean up temp file on failure
	defer func() {
		if tmpPath != "" {
			os.Remove(tmpPath)
		}
	}()

	written, err := io.Copy(tmpFile, resp.Body)
	if err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to write update: %w", err)
	}
	tmpFile.Close()

	log.Printf("📦 Downloaded %d bytes", written)

	// Make executable
	if err := os.Chmod(tmpPath, 0755); err != nil {
		return fmt.Errorf("failed to set permissions: %w", err)
	}

	// Atomic replace: rename temp file over current executable
	if err := os.Rename(tmpPath, execPath); err != nil {
		return fmt.Errorf("failed to replace binary: %w", err)
	}

	// Prevent deferred cleanup since rename succeeded
	tmpPath = ""

	log.Printf("✅ Updated successfully to latest version (was %s)", version.Version)
	log.Println("🔄 Please restart the runner to use the new version.")

	return nil
}
