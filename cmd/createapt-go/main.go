// Command createapt-go creates, updates, publishes and validates apt
// (Debian/Ubuntu) package repositories on local disk or remote storage.
package main

import (
	"fmt"
	"os"

	"github.com/danudey/createrepo-go/internal/cli"
	"github.com/danudey/createrepo-go/pkg/backend"
)

func main() {
	backend.SetAppName("createapt-go")
	ctx, stop := cli.SignalContext()
	err := rootCmd().ExecuteContext(ctx)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
