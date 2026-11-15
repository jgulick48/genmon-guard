package demand

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseValue(t *testing.T) {
	const m = "lxp_gen_dry_contact"
	tests := []struct {
		name    string
		body    string
		labels  map[string]string
		want    float64
		wantErr string
	}{
		{"present 1", "# HELP x y\n# TYPE x gauge\n" + m + `{deploymentID="BA12345678"} 1` + "\n", nil, 1, ""},
		{"present 0", m + `{deploymentID="BA12345678"} 0` + "\n", nil, 0, ""},
		{"no labels with timestamp", m + " 1 1700000000000\n", nil, 1, ""},
		{"labels filter", m + `{deploymentID="A"} 0` + "\n" + m + `{deploymentID="B"} 1` + "\n", map[string]string{"deploymentID": "B"}, 1, ""},
		{"labels filter no match", m + `{deploymentID="A"} 0` + "\n", map[string]string{"deploymentID": "B"}, 0, "not found"},
		{"missing metric", "other_metric 1\n", nil, 0, "not found"},
		{"prefix name not matching", m + "_total 1\n", nil, 0, "not found"},
		{"quoted label comma brace escaped quote", m + `{note="a, b} {c \"q\" \\ x",id="z"} 1` + "\n", map[string]string{"note": `a, b} {c "q" \ x`, "id": "z"}, 1, ""},
		{"ambiguous", m + `{id="a"} 1` + "\n" + m + `{id="b"} 0` + "\n", nil, 0, "multiple series"},
		{"duplicate same value", m + `{id="a"} 1` + "\n" + m + `{id="b"} 1` + "\n", nil, 1, ""},
		{"NaN", m + " NaN\n", nil, 0, "NaN"},
		{"malformed lines skipped", "{{{\n" + m + `{id="a` + "\n" + m + " abc\n" + m + " 1\n", nil, 1, ""},
		{"empty", "", nil, 0, "not found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseValue(strings.NewReader(tc.body), m, tc.labels)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestScraperDemand(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    bool
		wantErr bool
	}{
		{"closed", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("lxp_gen_dry_contact 1\n")) }, true, false},
		{"open", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("lxp_gen_dry_contact 0\n")) }, false, false},
		{"http 500", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", 500) }, false, true},
		{"missing", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("foo 1\n")) }, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			got, err := New(Config{URL: srv.URL}, nil).Demand(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestScraperContextTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := New(Config{URL: srv.URL}, nil).Demand(ctx); err == nil {
		t.Fatal("expected timeout error")
	}
}
