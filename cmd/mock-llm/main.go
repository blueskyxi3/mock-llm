package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"mock-llm/internal/server"
)

func main() {
	configPath := flag.String("config", "", "JSON config file")
	check := flag.String("healthcheck", "", "check HTTP health URL and exit")
	dump := flag.Bool("dump-default-config", false, "print default configuration and exit")
	flag.Parse()
	if *check != "" {
		client := http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(*check)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			os.Exit(1)
		}
		return
	}
	if *dump {
		dumpConfig()
		return
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	c, err := server.LoadConfig(*configPath)
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	service, err := server.New(c, logger, nil)
	if err != nil {
		logger.Error("cannot initialize service", "error", err)
		os.Exit(1)
	}
	httpServer := &http.Server{Addr: c.Listen, Handler: service, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: c.Timeout(), WriteTimeout: c.Timeout() + 5*time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	done := make(chan error, 1)
	go func() { done <- httpServer.ListenAndServe() }()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logger.Info("mock model service starting", "listen", c.Listen, "models", len(c.Models), "max_active", c.MaxActive)
	select {
	case err := <-done:
		if err != http.ErrServerClosed {
			logger.Error("HTTP server failed", "error", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		service.Drain()
		shutdown, cancel := context.WithTimeout(context.Background(), time.Duration(c.ShutdownSeconds)*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdown); err != nil {
			service.Cancel()
			_ = httpServer.Close()
			logger.Warn("shutdown deadline reached", "error", err)
		}
	}
	service.Cancel()
}
