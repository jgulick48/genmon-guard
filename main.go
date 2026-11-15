// Command genmon-guard watches genmon and, when the generator fails a start with
// RPM Sense Loss while the inverter still demands a start, resets the alarm and
// returns the controller to Auto so it cranks again, up to a retry limit.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // the distroless image has no zoneinfo guarantee

	"github.com/jgulick48/genmon-guard/internal/alert"
	"github.com/jgulick48/genmon-guard/internal/config"
	"github.com/jgulick48/genmon-guard/internal/demand"
	"github.com/jgulick48/genmon-guard/internal/genmon"
	"github.com/jgulick48/genmon-guard/internal/guard"
	"github.com/jgulick48/genmon-guard/internal/metrics"
)

// Set via -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = "none"
)

var configPath = flag.String("config", "", "Path to the YAML config file (optional; env GENMON_GUARD_* overrides).")

func main() {
	flag.Parse()
	os.Exit(run())
}

func run() int {
	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("invalid config", "err", err)
		return 1
	}
	log := newLogger(cfg)
	slog.SetDefault(log)
	loc, _ := cfg.Location() // validated by Load

	log.Info("genmon-guard starting", "version", version, "commit", commit,
		"dry_run", *cfg.Reset.DryRun, "genmon", cfg.Genmon.URL, "demand", cfg.Demand.MetricsURL,
		"max_retries", *cfg.Reset.MaxRetries, "retry_delay", cfg.Reset.RetryDelay.String(),
		"alarm_codes", cfg.Reset.AlarmCodes, "timezone", loc.String())
	if *cfg.Reset.DryRun {
		log.Warn("DRY RUN: setremote commands will be logged and alerted as \"would reset\", never sent")
	}
	if cfg.GenmonPassword == "" || cfg.Genmon.Username == "" {
		log.Warn("genmon username or password not set; login will fail if genmon requires it")
	}

	gc, err := genmon.New(genmon.Config{
		URL: cfg.Genmon.URL, Username: cfg.Genmon.Username, Password: cfg.GenmonPassword,
		InsecureSkipVerify: cfg.Genmon.InsecureSkipVerify, CAFile: cfg.Genmon.CAFile,
		Location: loc, Timeout: cfg.Genmon.Timeout,
	})
	if err != nil {
		log.Error("genmon client", "err", err)
		return 1
	}

	alerters := alert.Multi{alert.Log{Logger: log}}
	if cfg.SlackWebhookURL != "" {
		alerters = append(alerters, alert.NewSlack(cfg.SlackWebhookURL, nil, loc))
	} else {
		log.Warn("no Slack webhook configured; alerts go to the log only")
	}

	reg := metrics.New()
	guard.DescribeMetrics(reg)
	runner := &guard.Runner{
		Machine: guard.New(guard.Config{
			RetryDelay: cfg.Reset.RetryDelay, SuccessHold: cfg.Reset.SuccessHold, StartupGrace: cfg.Reset.StartupGrace,
			OutageAlertAfter: cfg.OutageAlertAfter, MaxRetries: *cfg.Reset.MaxRetries,
			RetryAlarmCodes: cfg.Reset.AlarmCodes, RunningRPM: cfg.Reset.RunningRPM, DryRun: *cfg.Reset.DryRun,
		}, log),
		Genmon: gc,
		Demand: demand.New(demand.Config{URL: cfg.Demand.MetricsURL, Metric: cfg.Demand.Metric,
			Labels: cfg.Demand.Labels, Timeout: cfg.Demand.Timeout}, nil),
		Alerter:      alerters,
		Metrics:      reg,
		Log:          log,
		PollInterval: cfg.PollInterval,
		ResetSettle:  cfg.Reset.Settle,
		ConfirmDelay: cfg.Reset.ConfirmDelay,
		DryRun:       *cfg.Reset.DryRun,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.Handle("/metrics", reg)
	mux.Handle("/healthz", runner.HealthHandler())
	server := &http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serverErr := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	done := make(chan struct{})
	go func() { runner.Run(ctx); close(done) }()

	exit := 0
	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-serverErr:
		log.Error("http server failed", "err", err)
		exit = 1
		stop()
	}
	<-done
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	log.Info("shutdown complete")
	return exit
}

func newLogger(cfg *config.Config) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(strings.ToUpper(cfg.LogLevel))); err != nil {
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}
