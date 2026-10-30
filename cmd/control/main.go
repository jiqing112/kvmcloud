// kvmcloud-control 控制面：HTTP API + gRPC 注册/心跳 + 水位监控。
// 配置仅从环境变量读取（凭据不落盘）。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/kvm-cloud/platform/internal/api"
	"github.com/kvm-cloud/platform/internal/auth"
	"github.com/kvm-cloud/platform/internal/grpcserver"
	"github.com/kvm-cloud/platform/internal/monitor"
	"github.com/kvm-cloud/platform/internal/store"
	"github.com/kvm-cloud/platform/internal/updater"
)

var (
	version = "dev"
	commit  = "none"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-version") {
		fmt.Printf("kvmcloud-control %s (commit %s)\n", version, commit)
		return
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Error("DATABASE_URL is required (e.g. postgres://user:pass@host:5432/kvmcloud)")
		os.Exit(1)
	}
	httpAddr := envOr("CONTROL_HTTP_ADDR", ":8080")
	grpcAddr := envOr("CONTROL_GRPC_ADDR", ":9443")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.New(ctx, dsn)
	if err != nil {
		log.Error("connect database", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	if err := st.Migrate(ctx); err != nil {
		log.Error("migrate database", "err", err)
		os.Exit(1)
	}
	log.Info("database ready", "version", version, "commit", commit)

	mon := monitor.New(st, monitor.Thresholds{
		Alert:              envFloat("OVERCOMMIT_ALERT_THRESHOLD", monitor.DefaultAlertThreshold),
		DiskFullProtection: envFloat("DISK_FULL_PROTECT_THRESHOLD", 0.95),
	}, log, envDur("MONITOR_INTERVAL", 10*time.Second))

	joinAuth := auth.NewValidator(func() *auth.JoinTokenConfig {
		cfg, err := auth.LoadFromEnv()
		if err != nil {
			log.Warn("join token not configured; node registration disabled until CONTROL_JOIN_TOKEN is set", "err", err)
			return nil
		}
		return cfg
	}())

	go mon.Run(ctx)

	// --- 本体自动更新：定时检测新版本，auto 模式后台自更新（失败自动回滚） ---
	selfUpdate, err := updater.FromEnv("control", version, "cloud-control.service", log)
	if err != nil {
		log.Warn("auto update disabled", "err", err)
	} else if selfUpdate != nil {
		go selfUpdate.Run(ctx)
		log.Info("auto updater enabled", "mode", "on", "channel", os.Getenv("KC_UPDATE_CHANNEL"))
	}

	gs := grpcserver.New(st, mon, joinAuth, log)
	go func() {
		if err := gs.Listen(ctx, grpcAddr); err != nil {
			log.Error("grpc server", "err", err)
			stop()
		}
	}()
	go gs.Watchdog(ctx, envDur("NODE_STALE_AFTER", 90*time.Second), envDur("NODE_WATCHDOG_INTERVAL", 30*time.Second))

	hs := api.New(st, mon, log, version)
	if err := hs.ListenAndServe(ctx, httpAddr); err != nil {
		log.Error("http server", "err", err)
		os.Exit(1)
	}
	log.Info("control plane stopped")
}
