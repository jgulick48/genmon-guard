//go:build live

// Read-only check against a real genmon. Never sends setremote.
//
//	GENMON_LIVE_URL=https://genmon.example.lan GENMON_LIVE_USER=admin \
//	GENMON_LIVE_PASSWORD=... go test -tags live -run TestLive -v ./internal/genmon/
package genmon

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestLiveReadOnly(t *testing.T) {
	u := os.Getenv("GENMON_LIVE_URL")
	if u == "" {
		t.Skip("GENMON_LIVE_URL not set")
	}
	loc, err := time.LoadLocation(envOr("GENMON_LIVE_TZ", "America/Denver"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{URL: u, Username: os.Getenv("GENMON_LIVE_USER"), Password: os.Getenv("GENMON_LIVE_PASSWORD"),
		InsecureSkipVerify: true, Location: loc})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	st, err := c.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err) // stop here: a login failure must not be retried
	}
	t.Logf("status: %+v", st)
	if st.SwitchState == "" || st.EngineState == "" {
		t.Errorf("switch/engine state not parsed: %+v", st)
	}

	log, err := c.AlarmLog(ctx)
	if err != nil {
		t.Fatalf("alarm log: %v", err)
	}
	t.Logf("alarm log: %d entries", len(log))
	for i, e := range log {
		if i == 5 {
			break
		}
		t.Logf("  %s | text=%q code=%d", e.Time.Format(time.RFC3339), e.Text, e.Code)
	}

	c.mu.Lock()
	tok, err := c.csrfToken(ctx)
	c.mu.Unlock()
	switch {
	case errors.Is(err, errUnknownCommand):
		t.Log("setremote mode: legacy (genmon <= 2.0.03): GET /cmd/setremote?setremote=..., no CSRF token")
	case err != nil:
		t.Errorf("csrf token: %v", err)
	default:
		t.Logf("setremote mode: POST with X-CSRFToken (token %d chars)", len(tok))
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
