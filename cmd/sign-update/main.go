package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: sign-update <runner-binary> [...]")
	}
	privateKey := mustDecodeKey("RUNNER_UPDATE_SIGNING_KEY", ed25519.PrivateKeySize)
	publicKey := mustDecodeKey("RUNNER_UPDATE_PUBLIC_KEY", ed25519.PublicKeySize)
	if err := signFiles(os.Args[1:], ed25519.PrivateKey(privateKey), ed25519.PublicKey(publicKey)); err != nil {
		fatalf("%v", err)
	}
}

func signFiles(paths []string, privateKey ed25519.PrivateKey, publicKey ed25519.PublicKey) error {
	derivedPublicKey := privateKey[ed25519.SeedSize:]
	if !publicKey.Equal(ed25519.PublicKey(derivedPublicKey)) {
		return fmt.Errorf("RUNNER_UPDATE_PUBLIC_KEY does not match RUNNER_UPDATE_SIGNING_KEY")
	}

	for _, path := range paths {
		binary, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		sum := sha256.Sum256(binary)
		checksum := hex.EncodeToString(sum[:])
		checksumBody := fmt.Sprintf("%s  %s\n", checksum, filepath.Base(path))
		if err := os.WriteFile(path+".sha256", []byte(checksumBody), 0644); err != nil {
			return fmt.Errorf("write checksum for %s: %w", path, err)
		}
		signature := ed25519.Sign(ed25519.PrivateKey(privateKey), []byte(checksum))
		encoded := base64.StdEncoding.EncodeToString(signature) + "\n"
		if err := os.WriteFile(path+".sha256.sig", []byte(encoded), 0644); err != nil {
			return fmt.Errorf("write signature for %s: %w", path, err)
		}
	}
	return nil
}

func mustDecodeKey(name string, expectedSize int) []byte {
	encoded := strings.TrimSpace(os.Getenv(name))
	if encoded == "" {
		fatalf("%s is required", name)
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		fatalf("decode %s: %v", name, err)
	}
	if len(key) != expectedSize {
		fatalf("%s has %d bytes, expected %d", name, len(key), expectedSize)
	}
	return key
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
