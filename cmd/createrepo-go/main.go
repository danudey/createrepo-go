// Command createrepo-go is a lightweight dnf/yum (rpm-md) repository manager.
package main

import (
	"errors"
	"fmt"
	"os"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		// A reported check failure has already printed its details; exit
		// non-zero without an extra "error:" line.
		if !errors.Is(err, errCheckFailed) {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(1)
	}
}
