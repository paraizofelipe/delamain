package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/paraizo/delamain/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cli.NewRoot().ExecuteContext(ctx); err != nil {
		if !errors.Is(err, cli.ErrChecksFailed) {
			fmt.Fprintln(os.Stderr, "erro:", err)
		}
		os.Exit(1)
	}
}
