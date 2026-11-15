package genmon

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jgulick48/genmon-guard/internal/genmon/genmontest"
)

func newClient(t *testing.T, f *genmontest.Fake, mod func(*Config)) *Client {
	t.Helper()
	cfg := Config{URL: f.URL(), Username: "admin", Password: "secret", InsecureSkipVerify: true}
	if mod != nil {
		mod(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLoginOnceAndStatus(t *testing.T) {
	f := genmontest.New(t)
	f.Set(func(f *genmontest.Fake) { f.RPM = 1800; f.SwitchState = "System in Alarm"; f.EngineState = "Running" })
	c := newClient(t, f, nil)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		st, err := c.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := Status{SwitchState: "System in Alarm", EngineState: "Running", RPM: 1800, BatteryVoltage: "13.20 V"}
		if st != want {
			t.Fatalf("got %+v want %+v", st, want)
		}
		if !st.InAlarm() {
			t.Fatal("expected alarm")
		}
	}
	if _, err := c.AlarmLog(ctx); err != nil {
		t.Fatal(err)
	}
	if a, _ := f.Logins(); a != 1 {
		t.Fatalf("login attempts = %d, want 1", a)
	}
}

func TestCAFile(t *testing.T) {
	f := genmontest.New(t)
	path := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.Server.Certificate().Raw})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{URL: f.URL(), Username: "admin", Password: "secret", CAFile: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Without the CA, verification must fail.
	c2, _ := New(Config{URL: f.URL(), Username: "admin", Password: "secret"})
	if _, err := c2.Status(context.Background()); err == nil {
		t.Fatal("expected TLS error without CA")
	}
	if _, err := New(Config{URL: f.URL(), CAFile: filepath.Join(t.TempDir(), "missing.pem")}); err == nil {
		t.Fatal("expected error for missing CA file")
	}
}

func TestStatusParsingVariants(t *testing.T) {
	tests := []struct {
		name string
		rpm  any
		want int
	}{
		{"string", "1800", 1800},
		{"unit suffix", "0 RPM", 0},
		{"junk", "n/a", -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseRPM(tt.rpm); got != tt.want {
				t.Fatalf("got %d want %d", got, tt.want)
			}
		})
	}
	if parseRPM(float64(3600)) != 3600 || parseRPM(nil) != -1 {
		t.Fatal("number/missing RPM")
	}
	v, _ := findKey(map[string]any{"a": []any{map[string]any{"b": map[string]any{"RPM": "7"}}}}, "RPM")
	if v != "7" {
		t.Fatalf("nested find: %v", v)
	}
}

func TestParseLogLine(t *testing.T) {
	denver, err := time.LoadLocation("America/Denver")
	if err != nil {
		t.Skip("no tzdata")
	}
	tests := []struct {
		name string
		line string
		ok   bool
		text string
		code int
		when time.Time
	}{
		{"real", "11/15/26 07:00:59 RPM Sense Loss : Alarm Code: 1500", true, "RPM Sense Loss", 1500,
			time.Date(2026, 11, 15, 7, 0, 59, 0, denver)},
		{"no code", "11/15/26 07:00:59 Low Fuel", true, "Low Fuel", 0, time.Date(2026, 11, 15, 7, 0, 59, 0, denver)},
		{"empty", "", false, "", 0, time.Time{}},
		{"junk", "no alarms logged", false, "", 0, time.Time{}},
		{"bad date", "13/45/26 07:00:59 X", false, "", 0, time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, ok := ParseLogLine(tt.line, denver)
			if ok != tt.ok {
				t.Fatalf("ok=%v want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if e.Text != tt.text || e.Code != tt.code || !e.Time.Equal(tt.when) || e.Raw != tt.line {
				t.Fatalf("got %+v", e)
			}
			if e.Time.Location() != denver {
				t.Fatal("wrong location")
			}
		})
	}
}

func TestAlarmLog(t *testing.T) {
	f := genmontest.New(t)
	f.Set(func(f *genmontest.Fake) {
		f.AlarmLog = []string{
			"11/15/26 07:00:59 RPM Sense Loss : Alarm Code: 1500",
			"",
			"not a log line",
			"11/14/26 01:02:03 Low Fuel",
		}
	})
	denver, _ := time.LoadLocation("America/Denver")
	c := newClient(t, f, func(c *Config) { c.Location = denver })
	got, err := c.AlarmLog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries: %+v", len(got), got)
	}
	if got[0].Code != 1500 || got[0].Text != "RPM Sense Loss" || got[1].Text != "Low Fuel" || got[1].Code != 0 {
		t.Fatalf("bad entries %+v", got)
	}
	if !got[0].Time.After(got[1].Time) {
		t.Fatal("expected newest first")
	}
	f.Set(func(f *genmontest.Fake) { f.AlarmLog = nil })
	got, err = c.AlarmLog(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("empty log: %v %v", got, err)
	}
}

func TestExpiredSessionRelogin(t *testing.T) {
	f := genmontest.New(t)
	c := newClient(t, f, nil)
	ctx := context.Background()
	if _, err := c.Status(ctx); err != nil {
		t.Fatal(err)
	}
	f.ExpireSessions()
	if _, err := c.Status(ctx); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
	if a, _ := f.Logins(); a != 2 {
		t.Fatalf("logins = %d, want 2", a)
	}
	// Expiry during SetRemote too.
	f.ExpireSessions()
	if err := c.SetRemote(ctx, "resetalarm"); err != nil {
		t.Fatal(err)
	}
}

func TestLoginFailureBackoff(t *testing.T) {
	f := genmontest.New(t)
	c := newClient(t, f, func(c *Config) {
		c.Password = "wrong"
		c.LoginBackoffMin = time.Minute
		c.LoginBackoffMax = 3 * time.Minute
	})
	clock := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return clock }
	ctx := context.Background()

	_, err := c.Status(ctx)
	if !errors.Is(err, ErrLoginFailed) {
		t.Fatalf("want ErrLoginFailed, got %v", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Fatal("password leaked in error")
	}
	for i := 0; i < 3; i++ {
		if _, err := c.Status(ctx); !errors.Is(err, ErrLoginBackoff) {
			t.Fatalf("want ErrLoginBackoff, got %v", err)
		}
		if err := c.SetRemote(ctx, "auto"); !errors.Is(err, ErrLoginBackoff) {
			t.Fatalf("want ErrLoginBackoff, got %v", err)
		}
	}
	if a, _ := f.Logins(); a != 1 {
		t.Fatalf("attempts = %d, want 1", a)
	}
	// Doubling: 1m, 2m, then capped at 3m.
	for i, want := range []time.Duration{2 * time.Minute, 3 * time.Minute, 3 * time.Minute} {
		clock = clock.Add(c.backoff) // exactly when backoff expires
		if _, err := c.Status(ctx); !errors.Is(err, ErrLoginFailed) {
			t.Fatalf("step %d: want ErrLoginFailed, got %v", i, err)
		}
		if c.backoff != want {
			t.Fatalf("step %d: backoff %s want %s", i, c.backoff, want)
		}
	}
	if a, _ := f.Logins(); a != 4 {
		t.Fatalf("attempts = %d, want 4", a)
	}
	// Fixing the password resets backoff.
	f.Set(func(f *genmontest.Fake) { f.Password = "wrong" })
	clock = clock.Add(c.backoff)
	if _, err := c.Status(ctx); err != nil {
		t.Fatal(err)
	}
	if c.backoff != 0 {
		t.Fatal("backoff not reset")
	}
}

func TestLockedMessage(t *testing.T) {
	f := genmontest.New(t)
	f.Set(func(f *genmontest.Fake) { f.LockoutAfter = 1 })
	c := newClient(t, f, func(c *Config) { c.Password = "bad" })
	clock := time.Now()
	c.now = func() time.Time { return clock }
	c.Status(context.Background()) // first failure
	clock = clock.Add(time.Hour)
	_, err := c.Status(context.Background())
	if !errors.Is(err, ErrLoginFailed) || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("got %v", err)
	}
}

func TestSetRemote(t *testing.T) {
	f := genmontest.New(t)
	c := newClient(t, f, nil)
	ctx := context.Background()
	for _, cmd := range []string{"resetalarm", "auto", "resetalarm"} {
		if err := c.SetRemote(ctx, cmd); err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
	}
	if got := f.CommandsSent(); !reflect.DeepEqual(got, []string{"resetalarm", "auto", "resetalarm"}) {
		t.Fatalf("commands %v", got)
	}
}

func TestSetRemoteCSRFRotation(t *testing.T) {
	f := genmontest.New(t)
	c := newClient(t, f, nil)
	ctx := context.Background()
	if err := c.SetRemote(ctx, "auto"); err != nil {
		t.Fatal(err)
	}
	f.Set(func(f *genmontest.Fake) { f.CSRFToken = "rotated" })
	if err := c.SetRemote(ctx, "resetalarm"); err != nil {
		t.Fatal(err)
	}
}

func TestSetRemoteDisallowed(t *testing.T) {
	f := genmontest.New(t)
	c := newClient(t, f, nil)
	for _, cmd := range []string{"start", "stop", "manual", "off", "", "RESETALARM", "auto "} {
		if err := c.SetRemote(context.Background(), cmd); err == nil {
			t.Fatalf("%q should be rejected", cmd)
		}
	}
	if a, _ := f.Logins(); a != 0 {
		t.Fatal("network contacted for disallowed command")
	}
	if len(f.CommandsSent()) != 0 {
		t.Fatal("command reached server")
	}
}

func TestSetRemoteReadOnly(t *testing.T) {
	f := genmontest.New(t)
	f.Set(func(f *genmontest.Fake) { f.ReadOnly = true })
	c := newClient(t, f, nil)
	err := c.SetRemote(context.Background(), "resetalarm")
	if err == nil || !strings.Contains(err.Error(), "Read Only") {
		t.Fatalf("got %v", err)
	}
}

func TestFakeCSRFEnforced(t *testing.T) {
	f := genmontest.New(t)
	c := newClient(t, f, nil)
	if _, err := c.Status(context.Background()); err != nil { // establishes session in c.hc
		t.Fatal(err)
	}
	post := func(token string) int {
		req, _ := http.NewRequest(http.MethodPost, f.URL()+"/cmd/setremote", strings.NewReader(url.Values{"setremote": {"resetalarm"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if token != "" {
			req.Header.Set("X-CSRFToken", token)
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := post(""); got != 403 {
		t.Fatalf("missing token: %d", got)
	}
	if got := post("bogus"); got != 403 {
		t.Fatalf("bad token: %d", got)
	}
	if got := post(f.CSRFToken); got != 200 {
		t.Fatalf("good token: %d", got)
	}
	req, _ := http.NewRequest(http.MethodGet, f.URL()+"/cmd/setremote", nil)
	resp, err := c.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
}

func TestNewValidation(t *testing.T) {
	for _, u := range []string{"", "genmon.example.lan", "ftp://x"} {
		if _, err := New(Config{URL: u}); err == nil {
			t.Fatalf("%q should fail", u)
		}
	}
}

// genmon 2.0.03 (the release running on the real unit) has no csrf_token
// command and a GET-only /cmd/ route: setremote must fall back to a GET.
func TestSetRemoteLegacyGenmon(t *testing.T) {
	f := genmontest.New(t)
	f.Set(func(f *genmontest.Fake) { f.Legacy = true })
	c := newClient(t, f, nil)
	ctx := context.Background()
	for _, cmd := range []string{"resetalarm", "auto"} {
		if err := c.SetRemote(ctx, cmd); err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
	}
	if got := f.CommandsSent(); !slices.Equal(got, []string{"resetalarm", "auto"}) {
		t.Fatalf("commands = %q", got)
	}
	if !c.legacy || !c.legacyKnown {
		t.Error("legacy mode not detected")
	}
	// "Unknown Command" must not be mistaken for an expired session.
	if attempts, _ := f.Logins(); attempts != 1 {
		t.Errorf("logins = %d, want 1", attempts)
	}
	// Expired session in legacy mode: re-login, command still sent once.
	f.ExpireSessions()
	if err := c.SetRemote(ctx, "auto"); err != nil {
		t.Fatal(err)
	}
	if got := f.CommandsSent(); len(got) != 3 {
		t.Fatalf("commands after re-login = %q", got)
	}
}

func TestSetRemoteLegacyReadOnly(t *testing.T) {
	f := genmontest.New(t)
	f.Set(func(f *genmontest.Fake) { f.Legacy = true; f.ReadOnly = true })
	if err := newClient(t, f, nil).SetRemote(context.Background(), "resetalarm"); err == nil || !strings.Contains(err.Error(), "Read Only Mode") {
		t.Fatalf("err = %v", err)
	}
}

func TestUnknownCommandIsNotRelogin(t *testing.T) {
	f := genmontest.New(t)
	c := newClient(t, f, nil)
	c.mu.Lock()
	_, err := c.cmd(context.Background(), http.MethodGet, "/cmd/nonexistent", nil, nil)
	c.mu.Unlock()
	if !errors.Is(err, errUnknownCommand) {
		t.Fatalf("err = %v", err)
	}
	if attempts, _ := f.Logins(); attempts != 1 {
		t.Errorf("logins = %d, want 1", attempts)
	}
}
