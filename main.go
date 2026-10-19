package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, err := loadConfig()
	if err != nil {
		fatal("invalid config", err)
	}
	m, err := NewManager(cfg)
	if err != nil {
		fatal("create docker client", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	initCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	err = m.Init(initCtx)
	cancel()
	if err != nil {
		fatal("init sandbox manager", err)
	}
	go m.RunReaper(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           newHandler(cfg, m),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server stopped", "error", err)
			stop()
		}
	}()
	slog.Info("sandbox server listening", "addr", cfg.Listen, "image", cfg.Image, "data_root", cfg.DataRoot,
		"network", cfg.Network)

	var proxySrv *http.Server
	if cfg.Network == "proxy" {
		proxySrv = &http.Server{Addr: cfg.ProxyListen, Handler: newEgressProxy(cfg), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := proxySrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("egress proxy stopped", "error", err)
				stop()
			}
		}()
		slog.Info("egress proxy listening", "addr", cfg.ProxyListen, "network", cfg.ProxyNetwork,
			"allowlist", cfg.EgressAllowlist)
		if slices.Contains(cfg.EgressAllowlist, "*") {
			slog.Warn("egress allowlist contains *, sandboxes can reach any public host (private addresses still blocked)")
		}
	}

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	if proxySrv != nil {
		_ = proxySrv.Close()
	}
	m.Close()
}

func fatal(msg string, err error) {
	slog.Error(msg, "error", err)
	os.Exit(1)
}
