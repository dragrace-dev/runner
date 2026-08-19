package main

import (
	"fmt"
	"os"

	"dragrace/internal/updater"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: verify-update <runner-binary>")
		os.Exit(2)
	}
	if err := updater.VerifyArtifactFiles(os.Args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "verify %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
	fmt.Printf("verified %s\n", os.Args[1])
}
