package alert

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func captureText(t *testing.T, a Alert) string {
	t.Helper()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method %s", r.Method)
		}
		var p map[string]string
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &p); err != nil {
			t.Errorf("bad json: %v", err)
		}
		got = p["text"]
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	if err := NewSlack(srv.URL+"/services/SECRET", nil, time.UTC).Send(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestSlackPayload(t *testing.T) {
	ts := time.Date(2026, 11, 15, 7, 4, 12, 0, time.UTC)
	for lvl, emoji := range map[Level]string{Info: ":white_check_mark:", Warning: ":warning:", Error: ":rotating_light:"} {
		got := captureText(t, Alert{Level: lvl, Title: "T", Text: "body", Time: ts})
		if !strings.HasPrefix(got, emoji+" *genmon-guard: T*") {
			t.Errorf("%s: got %q", lvl, got)
		}
	}
	full := captureText(t, Alert{Level: Error, Title: "Gave up", Text: "body", AlarmLine: "11/15/26 RPM Sense Loss", BatteryVoltage: "13.20 V", Time: ts, DryRun: true})
	for _, want := range []string{"[DRY RUN]", "body", "Alarm: `11/15/26 RPM Sense Loss`", "Battery: 13.20 V · Time: Sun 07:04:12 UTC"} {
		if !strings.Contains(full, want) {
			t.Errorf("missing %q in %q", want, full)
		}
	}
}

func TestSlackOmitsEmpty(t *testing.T) {
	got := captureText(t, Alert{Level: Info, Title: "T"})
	for _, bad := range []string{"Alarm:", "Battery:", "[DRY RUN]"} {
		if strings.Contains(got, bad) {
			t.Errorf("unexpected %q in %q", bad, got)
		}
	}
	if !strings.Contains(got, "Time: ") {
		t.Errorf("time missing: %q", got)
	}
}

func TestSlackNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, strings.Repeat("x", 500), http.StatusForbidden)
	}))
	defer srv.Close()
	err := NewSlack(srv.URL+"/services/SECRET", nil, nil).Send(context.Background(), Alert{Title: "T"})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v", err)
	}
	if len(err.Error()) > 300 {
		t.Errorf("error too long: %d", len(err.Error()))
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Error("error leaks URL")
	}
}

func TestSlackErrorHidesURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	u := srv.URL + "/services/SECRET"
	srv.Close()
	err := NewSlack(u, nil, nil).Send(context.Background(), Alert{Title: "T"})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), srv.URL) {
		t.Errorf("error leaks URL: %v", err)
	}
	// Malformed URL path must not leak either.
	err = NewSlack("http://bad host/SECRET\x7f", nil, nil).Send(context.Background(), Alert{Title: "T"})
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("bad URL error: %v", err)
	}
}
