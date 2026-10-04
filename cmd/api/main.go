// Command api serves the page and the API under traces, metrics, logs and profiles.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go-observatory/internal/app"
	"go-observatory/internal/config"
	"go-observatory/internal/database"
	"go-observatory/internal/observability"
)

// How long requests in flight get to finish once the process is told to stop.
const shutdownTimeout = 10 * time.Second

func main() {
	command := serve
	// `api health` asks the running server for /health: the image has no shell and no curl.
	if len(os.Args) > 1 && os.Args[1] == "health" {
		command = checkHealth
	}
	if err := command(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// serve reads the settings, sets up logging and the database file, and serves until SIGINT or
// SIGTERM; then the requests in flight finish, and the application closes.
func serve() error {
	settings, err := config.Load()
	if err != nil {
		return err
	}
	observability.ConfigureLogging(settings.LogLevel.Level)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := database.Prepare(ctx, settings.DB.Path); err != nil {
		return err
	}
	application, err := app.New(ctx, settings)
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              settings.Server.Address(),
		Handler:           application.Handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// The server's own complaints — a bad TLS handshake, a broken connection — as JSON lines too.
		ErrorLog: slog.NewLogLogger(observability.NewLogger(os.Stdout, settings.LogLevel.Level, "net/http").Handler(), slog.LevelWarn),
	}
	served := make(chan error, 1)
	go func() { served <- server.ListenAndServe() }()
	slog.InfoContext(ctx, "serving", "address", server.Addr)

	select {
	case err := <-served:
		return errors.Join(err, application.Close(context.Background()))
	case <-ctx.Done():
		slog.InfoContext(ctx, "stopping")
	}
	shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return errors.Join(server.Shutdown(shutdown), application.Close(shutdown))
}

// checkHealth asks the running server for /health.
func checkHealth() error {
	settings, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/health", settings.Server.Port), nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("/health: status %d", response.StatusCode)
	}
	return nil
}
