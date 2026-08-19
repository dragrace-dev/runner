package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSignFilesCreatesVerifiableReleaseArtifacts(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "dragrace-runner-linux-amd64")
	binary := []byte("release binary")
	if err := os.WriteFile(path, binary, 0755); err != nil {
		t.Fatal(err)
	}

	if err := signFiles([]string{path}, privateKey, publicKey); err != nil {
		t.Fatal(err)
	}
	checksumBody, err := os.ReadFile(path + ".sha256")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(binary)
	checksum := hex.EncodeToString(sum[:])
	if !strings.HasPrefix(string(checksumBody), checksum+"  ") {
		t.Fatalf("unexpected checksum body: %q", checksumBody)
	}
	signatureBody, err := os.ReadFile(path + ".sha256.sig")
	if err != nil {
		t.Fatal(err)
	}
	signature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(signatureBody)))
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(publicKey, []byte(checksum), signature) {
		t.Fatal("release signature did not verify")
	}
}
