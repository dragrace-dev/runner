package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Clone clones a Git repository at a specific ref (branch, tag, or commit hash)
// Uses shallow clone (depth=1) to minimize bandwidth and disk usage
func Clone(url, ref, destDir string) error {
	// Ensure destination directory exists
	if err := os.MkdirAll(filepath.Dir(destDir), 0755); err != nil {
		return fmt.Errorf("failed to create parent directory: %w", err)
	}

	// Try shallow clone with branch/tag first
	cmd := exec.Command("git", "clone", "--depth", "1", "--branch", ref, url, destDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		// If branch/tag clone fails, it might be a commit hash
		// Clone without branch, then checkout the specific commit
		return cloneWithCommit(url, ref, destDir)
	}

	return nil
}

// cloneWithCommit handles the case where ref is a commit hash
func cloneWithCommit(url, commitHash, destDir string) error {
	// Remove potentially partial clone
	os.RemoveAll(destDir)

	// Clone without depth restriction (needed for arbitrary commit)
	cloneCmd := exec.Command("git", "clone", url, destDir)
	cloneCmd.Stdout = os.Stdout
	cloneCmd.Stderr = os.Stderr

	if err := cloneCmd.Run(); err != nil {
		return fmt.Errorf("failed to clone repository: %w", err)
	}

	// Checkout the specific commit
	checkoutCmd := exec.Command("git", "checkout", commitHash)
	checkoutCmd.Dir = destDir
	checkoutCmd.Stdout = os.Stdout
	checkoutCmd.Stderr = os.Stderr

	if err := checkoutCmd.Run(); err != nil {
		return fmt.Errorf("failed to checkout commit %s: %w", commitHash, err)
	}

	return nil
}

// CloneShallow clones only the latest commit without history
// Best for CI/CD where history is not needed
func CloneShallow(url, ref, destDir string) error {
	cmd := exec.Command("git", "clone",
		"--depth", "1",
		"--single-branch",
		"--branch", ref,
		url, destDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
