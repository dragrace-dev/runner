package updater

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

func TestVerifyDownloadAcceptsSignedChecksum(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	binary := []byte("trusted runner binary")
	sum := sha256.Sum256(binary)
	checksum := hex.EncodeToString(sum[:])
	signature := ed25519.Sign(privateKey, []byte(checksum))

	err = verifyDownload(binary, []byte(checksum+"  dragrace-runner\n"), []byte(base64.StdEncoding.EncodeToString(signature)), publicKey)
	if err != nil {
		t.Fatalf("valid signed update rejected: %v", err)
	}
}

func TestVerifyArtifactFilesUsesUpdaterVerificationPath(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	binary := []byte("published runner")
	sum := sha256.Sum256(binary)
	checksum := hex.EncodeToString(sum[:])
	dir := t.TempDir()
	path := filepath.Join(dir, "dragrace-runner-linux-amd64")
	if err := os.WriteFile(path, binary, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".sha256", []byte(checksum+"  "+filepath.Base(path)+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(privateKey, []byte(checksum))
	if err := os.WriteFile(path+".sha256.sig", []byte(base64.StdEncoding.EncodeToString(signature)+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUNNER_UPDATE_PUBLIC_KEY", base64.StdEncoding.EncodeToString(publicKey))

	if err := VerifyArtifactFiles(path); err != nil {
		t.Fatalf("published artifact rejected: %v", err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := VerifyArtifactFiles(path); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered published artifact error = %v, want checksum mismatch", err)
	}
}

func TestVerifyDownloadRejectsTampering(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	binary := []byte("trusted runner binary")
	sum := sha256.Sum256(binary)
	checksum := hex.EncodeToString(sum[:])
	signature := ed25519.Sign(privateKey, []byte(checksum))
	signatureB64 := []byte(base64.StdEncoding.EncodeToString(signature))

	if err := verifyDownload([]byte("tampered"), []byte(checksum), signatureB64, publicKey); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered binary error = %v, want checksum mismatch", err)
	}
	badSignature := append([]byte(nil), signature...)
	badSignature[0] ^= 0xff
	if err := verifyDownload(binary, []byte(checksum), []byte(base64.StdEncoding.EncodeToString(badSignature)), publicKey); err == nil || !strings.Contains(err.Error(), "signature verification failed") {
		t.Fatalf("tampered signature error = %v, want signature failure", err)
	}
}
