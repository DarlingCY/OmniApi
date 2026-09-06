package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/omniapi/omni-api/internal/app"
)

func main() {
	options, err := app.ParseOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.Run(ctx, options); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
