package jobs

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func validJob() JobMessage {
	var job JobMessage
	job.Challenge.Source = GitSource{URL: "https://github.com/dragrace/challenge.git", Ref: "main"}
	job.Solution.Source = GitSource{URL: "https://github.com/participant/solution.git", Ref: "0123456789abcdef0123456789abcdef01234567"}
	return job
}

func TestJobContractAcceptsRepositoryAndCommit(t *testing.T) {
	job := validJob()
	if err := job.Validate(); err != nil {
		t.Fatalf("expected valid submission contract, got %v", err)
	}
}

func TestJobContractRejectsArchiveAndShortSHA(t *testing.T) {
	archive := validJob()
	archive.Solution.Source.URL = "https://github.com/participant/solution/archive/main.zip"
	if err := archive.Validate(); err == nil {
		t.Fatal("expected archive URL to be rejected")
	}

	shortSHA := validJob()
	shortSHA.Solution.Source.Ref = "0123456"
	if err := shortSHA.Validate(); err == nil {
		t.Fatal("expected abbreviated SHA to be rejected")
	}
}

func TestTruncatePhaseLogBoundsUTF8(t *testing.T) {
	bounded, truncated := truncatePhaseLog(strings.Repeat("é", maxPhaseLogBytes))
	if !truncated {
		t.Fatal("expected oversized log to be truncated")
	}
	if len(bounded) > maxPhaseLogBytes {
		t.Fatalf("expected at most %d bytes, got %d", maxPhaseLogBytes, len(bounded))
	}
	if !utf8.ValidString(bounded) {
		t.Fatal("truncation must preserve valid UTF-8")
	}
	if !strings.Contains(bounded, "log truncated") {
		t.Fatal("truncated log must carry a visible marker")
	}
}
