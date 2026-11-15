package guard

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jgulick48/genmon-guard/internal/alert"
	"github.com/jgulick48/genmon-guard/internal/genmon"
	"github.com/jgulick48/genmon-guard/internal/genmon/genmontest"
	"github.com/jgulick48/genmon-guard/internal/metrics"
)

type fakeDemand struct{ v bool }

func (f *fakeDemand) Demand(context.Context) (bool, error) { return f.v, nil }

type recorder struct {
	mu     sync.Mutex
	alerts []alert.Alert
}

func (r *recorder) Send(_ context.Context, a alert.Alert) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.alerts = append(r.alerts, a)
	return nil
}

func (r *recorder) titles() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, a := range r.alerts {
		out = append(out, a.Title)
	}
	return out
}

// newRunner wires a Runner to a fake genmon over real HTTPS, with a fake clock.
func newRunner(t *testing.T, dry bool) (*Runner, *genmontest.Fake, *fakeDemand, *recorder, *time.Time) {
	t.Helper()
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		t.Skip("tzdata not available:", err)
	}
	f := genmontest.New(t)
	gc, err := genmon.New(genmon.Config{URL: f.URL(), Username: f.Username, Password: f.Password,
		InsecureSkipVerify: true, Location: loc})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 15, 7, 0, 0, 0, loc)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dem := &fakeDemand{}
	rec := &recorder{}
	r := &Runner{
		Machine: New(Config{MaxRetries: 3, DryRun: dry}, log),
		Genmon:  gc, Demand: dem, Alerter: rec, Metrics: metrics.New(), Log: log, DryRun: dry,
	}
	r.now = func() time.Time { return now }
	r.sleep = func(context.Context, time.Duration) error { return nil }
	return r, f, dem, rec, &now
}

func failNow(f *genmontest.Fake, at time.Time) {
	f.Set(func(f *genmontest.Fake) {
		f.SwitchState = genmon.SwitchAlarm
		f.EngineState = "Stopped - Alarm"
		f.AlarmLog = append([]string{at.Format("01/02/06 15:04:05") + " RPM Sense Loss : Alarm Code: 1500"}, f.AlarmLog...)
	})
}

func TestRunnerLiveResetSendsResetalarmThenAuto(t *testing.T) {
	r, f, dem, rec, now := newRunner(t, false)
	ctx := context.Background()
	dem.v = true
	r.Tick(ctx)
	*now = now.Add(time.Minute)
	failNow(f, *now)
	r.Tick(ctx)
	*now = now.Add(3 * time.Minute)
	r.Tick(ctx)

	if got, want := f.CommandsSent(), []string{"resetalarm", "auto"}; !slices.Equal(got, want) {
		t.Fatalf("commands = %q, want %q", got, want)
	}
	if titles := rec.titles(); len(titles) != 2 || !strings.Contains(titles[1], "Retry attempt 1/3") {
		t.Fatalf("alerts = %q", titles)
	}
	// Fake sets Auto on "auto", so no "did not return to Auto" alert.
	rw := httptest.NewRecorder()
	r.Metrics.ServeHTTP(rw, nil)
	if !strings.Contains(rw.Body.String(), `genmon_guard_retries_total{result="sent"} 1`) {
		t.Errorf("metrics missing retry counter:\n%s", rw.Body.String())
	}
}

func TestRunnerDryRunSendsNoCommands(t *testing.T) {
	r, f, dem, rec, now := newRunner(t, true)
	ctx := context.Background()
	dem.v = true
	r.Tick(ctx)
	*now = now.Add(time.Minute)
	failNow(f, *now)
	for i := 0; i < 60; i++ { // 10 minutes of polls
		*now = now.Add(10 * time.Second)
		r.Tick(ctx)
	}
	if got := f.CommandsSent(); len(got) != 0 {
		t.Fatalf("dry run sent commands: %q", got)
	}
	titles := rec.titles()
	if len(titles) != 2 || !strings.Contains(titles[1], "Retry attempt 1/3") || !rec.alerts[1].DryRun ||
		!strings.Contains(rec.alerts[1].Text, "Would reset") {
		t.Fatalf("alerts = %+v", rec.alerts)
	}
}

func TestRunnerExpiredSessionStillWorks(t *testing.T) {
	r, f, dem, rec, now := newRunner(t, false)
	ctx := context.Background()
	dem.v = true
	r.Tick(ctx)
	f.ExpireSessions()
	*now = now.Add(time.Minute)
	failNow(f, *now)
	r.Tick(ctx)
	f.ExpireSessions()
	*now = now.Add(3 * time.Minute)
	r.Tick(ctx)
	if got := f.CommandsSent(); !slices.Equal(got, []string{"resetalarm", "auto"}) {
		t.Fatalf("commands = %q (alerts %q)", got, rec.titles())
	}
}

func TestRunnerNoDemandNoCommands(t *testing.T) {
	r, f, _, rec, now := newRunner(t, false)
	ctx := context.Background()
	r.Tick(ctx)
	*now = now.Add(time.Minute)
	failNow(f, *now)
	for i := 0; i < 60; i++ {
		*now = now.Add(10 * time.Second)
		r.Tick(ctx)
	}
	if got := f.CommandsSent(); len(got) != 0 {
		t.Fatalf("commands sent without demand: %q", got)
	}
	if titles := rec.titles(); len(titles) != 1 || !strings.Contains(titles[0], "not retrying") {
		t.Fatalf("alerts = %q", titles)
	}
}

func TestHealthHandler(t *testing.T) {
	r, _, _, _, now := newRunner(t, true)
	rw := httptest.NewRecorder()
	r.HealthHandler().ServeHTTP(rw, nil)
	if rw.Code != 503 {
		t.Fatalf("before first tick: %d", rw.Code)
	}
	r.Tick(context.Background())
	rw = httptest.NewRecorder()
	r.HealthHandler().ServeHTTP(rw, nil)
	if rw.Code != 200 {
		t.Fatalf("after tick: %d", rw.Code)
	}
	*now = now.Add(time.Hour)
	rw = httptest.NewRecorder()
	r.HealthHandler().ServeHTTP(rw, nil)
	if rw.Code != 503 {
		t.Fatalf("stalled: %d", rw.Code)
	}
}

func TestRunnerLiveResetLegacyGenmon(t *testing.T) {
	r, f, dem, _, now := newRunner(t, false)
	f.Set(func(f *genmontest.Fake) { f.Legacy = true })
	ctx := context.Background()
	dem.v = true
	r.Tick(ctx)
	*now = now.Add(time.Minute)
	failNow(f, *now)
	r.Tick(ctx)
	*now = now.Add(3 * time.Minute)
	r.Tick(ctx)
	if got := f.CommandsSent(); !slices.Equal(got, []string{"resetalarm", "auto"}) {
		t.Fatalf("commands = %q", got)
	}
}
