package config

import (
	"os"
	"path/filepath"
	"testing"
)

// helper: write a temp file and return its path
func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "dragrace.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// ── ParseUnifiedFile ────────────────────────────────────────────────────────

func TestParseUnifiedFile_BothDocuments(t *testing.T) {
	path := writeTempYAML(t, `
version: "1.0"
type: challenge
challenge:
  id: test-1brc
  name: One Billion Row Challenge
limits:
  memory: "8GB"
  cpu: "4.0"
  timeout: "5m"
scoring:
  primary: execution_time
  direction: minimize
---
version: "1.0"
type: solution
runtime:
  docker: "eclipse-temurin:21-jdk"
run:
  script: "scripts/run.sh"
`)

	challenge, solution, err := ParseUnifiedFile(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if challenge == nil {
		t.Fatal("expected challenge, got nil")
	}
	if solution == nil {
		t.Fatal("expected solution, got nil")
	}
	if challenge.Challenge.ID != "test-1brc" {
		t.Errorf("expected challenge id 'test-1brc', got '%s'", challenge.Challenge.ID)
	}
	if solution.Runtime.Docker != "eclipse-temurin:21-jdk" {
		t.Errorf("expected docker 'eclipse-temurin:21-jdk', got '%s'", solution.Runtime.Docker)
	}
}

func TestParseUnifiedFile_ChallengeOnly(t *testing.T) {
	path := writeTempYAML(t, `
version: "1.0"
type: challenge
challenge:
  id: test
  name: Test Challenge
limits:
  memory: "512MB"
  cpu: "1.0"
  timeout: "60s"
scoring:
  primary: execution_time
  direction: minimize
`)

	challenge, solution, err := ParseUnifiedFile(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if challenge == nil {
		t.Fatal("expected challenge, got nil")
	}
	if solution != nil {
		t.Fatal("expected nil solution for challenge-only file")
	}
}

func TestParseUnifiedFile_SolutionOnly(t *testing.T) {
	path := writeTempYAML(t, `
version: "1.0"
type: solution
runtime:
  docker: "golang:1.21"
run:
  script: "scripts/run.sh"
`)

	challenge, solution, err := ParseUnifiedFile(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if challenge != nil {
		t.Fatal("expected nil challenge for solution-only file")
	}
	if solution == nil {
		t.Fatal("expected solution, got nil")
	}
}

func TestParseUnifiedFile_DuplicateChallenge(t *testing.T) {
	path := writeTempYAML(t, `
version: "1.0"
type: challenge
challenge:
  id: test1
  name: Test 1
---
version: "1.0"
type: challenge
challenge:
  id: test2
  name: Test 2
`)

	_, _, err := ParseUnifiedFile(path)
	if err == nil {
		t.Fatal("expected error for duplicate challenge documents")
	}
}

func TestParseUnifiedFile_DuplicateSolution(t *testing.T) {
	path := writeTempYAML(t, `
version: "1.0"
type: solution
runtime:
  docker: "golang:1.21"
run:
  script: "scripts/run1.sh"
---
version: "1.0"
type: solution
runtime:
  docker: "golang:1.22"
run:
  script: "scripts/run2.sh"
`)

	_, _, err := ParseUnifiedFile(path)
	if err == nil {
		t.Fatal("expected error for duplicate solution documents")
	}
}

func TestParseUnifiedFile_UnknownType(t *testing.T) {
	path := writeTempYAML(t, `
version: "1.0"
type: foobar
`)

	_, _, err := ParseUnifiedFile(path)
	if err == nil {
		t.Fatal("expected error for unknown document type")
	}
}

func TestParseUnifiedFile_EmptyFile(t *testing.T) {
	path := writeTempYAML(t, "")
	_, _, err := ParseUnifiedFile(path)
	if err == nil {
		t.Fatal("expected error for empty file")
	}
}

// ── ExtractSolutionFromFile ─────────────────────────────────────────────────

func TestExtractSolution_IgnoresChallenge(t *testing.T) {
	// Security test: challenge section must be ignored
	path := writeTempYAML(t, `
version: "1.0"
type: challenge
challenge:
  id: tampered
  name: Tampered Challenge
limits:
  memory: "999GB"
  timeout: "999h"
scoring:
  primary: execution_time
  direction: maximize
---
version: "1.0"
type: solution
runtime:
  docker: "golang:1.21"
run:
  script: "scripts/run.sh"
`)

	sol, err := ExtractSolutionFromFile(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sol == nil {
		t.Fatal("expected solution, got nil")
	}
	if sol.Runtime.Docker != "golang:1.21" {
		t.Errorf("wrong docker image: %s", sol.Runtime.Docker)
	}
}

func TestExtractSolution_SolutionOnlyFile(t *testing.T) {
	path := writeTempYAML(t, `
version: "1.0"
type: solution
runtime:
  docker: "python:3.12"
run:
  script: "scripts/run.sh"
`)

	sol, err := ExtractSolutionFromFile(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sol.Runtime.Docker != "python:3.12" {
		t.Errorf("expected python:3.12, got %s", sol.Runtime.Docker)
	}
}

func TestExtractSolution_NoSolutionDocument(t *testing.T) {
	path := writeTempYAML(t, `
version: "1.0"
type: challenge
challenge:
  id: test
  name: Test
`)

	_, err := ExtractSolutionFromFile(path)
	if err == nil {
		t.Fatal("expected error when no solution document is present")
	}
}

// ── Reverse order (solution first, then challenge) ──────────────────────────

func TestParseUnifiedFile_ReverseOrder(t *testing.T) {
	path := writeTempYAML(t, `
version: "1.0"
type: solution
runtime:
  docker: "golang:1.21"
run:
  script: "scripts/run.sh"
---
version: "1.0"
type: challenge
challenge:
  id: reverse-test
  name: Reverse Order Test
limits:
  memory: "1GB"
  cpu: "2.0"
  timeout: "120s"
scoring:
  primary: execution_time
  direction: minimize
`)

	challenge, solution, err := ParseUnifiedFile(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if challenge == nil || solution == nil {
		t.Fatal("expected both challenge and solution regardless of order")
	}
	if challenge.Challenge.ID != "reverse-test" {
		t.Errorf("wrong challenge id: %s", challenge.Challenge.ID)
	}
}
