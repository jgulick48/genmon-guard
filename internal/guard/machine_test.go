package guard

import (
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jgulick48/genmon-guard/internal/genmon"
)

var (
	t0      = time.Date(2026, 11, 15, 7, 0, 0, 0, time.UTC)
	rpmLoss = entry(t0.Add(59*time.Second), "RPM Sense Loss", 1500)
	errDown = errors.New("connection refused")
)

func entry(at time.Time, text string, code int) genmon.LogEntry {
	return genmon.LogEntry{
		Raw:  at.Format("01/02/06 15:04:05") + " " + text + " : Alarm Code: " + strconv.Itoa(code),
		Time: at, Text: text, Code: code,
	}
}

var (
	auto    = genmon.Status{SwitchState: "Auto", EngineState: "Stopped", BatteryVoltage: "13.20 V"}
	inAlarm = genmon.Status{SwitchState: "System in Alarm", EngineState: "Stopped - Alarm", BatteryVoltage: "12.40 V"}
	running = genmon.Status{SwitchState: "Auto", EngineState: "Running", RPM: 1800, BatteryVoltage: "14.10 V"}
)

// step is one poll in a scenario.
type step struct {
	at     time.Duration // offset from t0
	demand bool
	dErr   error
	status genmon.Status
	gErr   error
	alarm  genmon.LogEntry

	// reset outcome to report when the step yields Reset (default: Auto, no error)
	resetStatus *genmon.Status
	resetErr    error

	// expectations
	wantReset    bool
	wantRetry    bool
	wantGaveUp   bool
	wantRecover  bool
	wantAlerts   []string // substrings of alert titles, in order; nil = no alerts
	wantRetries  int
	skipRetryChk bool
}

func newTestMachine(dry bool) *Machine {
	return New(Config{
		RetryDelay: 3 * time.Minute, SuccessHold: 2 * time.Minute, StartupGrace: 5 * time.Minute,
		OutageAlertAfter: 10 * time.Minute, MaxRetries: 3, RetryAlarmCodes: []int{1500}, DryRun: dry,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func run(t *testing.T, m *Machine, steps []step) {
	t.Helper()
	for i, s := range steps {
		in := Input{Now: t0.Add(s.at), Demand: s.demand, DemandErr: s.dErr, Status: s.status,
			StatusErr: s.gErr, Alarm: s.alarm, AlarmErr: s.gErr}
		d := m.Step(in)
		if d.Reset {
			rs := auto
			if s.resetStatus != nil {
				rs = *s.resetStatus
			}
			r := m.ResetResult(in.Now, rs, s.resetErr)
			d.Alerts = append(d.Alerts, r.Alerts...)
		}
		var titles []string
		for _, a := range d.Alerts {
			titles = append(titles, a.Title)
		}
		if d.Reset != s.wantReset || d.RetrySent != s.wantRetry || d.GaveUp != s.wantGaveUp || d.Recovered != s.wantRecover {
			t.Errorf("step %d (+%s): reset=%v retry=%v gaveUp=%v recovered=%v, want %v %v %v %v; alerts %q",
				i, s.at, d.Reset, d.RetrySent, d.GaveUp, d.Recovered, s.wantReset, s.wantRetry, s.wantGaveUp, s.wantRecover, titles)
		}
		if len(titles) != len(s.wantAlerts) {
			t.Errorf("step %d (+%s): alerts %q, want %q", i, s.at, titles, s.wantAlerts)
		} else {
			for j := range titles {
				if !strings.Contains(titles[j], s.wantAlerts[j]) {
					t.Errorf("step %d (+%s): alert %d = %q, want it to contain %q", i, s.at, j, titles[j], s.wantAlerts[j])
				}
			}
		}
		if !s.skipRetryChk && m.Retries() != s.wantRetries {
			t.Errorf("step %d (+%s): retries = %d, want %d", i, s.at, m.Retries(), s.wantRetries)
		}
	}
}

const m1 = time.Minute

func TestMachine(t *testing.T) {
	fail2 := entry(t0.Add(5*m1), "RPM Sense Loss", 1500)
	fail3 := entry(t0.Add(10*m1), "RPM Sense Loss", 1500)
	fail4 := entry(t0.Add(15*m1), "RPM Sense Loss", 1500)
	eStop := entry(t0.Add(time.Minute), "E-Stop", 2800)
	old := entry(t0.Add(-2*time.Hour), "RPM Sense Loss", 1500)

	tests := []struct {
		name  string
		dry   bool
		steps []step
	}{
		{
			name: "failure with demand active retries after the delay",
			steps: []step{
				{at: 0, demand: true, status: auto},
				{at: m1, demand: true, status: inAlarm, alarm: rpmLoss, wantAlerts: []string{"Start failed: RPM Sense Loss"}},
				{at: 2 * m1, demand: true, status: inAlarm, alarm: rpmLoss},
				{at: 4*m1 - time.Second, demand: true, status: inAlarm, alarm: rpmLoss},
				{at: 4 * m1, demand: true, status: inAlarm, alarm: rpmLoss, wantReset: true, wantRetry: true, wantRetries: 1, wantAlerts: []string{"Retry attempt 1/3"}},
				// same entry afterwards is never retried again
				{at: 4*m1 + 10*time.Second, demand: true, status: auto, alarm: rpmLoss, wantRetries: 1},
			},
		},
		{
			name: "failure with no demand is alert only",
			steps: []step{
				{at: 0, demand: false, status: auto},
				{at: m1, demand: false, status: inAlarm, alarm: rpmLoss, wantAlerts: []string{"not retrying"}},
				{at: 10 * m1, demand: false, status: inAlarm, alarm: rpmLoss},
				{at: 11 * m1, demand: true, status: inAlarm, alarm: rpmLoss},
			},
		},
		{
			name: "demand dropping during the delay cancels the reset",
			steps: []step{
				{at: 0, demand: true, status: auto},
				{at: m1, demand: true, status: inAlarm, alarm: rpmLoss, wantAlerts: []string{"Start failed"}},
				{at: 2 * m1, demand: false, status: inAlarm, alarm: rpmLoss, wantAlerts: []string{"demand ended before retry"}},
				{at: 4 * m1, demand: false, status: inAlarm, alarm: rpmLoss},
				{at: 5 * m1, demand: true, status: inAlarm, alarm: rpmLoss},
			},
		},
		{
			name: "repeated failures give up once, then nothing until demand ends",
			steps: []step{
				{at: 0, demand: true, status: auto},
				{at: m1, demand: true, status: inAlarm, alarm: rpmLoss, wantAlerts: []string{"Start failed"}},
				{at: 4 * m1, demand: true, status: inAlarm, alarm: rpmLoss, wantReset: true, wantRetry: true, wantRetries: 1, wantAlerts: []string{"Retry attempt 1/3"}},
				{at: 5 * m1, demand: true, status: inAlarm, alarm: fail2, wantRetries: 1, wantAlerts: []string{"Start failed"}},
				{at: 8 * m1, demand: true, status: inAlarm, alarm: fail2, wantReset: true, wantRetry: true, wantRetries: 2, wantAlerts: []string{"Retry attempt 2/3"}},
				{at: 10 * m1, demand: true, status: inAlarm, alarm: fail3, wantRetries: 2, wantAlerts: []string{"Start failed"}},
				{at: 13 * m1, demand: true, status: inAlarm, alarm: fail3, wantReset: true, wantRetry: true, wantRetries: 3, wantAlerts: []string{"Retry attempt 3/3"}},
				{at: 15 * m1, demand: true, status: inAlarm, alarm: fail4, wantRetries: 3, wantGaveUp: true, wantAlerts: []string{"Gave up"}},
				{at: 20 * m1, demand: true, status: inAlarm, alarm: fail4, wantRetries: 3},
				{at: 30 * m1, demand: true, status: inAlarm, alarm: fail4, wantRetries: 3},
				{at: 40 * m1, demand: false, status: inAlarm, alarm: fail4, wantRetries: 0},
			},
		},
		{
			name: "success after a retry resets the counter",
			steps: []step{
				{at: 0, demand: true, status: auto},
				{at: m1, demand: true, status: inAlarm, alarm: rpmLoss, wantAlerts: []string{"Start failed"}},
				{at: 4 * m1, demand: true, status: inAlarm, alarm: rpmLoss, wantReset: true, wantRetry: true, wantRetries: 1, wantAlerts: []string{"Retry attempt 1/3"}},
				{at: 4*m1 + 20*time.Second, demand: true, status: running, alarm: rpmLoss, wantRetries: 1},
				{at: 6 * m1, demand: true, status: running, alarm: rpmLoss, wantRetries: 1},
				{at: 6*m1 + 20*time.Second, demand: true, status: running, alarm: rpmLoss, wantRecover: true, wantRetries: 0, wantAlerts: []string{"Recovered after 1 retry"}},
				{at: 10 * m1, demand: true, status: running, alarm: rpmLoss},
			},
		},
		{
			name: "non-retryable alarm is alert only",
			steps: []step{
				{at: 0, demand: true, status: auto},
				{at: m1, demand: true, status: inAlarm, alarm: eStop, wantAlerts: []string{"Generator alarm: E-Stop (code 2800)"}},
				{at: 5 * m1, demand: true, status: inAlarm, alarm: eStop},
			},
		},
		{
			name: "alarm older than startup grace is treated as handled",
			steps: []step{
				{at: 0, demand: true, status: inAlarm, alarm: old, wantAlerts: []string{"in alarm at startup"}},
				{at: 5 * m1, demand: true, status: inAlarm, alarm: old},
			},
		},
		{
			name: "recent alarm at startup is still handled",
			steps: []step{
				{at: 2 * m1, demand: true, status: inAlarm, alarm: rpmLoss, wantAlerts: []string{"Start failed"}},
				{at: 5 * m1, demand: true, status: inAlarm, alarm: rpmLoss, wantReset: true, wantRetry: true, wantRetries: 1, wantAlerts: []string{"Retry attempt 1/3"}},
			},
		},
		{
			name: "genmon unreachable: no action, one alert after the threshold, recovery notice",
			steps: []step{
				{at: 0, demand: true, status: auto},
				{at: m1, demand: true, gErr: errDown},
				{at: 10 * m1, demand: true, gErr: errDown},
				{at: 11 * m1, demand: true, gErr: errDown, wantAlerts: []string{"Genmon unreachable"}},
				{at: 12 * m1, demand: true, gErr: errDown},
				{at: 13 * m1, demand: true, status: auto, wantAlerts: []string{"Genmon reachable again"}},
			},
		},
		{
			name: "genmon unreachable during the delay holds the reset",
			steps: []step{
				{at: 0, demand: true, status: auto},
				{at: m1, demand: true, status: inAlarm, alarm: rpmLoss, wantAlerts: []string{"Start failed"}},
				{at: 4 * m1, demand: true, gErr: errDown},
				{at: 5 * m1, demand: true, status: inAlarm, alarm: rpmLoss, wantReset: true, wantRetry: true, wantRetries: 1, wantAlerts: []string{"Retry attempt 1/3"}},
			},
		},
		{
			name: "demand unknown: no reset; failure alerted as not retrying",
			steps: []step{
				{at: 0, dErr: errDown, status: auto},
				{at: m1, dErr: errDown, status: inAlarm, alarm: rpmLoss, wantAlerts: []string{"not retrying"}},
				{at: 11 * m1, dErr: errDown, status: inAlarm, alarm: rpmLoss, wantAlerts: []string{"Start-demand source unreachable"}},
			},
		},
		{
			name: "demand unknown during the delay holds the reset",
			steps: []step{
				{at: 0, demand: true, status: auto},
				{at: m1, demand: true, status: inAlarm, alarm: rpmLoss, wantAlerts: []string{"Start failed"}},
				{at: 4 * m1, dErr: errDown, status: inAlarm, alarm: rpmLoss},
				{at: 5 * m1, demand: true, status: inAlarm, alarm: rpmLoss, wantReset: true, wantRetry: true, wantRetries: 1, wantAlerts: []string{"Retry attempt 1/3"}},
			},
		},
		{
			name: "dry run never asks for a reset",
			dry:  true,
			steps: []step{
				{at: 0, demand: true, status: auto},
				{at: m1, demand: true, status: inAlarm, alarm: rpmLoss, wantAlerts: []string{"Start failed"}},
				{at: 4 * m1, demand: true, status: inAlarm, alarm: rpmLoss, wantRetry: true, wantRetries: 1, wantAlerts: []string{"Retry attempt 1/3"}},
				{at: 10 * m1, demand: true, status: inAlarm, alarm: rpmLoss, wantRetries: 1},
			},
		},
		{
			name: "manual reset during the delay cancels the retry",
			steps: []step{
				{at: 0, demand: true, status: auto},
				{at: m1, demand: true, status: inAlarm, alarm: rpmLoss, wantAlerts: []string{"Start failed"}},
				{at: 2 * m1, demand: true, status: running, alarm: rpmLoss},
				{at: 5 * m1, demand: true, status: running, alarm: rpmLoss},
			},
		},
		{
			name: "switch state not Auto after reset gives up for this demand",
			steps: []step{
				{at: 0, demand: true, status: auto},
				{at: m1, demand: true, status: inAlarm, alarm: rpmLoss, wantAlerts: []string{"Start failed"}},
				{at: 4 * m1, demand: true, status: inAlarm, alarm: rpmLoss, resetStatus: &inAlarm, wantReset: true, wantRetry: true, wantRetries: 1,
					wantAlerts: []string{"Retry attempt 1/3", "did not return to Auto"}},
				{at: 5 * m1, demand: true, status: inAlarm, alarm: fail2, wantRetries: 1, wantAlerts: []string{"not retrying"}},
			},
		},
		{
			name: "setremote error gives up for this demand",
			steps: []step{
				{at: 0, demand: true, status: auto},
				{at: m1, demand: true, status: inAlarm, alarm: rpmLoss, wantAlerts: []string{"Start failed"}},
				{at: 4 * m1, demand: true, status: inAlarm, alarm: rpmLoss, resetErr: errors.New("Read Only Mode"), wantReset: true, wantRetry: true, wantRetries: 1,
					wantAlerts: []string{"Retry attempt 1/3", "Reset failed"}},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { run(t, newTestMachine(tc.dry), tc.steps) })
	}
}

func TestAlertsCarryContext(t *testing.T) {
	m := newTestMachine(true)
	m.Step(Input{Now: t0, Demand: true, Status: auto})
	d := m.Step(Input{Now: t0.Add(m1), Demand: true, Status: inAlarm, Alarm: rpmLoss})
	if len(d.Alerts) != 1 {
		t.Fatalf("alerts = %d", len(d.Alerts))
	}
	a := d.Alerts[0]
	if a.AlarmLine != rpmLoss.Raw || a.BatteryVoltage != "12.40 V" || !a.Time.Equal(t0.Add(m1)) || !a.DryRun {
		t.Errorf("alert missing context: %+v", a)
	}
}

func TestRestartWithinGraceDoesNotRealertAfterHandling(t *testing.T) {
	// The documented trade-off: state is in memory. An alarm older than the grace
	// is never acted on after a restart.
	m := newTestMachine(false)
	d := m.Step(Input{Now: rpmLoss.Time.Add(6 * m1), Demand: true, Status: auto, Alarm: rpmLoss})
	if len(d.Alerts) != 0 || d.Reset {
		t.Fatalf("restart re-acted on handled alarm: %+v", d)
	}
}
