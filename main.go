// CampusPulse: know before you go.
//
// A real-time campus occupancy and Wi-Fi monitoring prototype for
// George Washington University, written in Go.
//
//	go run .            then open http://localhost:8080
//	go run . -demo      start at the Bell Hall 1:40 PM changeover scenario
//	go run . -h         list every option
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	_ "time/tzdata" // embed time zones so America/New_York works in minimal containers

	"github.com/dylanpanashe/csci_6221/internal/app"
	"github.com/dylanpanashe/csci_6221/internal/config"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "campuspulse:", err)
		os.Exit(1)
	}
}

// run holds everything main would do, but returns an error instead of
// calling os.Exit, so deferred cleanup always runs.
func run(args []string) error {
	cfg, err := config.Load(args, os.Getenv)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	// Ctrl-C or `docker stop` cancels this context, which starts graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.New(cfg, log)
	if err != nil {
		return err
	}
	return a.Run(ctx)
}
