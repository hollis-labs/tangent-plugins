// Command ptrack calls an explicitly configured portfolio backend.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/hollis-labs/tangent-plugins/portfolio/internal/ptrack"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := ptrack.Run(ctx, os.Args[1:], ptrack.Options{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, LookupEnv: os.Getenv})
	cancel()
	os.Exit(code)
}
