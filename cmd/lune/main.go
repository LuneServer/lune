package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/LuneServer/lune/internal/api"
	"github.com/LuneServer/lune/internal/auth"
	"github.com/LuneServer/lune/internal/config"
	"github.com/LuneServer/lune/internal/edge"
	"github.com/LuneServer/lune/internal/library"
	"github.com/LuneServer/lune/internal/maintenance"
	"github.com/LuneServer/lune/internal/metadata"
	"github.com/LuneServer/lune/internal/requests"
	"github.com/LuneServer/lune/internal/security"
	"github.com/LuneServer/lune/internal/store"
	"github.com/LuneServer/lune/internal/stream"
	"github.com/LuneServer/lune/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "Print Lune Server version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("lune %s (commit %s, built %s)\n", version.Version, version.Commit, version.BuildDate)
		return
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "lune: configuration error: %v\n", err)
		os.Exit(1)
	}

	logger := newLogger(cfg.LogLevel, cfg.LogFormat)
	logger.Info("starting lune server",
		"version", version.Version,
		"edge_mode", cfg.EdgeMode,
		"data_dir", cfg.DataDir,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DBPath())
	if err != nil {
		logger.Error("failed to initialize database", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := db.Close(); err != nil {
			logger.Warn("error closing database", "error", err)
		}
	}()

	if rawNet, err := db.GetSetting(ctx, config.NetworkConfigSettingKey); err == nil && strings.TrimSpace(rawNet) != "" {
		var stored config.NetworkConfig
		if err := json.Unmarshal([]byte(rawNet), &stored); err != nil {
			logger.Warn("ignoring malformed stored network configuration; falling back to environment", "error", err)
		} else if merged, err := config.ApplyStoredNetworkConfig(cfg, stored); err != nil {
			logger.Warn("ignoring invalid stored network configuration; falling back to environment", "error", err)
		} else {
			cfg = merged
			logger.Info("applied stored network configuration",
				"edge_mode", cfg.EdgeMode,
				"listen_addr", cfg.ListenAddr,
			)
		}
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		logger.Warn("failed to load stored network configuration; falling back to environment", "error", err)
	}

	authSvc := auth.NewService(db, security.NewAuthRateLimiter())
	authSvc.SetDataDir(cfg.DataDir)
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := db.PruneExpiredAuthState(ctx); err != nil && !errors.Is(err, context.Canceled) {
					logger.Warn("failed to prune expired auth state", "error", err)
				}
			}
		}
	}()
	libSvc := library.NewService(db, metadata.NewFFProbe(), logger)
	var tmdbDiscoverer requests.TMDBDiscoverer
	if tmdb := metadata.NewTMDBClient(cfg.TMDBAPIKey, cfg.TMDBBaseURL, cfg.DataDir); tmdb != nil {
		libSvc.SetEnricher(tmdb)
		tmdbDiscoverer = tmdb
		logger.Info("enabled TMDB metadata enricher")
	}
	reqSvc := requests.NewService(db, cfg, tmdbDiscoverer, logger)

	watcher := library.NewWatcher(libSvc, cfg.ScanInterval)
	libSvc.SetWatcher(watcher)
	go watcher.Run(ctx)
	logger.Info("started library watcher", "periodic_interval", cfg.ScanInterval.String())
	streamSvc, err := stream.NewService(ctx, db, cfg.DataDir, logger)
	if err != nil {
		logger.Error("failed to initialize streaming service", "error", err)
		os.Exit(1)
	}
	defer streamSvc.Shutdown()

	maintSvc := maintenance.NewService(ctx, db, maintenance.Options{
		DataDir:             cfg.DataDir,
		IsScanRunning:       libSvc.HasRunningScan,
		ActiveTranscodeDirs: streamSvc.ActiveTranscodeDirs,
		Logger:              logger,
	})
	go maintSvc.Run(ctx)

	handler := api.NewHandler(api.Dependencies{
		Config:      cfg,
		Store:       db,
		Auth:        authSvc,
		Library:     libSvc,
		Stream:      streamSvc,
		Requests:    reqSvc,
		Maintenance: maintSvc,
		Logger:      logger,
		StartedAt:   time.Now(),
	})

	srv := edge.New(cfg, handler, logger)
	if err := srv.Run(ctx); err != nil {
		logger.Error("edge server exited with error", "error", err)
		os.Exit(1)
	}
	logger.Info("lune server stopped cleanly")
}

func newLogger(level, format string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: lvl}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}
