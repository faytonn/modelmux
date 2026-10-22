// Command modelmux runs the LLM gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/faytonn/modelmux/internal/config"
	"github.com/faytonn/modelmux/internal/providers"
	"github.com/faytonn/modelmux/internal/router"
	"github.com/faytonn/modelmux/internal/server"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to the YAML config file")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(*configPath, log); err != nil {
		log.Error("modelmux exited", "error", err)
		os.Exit(1)
	}
}

func run(configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	candidates, err := buildCandidates(cfg)
	if err != nil {
		return err
	}
	rt, err := router.New(candidates, router.Options{Routing: cfg.Routing, Retry: cfg.Retry, Logger: log})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           server.New(rt, cfg.Server, log).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      cfg.Server.RequestTimeout + 5*time.Second,
	}

	// ctx is cancelled on Ctrl+C or SIGTERM (what `docker stop` sends).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("modelmux listening", "addr", cfg.Server.Addr, "candidates", len(candidates))
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Shutdown stops accepting connections and waits for in-flight requests.
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// buildCandidates turns provider config into router candidates. Wiring lives
// here so that config and providers do not need to import each other.
func buildCandidates(cfg *config.Config) ([]router.Candidate, error) {
	var out []router.Candidate
	for _, pc := range cfg.Providers {
		var p providers.Provider
		switch pc.Type {
		case "mock":
			p = providers.NewMock(pc.Name, providers.MockConfig(pc.Mock))
		default:
			return nil, fmt.Errorf("provider %q: unknown type %q", pc.Name, pc.Type)
		}
		for _, m := range pc.Models {
			out = append(out, router.Candidate{Provider: p, Model: m, Timeout: pc.AttemptTimeout})
		}
	}
	return out, nil
}
