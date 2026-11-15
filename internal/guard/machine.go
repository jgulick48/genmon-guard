// Package guard holds genmon-guard's decision logic. Machine is a pure state
// machine: it is fed one Input per poll and returns a Decision describing the
// alerts to send and whether to reset the controller. It performs no I/O, which
// keeps every decision table-testable. Runner (runner.go) does the I/O.
package guard

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jgulick48/genmon-guard/internal/alert"
	"github.com/jgulick48/genmon-guard/internal/genmon"
)

// Config tunes the state machine.
type Config struct {
	RetryDelay       time.Duration // wait after a failed start before resetting
	SuccessHold      time.Duration // engine must run this long to count as recovered
	StartupGrace     time.Duration // alarms older than this at startup count as handled
	OutageAlertAfter time.Duration // alert once an input has been unreadable this long
	MaxRetries       int           // reset attempts allowed per demand period
	RetryAlarmCodes  []int         // alarm codes that may be reset (default 1500)
	RunningRPM       int           // RPM at or above which the engine counts as running
	DryRun           bool          // decide and alert, but never send setremote
}

// Input is one poll's observations. An error field set means that source could
// not be read and its values must not be trusted.
type Input struct {
	Now time.Time

	Demand    bool
	DemandErr error

	Status    genmon.Status
	StatusErr error

	Alarm    genmon.LogEntry // newest Alarm Log entry; zero when the log is empty
	AlarmErr error
}

// Decision is what the runner must do after a Step.
type Decision struct {
	Alerts []alert.Alert

	// Reset asks the runner to send setremote=resetalarm then setremote=auto,
	// and report the outcome with Machine.ResetResult. Never set in dry run.
	Reset   bool
	Attempt int // retry number for Reset (1-based)

	// Metric events.
	RetrySent bool // a reset was sent (or would have been, in dry run)
	Recovered bool
	GaveUp    bool
}

type pendingRetry struct {
	entry  genmon.LogEntry
	fireAt time.Time
}

type source struct {
	name      string
	downSince time.Time
	alerted   bool
}

// Machine is the retry state machine. It is not safe for concurrent use.
type Machine struct {
	cfg Config
	log *slog.Logger

	started bool
	handled genmon.LogEntry // newest alarm-log entry already dealt with

	demandKnown  bool
	demand       bool
	retries      int  // resets sent during the current demand period
	gaveUp       bool // stop resetting until demand drops
	pending      *pendingRetry
	runningSince time.Time

	genmonSrc source
	demandSrc source
}

// New returns a Machine. Zero config values get the documented defaults.
func New(cfg Config, log *slog.Logger) *Machine {
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = 3 * time.Minute
	}
	if cfg.SuccessHold <= 0 {
		cfg.SuccessHold = 2 * time.Minute
	}
	if cfg.StartupGrace <= 0 {
		cfg.StartupGrace = 5 * time.Minute
	}
	if cfg.OutageAlertAfter <= 0 {
		cfg.OutageAlertAfter = 10 * time.Minute
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	}
	if len(cfg.RetryAlarmCodes) == 0 {
		cfg.RetryAlarmCodes = []int{1500}
	}
	if cfg.RunningRPM <= 0 {
		cfg.RunningRPM = 1500
	}
	if log == nil {
		log = slog.Default()
	}
	return &Machine{
		cfg:       cfg,
		log:       log,
		genmonSrc: source{name: "genmon"},
		demandSrc: source{name: "start-demand source"},
	}
}

// Retries returns the number of resets sent in the current demand period.
func (m *Machine) Retries() int { return m.retries }

// Step feeds one poll's observations to the machine.
func (m *Machine) Step(in Input) Decision {
	var d Decision
	genmonErr := firstErr(in.StatusErr, in.AlarmErr)
	m.trackOutage(&d, &m.genmonSrc, genmonErr, in)
	m.trackOutage(&d, &m.demandSrc, in.DemandErr, in)

	if in.DemandErr == nil {
		m.observeDemand(&d, in)
	}
	if genmonErr != nil {
		// Fail closed: without a trustworthy view of the controller, do nothing.
		return d
	}

	if !m.started {
		m.started = true
		m.startup(&d, in)
	}

	m.trackRunning(&d, in)

	if m.pending != nil && !m.stepPending(&d, in) {
		return d
	}

	if !in.Alarm.IsZero() && newer(in.Alarm, m.handled) {
		m.handled = in.Alarm
		m.newAlarm(&d, in)
	}
	return d
}

// ResetResult reports the outcome of a Reset decision: err is the setremote
// error, if any, and status is the controller status read afterwards.
func (m *Machine) ResetResult(now time.Time, status genmon.Status, err error) Decision {
	var d Decision
	in := Input{Now: now, Status: status}
	switch {
	case err != nil:
		m.giveUp()
		m.log.Error("reset command failed", "err", err, "attempt", m.retries)
		d.Alerts = append(d.Alerts, m.newAlert(alert.Error, in,
			"Reset failed: needs manual reset",
			fmt.Sprintf("Sending resetalarm/auto failed: %v. Not retrying again until the start demand ends.", err)))
	case !strings.EqualFold(status.SwitchState, genmon.SwitchAuto) && !m.running(status):
		m.giveUp()
		m.log.Error("switch state not Auto after reset", "switch_state", status.SwitchState, "attempt", m.retries)
		d.Alerts = append(d.Alerts, m.newAlert(alert.Error, in,
			"Reset did not return to Auto: needs manual reset",
			fmt.Sprintf("Switch State is %q after resetalarm+auto (expected Auto). Not retrying again until the start demand ends.", status.SwitchState)))
	default:
		m.log.Info("reset confirmed, controller back in Auto", "switch_state", status.SwitchState,
			"engine_state", status.EngineState, "attempt", m.retries)
	}
	return d
}

func (m *Machine) giveUp() {
	m.gaveUp = true
	m.pending = nil
}

func (m *Machine) trackOutage(d *Decision, s *source, err error, in Input) {
	if err == nil {
		if s.alerted {
			m.log.Info("input readable again", "source", s.name)
			d.Alerts = append(d.Alerts, m.newAlert(alert.Info, in,
				fmt.Sprintf("%s reachable again", capitalize(s.name)),
				fmt.Sprintf("%s was unreadable for %s.", capitalize(s.name), in.Now.Sub(s.downSince).Round(time.Second))))
		}
		s.downSince, s.alerted = time.Time{}, false
		return
	}
	if s.downSince.IsZero() {
		s.downSince = in.Now
		m.log.Warn("input unreadable; taking no action", "source", s.name, "err", err)
	}
	if !s.alerted && in.Now.Sub(s.downSince) >= m.cfg.OutageAlertAfter {
		s.alerted = true
		d.Alerts = append(d.Alerts, m.newAlert(alert.Warning, in,
			fmt.Sprintf("%s unreachable", capitalize(s.name)),
			fmt.Sprintf("Cannot read %s for %s: %v. genmon-guard is taking no action until it is readable again.",
				s.name, in.Now.Sub(s.downSince).Round(time.Second), err)))
	}
}

func (m *Machine) observeDemand(d *Decision, in Input) {
	was, known := m.demand, m.demandKnown
	m.demand, m.demandKnown = in.Demand, true
	if !known || was == in.Demand {
		return
	}
	if in.Demand {
		m.log.Info("start demand began")
		return
	}
	m.log.Info("start demand ended; resetting retry counter", "retries", m.retries, "gave_up", m.gaveUp)
	if m.pending != nil {
		m.log.Warn("start demand ended before the retry; not resetting", "alarm", m.pending.entry.Raw)
		d.Alerts = append(d.Alerts, m.newAlertFor(alert.Warning, in, m.pending.entry,
			"Start demand ended before retry: needs manual reset",
			"The start demand dropped during the retry delay, so the alarm was not reset. The controller stays in alarm and will not start on the next demand until it is reset."))
	}
	m.retries, m.gaveUp, m.pending = 0, false, nil
}

func (m *Machine) startup(d *Decision, in Input) {
	if in.Alarm.IsZero() || in.Now.Sub(in.Alarm.Time) <= m.cfg.StartupGrace {
		return
	}
	m.handled = in.Alarm
	m.log.Info("startup: treating newest alarm as already handled", "alarm", in.Alarm.Raw, "grace", m.cfg.StartupGrace)
	if in.Status.InAlarm() {
		d.Alerts = append(d.Alerts, m.newAlertFor(alert.Warning, in, in.Alarm,
			"Controller in alarm at startup",
			fmt.Sprintf("The newest alarm is older than the startup grace (%s), so genmon-guard will not reset it. It needs a manual reset.", m.cfg.StartupGrace)))
	}
}

func (m *Machine) running(s genmon.Status) bool {
	es := strings.ToLower(s.EngineState)
	return strings.Contains(es, "running") || strings.Contains(es, "exercising") || s.RPM >= m.cfg.RunningRPM
}

func (m *Machine) trackRunning(d *Decision, in Input) {
	if !m.running(in.Status) {
		m.runningSince = time.Time{}
		return
	}
	if m.runningSince.IsZero() {
		m.runningSince = in.Now
		m.log.Info("engine running", "engine_state", in.Status.EngineState, "rpm", in.Status.RPM)
	}
	if m.retries > 0 && in.Now.Sub(m.runningSince) >= m.cfg.SuccessHold {
		m.log.Info("recovered", "retries", m.retries)
		d.Alerts = append(d.Alerts, m.newAlert(alert.Info, in,
			fmt.Sprintf("Recovered after %d %s", m.retries, plural(m.retries, "retry", "retries")),
			fmt.Sprintf("Engine has been running for %s (%s, %d RPM).", m.cfg.SuccessHold, in.Status.EngineState, in.Status.RPM)))
		d.Recovered = true
		m.retries, m.gaveUp = 0, false
	}
}

// stepPending advances a scheduled retry. It returns true when Step should go
// on to look for a new alarm entry.
func (m *Machine) stepPending(d *Decision, in Input) bool {
	p := m.pending
	if in.Alarm.Raw != p.entry.Raw || !in.Status.InAlarm() {
		m.log.Info("alarm changed or cleared before the retry; cancelling it",
			"pending", p.entry.Raw, "newest", in.Alarm.Raw, "switch_state", in.Status.SwitchState)
		m.pending = nil
		return true
	}
	if in.DemandErr != nil || !m.demandKnown || !m.demand {
		// Demand unknown: hold; demand drop is handled in observeDemand.
		return false
	}
	if in.Now.Before(p.fireAt) {
		return false
	}
	m.pending = nil
	m.retries++
	d.RetrySent = true
	d.Attempt = m.retries
	verb := "Resetting alarm and returning to Auto"
	if m.cfg.DryRun {
		verb = "Would reset alarm and return to Auto (dry run, nothing sent)"
	} else {
		d.Reset = true
	}
	m.log.Warn("retrying start", "attempt", m.retries, "max", m.cfg.MaxRetries, "dry_run", m.cfg.DryRun, "alarm", p.entry.Raw)
	d.Alerts = append(d.Alerts, m.newAlertFor(alert.Warning, in, p.entry,
		fmt.Sprintf("Retry attempt %d/%d", m.retries, m.cfg.MaxRetries),
		verb+". Start demand is still active, so the controller should crank again."))
	return false
}

func (m *Machine) newAlarm(d *Decision, in Input) {
	a := in.Alarm
	log := m.log.With("alarm", a.Raw, "code", a.Code)
	if !slices.Contains(m.cfg.RetryAlarmCodes, a.Code) {
		log.Warn("alarm is not retryable; alert only")
		d.Alerts = append(d.Alerts, m.newAlertFor(alert.Error, in, a,
			fmt.Sprintf("Generator alarm: %s (code %d)", a.Text, a.Code),
			"This alarm is never reset automatically. Check the generator."))
		return
	}
	reason := ""
	switch {
	case !in.Status.InAlarm():
		reason = fmt.Sprintf("the controller is not in alarm (Switch State %q)", in.Status.SwitchState)
	case in.DemandErr != nil || !m.demandKnown:
		reason = "the start-demand signal can't be read"
	case !in.Demand:
		reason = "no start is demanded"
	case m.gaveUp:
		reason = "genmon-guard already gave up during this demand period"
	}
	if reason != "" {
		log.Warn("retryable alarm but not retrying", "reason", reason)
		d.Alerts = append(d.Alerts, m.newAlertFor(alert.Error, in, a,
			fmt.Sprintf("%s: not retrying", a.Text),
			fmt.Sprintf("Not resetting because %s. It may need a manual reset.", reason)))
		return
	}
	if m.retries >= m.cfg.MaxRetries {
		m.giveUp()
		d.GaveUp = true
		log.Error("gave up", "retries", m.retries)
		d.Alerts = append(d.Alerts, m.newAlertFor(alert.Error, in, a,
			"Gave up: needs manual reset",
			fmt.Sprintf("The start failed again after %d %s. Switch the generator Off and back to Auto by hand.",
				m.retries, plural(m.retries, "retry", "retries"))))
		return
	}
	m.pending = &pendingRetry{entry: a, fireAt: in.Now.Add(m.cfg.RetryDelay)}
	log.Warn("failed start with demand active; retry scheduled", "delay", m.cfg.RetryDelay, "next_attempt", m.retries+1)
	d.Alerts = append(d.Alerts, m.newAlertFor(alert.Warning, in, a,
		fmt.Sprintf("Start failed: %s", a.Text),
		fmt.Sprintf("Start demand is active. Retry %d/%d in %s if the demand is still active.",
			m.retries+1, m.cfg.MaxRetries, m.cfg.RetryDelay)))
}

func (m *Machine) newAlert(level alert.Level, in Input, title, text string) alert.Alert {
	return alert.Alert{Level: level, Title: title, Text: text,
		BatteryVoltage: in.Status.BatteryVoltage, Time: in.Now, DryRun: m.cfg.DryRun}
}

func (m *Machine) newAlertFor(level alert.Level, in Input, e genmon.LogEntry, title, text string) alert.Alert {
	a := m.newAlert(level, in, title, text)
	a.AlarmLine = e.Raw
	return a
}

// newer reports whether e is a newer alarm-log entry than h.
func newer(e, h genmon.LogEntry) bool {
	if h.IsZero() {
		return true
	}
	return e.Time.After(h.Time) || (e.Time.Equal(h.Time) && e.Raw != h.Raw)
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
