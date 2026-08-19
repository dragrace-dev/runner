package git

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCloneErrorDoesNotDiscloseSourceMetadata(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "private-repository-name")
	ref := "private-ref-or-commit"
	err := Clone(repository, ref, filepath.Join(t.TempDir(), "checkout"))
	if err == nil {
		t.Fatal("expected clone to fail for a missing local repository")
	}
	message := err.Error()
	if strings.Contains(message, repository) || strings.Contains(message, ref) {
		t.Fatalf("clone error disclosed repository or ref: %q", message)
	}
}
