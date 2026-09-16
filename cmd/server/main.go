package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"catpaw2api/internal/auth"
	"catpaw2api/internal/pool"
	"catpaw2api/internal/scheduler"
	"catpaw2api/internal/server"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)
	if len(auths) == 0 {
		log.Printf("WARNING: no accounts found. Run `catpaw2api-login` first.")
	}

	if cfg.StateFile != "" {
		_ = os.MkdirAll(filepath.Dir(cfg.StateFile), 0o700)
	}
	p, err := pool.New(auths, pool.Config{
		ErrThreshold:    cfg.Cooldown.ErrThresh,
		ErrCooldown:     cfg.ErrCooldownDur,
		SoftCooldown:    cfg.SoftRateDur,
		UpstreamTimeout: time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second,
	}, cfg.StateFile)
	if err != nil {
		log.Fatalf("build pool: %v", err)
	}

	sch := scheduler.New(scheduler.Config{
		Pool:            p,
		Enabled:         cfg.Quota.Enabled,
		PollInterval:    time.Duration(cfg.Quota.PollMinutes) * time.Minute,
		ApplyThreshold:  cfg.Quota.ApplyThreshold,
		ApplyMethod:     cfg.Quota.ApplyMethod,
		ApplyCooldown:   time.Duration(cfg.Quota.ApplyCooldownHour) * time.Hour,
		RegisterOnStart: cfg.Quota.RegisterOnStart,
		AutoRenew:       cfg.Quota.AutoRenew,
		RenewThreshold:  time.Duration(cfg.Quota.RenewThresholdHours) * time.Hour,
		RenewWaitMax:    15 * time.Minute,
		AuthDir:         cfg.AuthDir,
	})

	convStateFile := ""
	if cfg.StateFile != "" {
		convStateFile = cfg.StateFile + ".conv.json"
	}
	h := server.NewHandler(server.Config{
		Pool:          p,
		APIKey:        cfg.APIKey,
		AuthDir:       cfg.AuthDir,
		MaxRotate:     3,
		SoftCooldown:  cfg.SoftRateDur,
		ErrThreshold:  cfg.Cooldown.ErrThresh,
		ErrCooldown:   cfg.ErrCooldownDur,
		DefaultModel:  cfg.DefaultModel,
		ConvStateFile: convStateFile,
		QuotaInfo: map[string]any{
			"enabled":               cfg.Quota.Enabled,
			"poll_minutes":          cfg.Quota.PollMinutes,
			"apply_threshold":       cfg.Quota.ApplyThreshold,
			"apply_method":          cfg.Quota.ApplyMethod,
			"apply_cooldown_hours":  cfg.Quota.ApplyCooldownHour,
			"register_on_start":     cfg.Quota.RegisterOnStart,
			"auto_renew":            cfg.Quota.AutoRenew,
			"renew_threshold_hours": cfg.Quota.RenewThresholdHours,
		},
		RenewStatuses: func() []any {
			statuses := sch.RenewStatuses()
			out := make([]any, 0, len(statuses))
			for _, s := range statuses {
				out = append(out, s)
			}
			return out
		},
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("http shutdown: %v", err)
		}
	}()

	log.Printf("catpaw2api listening on %s (authentication enabled)", cfg.Listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}
