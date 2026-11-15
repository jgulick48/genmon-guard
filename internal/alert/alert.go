// Package alert sends genmon-guard notifications. Alerter is an interface so
// other transports (ntfy, MQTT, ...) can be added next to Slack.
package alert

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Level is the severity of an alert.
type Level string

const (
	Info    Level = "info"
	Warning Level = "warning"
	Error   Level = "error"
)

// Alert is one notification.
type Alert struct {
	Level          Level
	Title          string    // short headline, e.g. "Retry attempt 1/3"
	Text           string    // details
	AlarmLine      string    // raw genmon alarm-log line, if any
	BatteryVoltage string    // e.g. "13.20 V", if known
	Time           time.Time // when the decision was made
	DryRun         bool      // the service is in dry-run mode
}

// Alerter delivers alerts.
type Alerter interface {
	Send(ctx context.Context, a Alert) error
}

// Multi fans an alert out to several alerters and joins their errors.
type Multi []Alerter

func (m Multi) Send(ctx context.Context, a Alert) error {
	var errs []error
	for _, x := range m {
		if err := x.Send(ctx, a); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Log writes alerts to a slog.Logger. It is always enabled so alerts are visible
// even when no webhook is configured.
type Log struct{ Logger *slog.Logger }

func (l Log) Send(_ context.Context, a Alert) error {
	lvl := slog.LevelInfo
	switch a.Level {
	case Warning:
		lvl = slog.LevelWarn
	case Error:
		lvl = slog.LevelError
	}
	l.Logger.Log(context.Background(), lvl, "alert",
		"title", a.Title, "text", a.Text, "alarm_line", a.AlarmLine,
		"battery_voltage", a.BatteryVoltage, "dry_run", a.DryRun)
	return nil
}
