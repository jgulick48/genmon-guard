package guard

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/jgulick48/genmon-guard/internal/alert"
	"github.com/jgulick48/genmon-guard/internal/genmon"
	"github.com/jgulick48/genmon-guard/internal/metrics"
)

// Genmon is the part of the genmon client the runner needs.
type Genmon interface {
	Status(ctx context.Context) (genmon.Status, error)
	AlarmLog(ctx context.Context) ([]genmon.LogEntry, error)
	SetRemote(ctx context.Context, cmd string) error
}

// DemandSource reports whether a generator start is being demanded.
type DemandSource interface {
	Demand(ctx context.Context) (bool, error)
}

// Metric names.
const (
	MetricRetries   = "genmon_guard_retries_total"
	MetricDemand    = "genmon_guard_demand"
	MetricDemandUp  = "genmon_guard_demand_up"
	MetricGenmonUp  = "genmon_guard_genmon_up"
	MetricLastAlarm = "genmon_guard_last_alarm_code"
	MetricDryRun    = "genmon_guard_dry_run"
	MetricAlertErrs = "genmon_guard_alert_errors_total"
)

// Runner polls the inputs, feeds the Machine and carries out its decisions.
type Runner struct {
	Machine      *Machine
	Genmon       Genmon
	Demand       DemandSource
	Alerter      alert.Alerter
	Metrics      *metrics.Registry
	Log          *slog.Logger
	PollInterval time.Duration
	ResetSettle  time.Duration // pause between resetalarm and auto
	ConfirmDelay time.Duration // pause after auto before reading Switch State back
	DryRun       bool

	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
	lastTick atomic.Int64 // unix nanos of the last completed poll
}

// DescribeMetrics registers all series so they export before the first update.
func DescribeMetrics(r *metrics.Registry) {
	for _, res := range []string{"sent", "recovered", "gave_up"} {
		r.Describe(MetricRetries, "Retry outcomes (sent includes dry-run would-resets).", true, metrics.Labels{"result", res})
	}
	r.Describe(MetricDemand, "1 when the inverter demands a generator start.", false, nil)
	r.Describe(MetricDemandUp, "1 when the start-demand source was readable on the last poll.", false, nil)
	r.Describe(MetricGenmonUp, "1 when genmon was readable on the last poll.", false, nil)
	r.Describe(MetricLastAlarm, "Alarm code of the newest genmon Alarm Log entry (0 if none).", false, nil)
	r.Describe(MetricDryRun, "1 when running in dry-run mode (no setremote commands sent).", false, nil)
	r.Describe(MetricAlertErrs, "Alerts that failed to send.", true, nil)
}

func (r *Runner) init() {
	if r.now == nil {
		r.now = time.Now
	}
	if r.sleep == nil {
		r.sleep = sleepCtx
	}
	if r.PollInterval <= 0 {
		r.PollInterval = 10 * time.Second
	}
	if r.ResetSettle <= 0 {
		r.ResetSettle = 3 * time.Second
	}
	if r.ConfirmDelay <= 0 {
		r.ConfirmDelay = 3 * time.Second
	}
	if r.Log == nil {
		r.Log = slog.Default()
	}
	if r.Metrics == nil {
		r.Metrics = metrics.New()
	}
}

// Run polls until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) {
	r.init()
	r.Metrics.SetBool(MetricDryRun, r.DryRun)
	r.lastTick.Store(r.now().UnixNano()) // healthy while the first poll runs
	t := time.NewTicker(r.PollInterval)
	defer t.Stop()
	for {
		r.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick runs one poll: read inputs, step the machine, act.
func (r *Runner) Tick(ctx context.Context) {
	r.init()
	in := r.observe(ctx)
	d := r.Machine.Step(in)
	if d.Reset && !r.DryRun {
		// Belt and braces: the machine never asks for a reset in dry run.
		status, err := r.reset(ctx)
		rd := r.Machine.ResetResult(r.now(), status, err)
		d.Alerts = append(d.Alerts, rd.Alerts...)
	}
	r.record(d)
	for _, a := range d.Alerts {
		if err := r.Alerter.Send(ctx, a); err != nil {
			r.Metrics.Inc(MetricAlertErrs, nil)
			r.Log.Error("sending alert failed", "title", a.Title, "err", err)
		}
	}
	r.lastTick.Store(r.now().UnixNano())
}

func (r *Runner) observe(ctx context.Context) Input {
	pollCtx, cancel := context.WithTimeout(ctx, r.PollInterval*3)
	defer cancel()
	in := Input{Now: r.now()}
	in.Demand, in.DemandErr = r.Demand.Demand(pollCtx)
	in.Status, in.StatusErr = r.Genmon.Status(pollCtx)
	if in.StatusErr == nil {
		var log []genmon.LogEntry
		log, in.AlarmErr = r.Genmon.AlarmLog(pollCtx)
		if len(log) > 0 {
			in.Alarm = log[0]
		}
	}

	r.Metrics.SetBool(MetricDemandUp, in.DemandErr == nil)
	if in.DemandErr == nil {
		r.Metrics.SetBool(MetricDemand, in.Demand)
	}
	genmonUp := in.StatusErr == nil && in.AlarmErr == nil
	r.Metrics.SetBool(MetricGenmonUp, genmonUp)
	if genmonUp {
		r.Metrics.Set(MetricLastAlarm, float64(in.Alarm.Code), nil)
	}
	r.Log.Debug("poll", "demand", in.Demand, "demand_err", in.DemandErr,
		"switch_state", in.Status.SwitchState, "engine_state", in.Status.EngineState, "rpm", in.Status.RPM,
		"genmon_err", firstErr(in.StatusErr, in.AlarmErr), "newest_alarm", in.Alarm.Raw)
	return in
}

// reset sends resetalarm, then auto, then reads the status back.
func (r *Runner) reset(ctx context.Context) (genmon.Status, error) {
	r.Log.Warn("sending setremote=resetalarm")
	if err := r.Genmon.SetRemote(ctx, "resetalarm"); err != nil {
		return genmon.Status{}, err
	}
	if err := r.sleep(ctx, r.ResetSettle); err != nil {
		return genmon.Status{}, err
	}
	r.Log.Warn("sending setremote=auto")
	if err := r.Genmon.SetRemote(ctx, "auto"); err != nil {
		return genmon.Status{}, err
	}
	if err := r.sleep(ctx, r.ConfirmDelay); err != nil {
		return genmon.Status{}, err
	}
	return r.Genmon.Status(ctx)
}

func (r *Runner) record(d Decision) {
	if d.RetrySent {
		r.Metrics.Inc(MetricRetries, metrics.Labels{"result", "sent"})
	}
	if d.Recovered {
		r.Metrics.Inc(MetricRetries, metrics.Labels{"result", "recovered"})
	}
	if d.GaveUp {
		r.Metrics.Inc(MetricRetries, metrics.Labels{"result", "gave_up"})
	}
}

// HealthHandler answers 200 while the poll loop is ticking, 503 otherwise.
// It reports the loop's liveness, not genmon's; genmon outages are alerted and
// exported as genmon_guard_genmon_up instead of restarting the pod.
func (r *Runner) HealthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		r.init()
		last := r.lastTick.Load()
		if last == 0 || r.now().Sub(time.Unix(0, last)) > 5*r.PollInterval+time.Minute {
			http.Error(w, "poll loop stalled", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
