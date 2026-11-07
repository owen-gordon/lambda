// Lambda is the hosted Python-function control plane.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/owen-gordon/lambda/internal/controlplane"
)

func main() {
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "lambda is a hosted service; configure it through environment variables")
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := controlplane.Run(ctx, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
