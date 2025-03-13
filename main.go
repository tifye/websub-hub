package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	hub := NewHub(logger.WithGroup("hub"))

	s := &http.Server{
		Handler:           hub.Mux(),
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "1913"
	}

	addr := "0.0.0.0:" + port
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	go func() {
		logger.Info("serving " + addr)
		if err := s.Serve(ln); err != nil {
			if !errors.Is(err, http.ErrServerClosed) {
				logger.Error(err.Error())
			}
		}
	}()

	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	logger.Info("shutting down")
	if err = s.Shutdown(sctx); err != nil {
		return fmt.Errorf("server shutdown: %w", err)
	}

	return nil
}
