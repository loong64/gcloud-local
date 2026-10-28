package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"gcloud-local/internal/localbuild"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := localbuild.Main(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "gcloud-local:", err)
		os.Exit(localbuild.ExitCode(err))
	}
}
