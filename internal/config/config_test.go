package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func write(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadYAMLAndSecrets(t *testing.T) {
	pw := write(t, "pw", "s3cret\n") // trailing newline must be stripped
	user := write(t, "user", "admin\n")
	hook := write(t, "hook", "https://hooks.example.invalid/x\n")
	cfg := write(t, "c.yaml", `
genmon:
  url: https://genmon.example.lan
  username_file: `+user+`
  password_file: `+pw+`
  insecure_skip_verify: true
  timezone: America/Denver
demand:
  metrics_url: http://bridge.example.svc:8080/metrics
  labels: {deploymentID: BA00000000}
poll_interval: 15s
retry_delay: 4m
max_retries: 2
retry_alarm_codes: [1500]
slack:
  webhook_url_file: `+hook+`
`)
	c, err := load(cfg, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.GenmonPassword != "s3cret" || c.Genmon.Username != "admin" || c.SlackWebhookURL != "https://hooks.example.invalid/x" {
		t.Errorf("secrets not trimmed/resolved: %q %q %q", c.GenmonPassword, c.Genmon.Username, c.SlackWebhookURL)
	}
	if c.PollInterval != 15*time.Second || c.RetryDelay != 4*time.Minute || *c.MaxRetries != 2 {
		t.Errorf("durations/retries: %v %v %d", c.PollInterval, c.RetryDelay, *c.MaxRetries)
	}
	if !*c.DryRun {
		t.Error("dry_run must default to true")
	}
	if c.Demand.Metric != "lxp_gen_dry_contact" || c.Demand.Labels["deploymentID"] != "BA00000000" {
		t.Errorf("demand: %+v", c.Demand)
	}
	if c.SuccessHold != 2*time.Minute || c.StartupGrace != 5*time.Minute || !slices.Equal(c.RetryAlarmCodes, []int{1500}) {
		t.Errorf("defaults: %+v", c)
	}
}

func TestEnvOverrides(t *testing.T) {
	cfg := write(t, "c.yaml", "genmon: {url: https://a.example.lan}\ndemand: {metrics_url: http://b.example/metrics}\ndry_run: true\n")
	c, err := load(cfg, env(map[string]string{
		"GENMON_GUARD_DRY_RUN":         "false",
		"GENMON_GUARD_GENMON_URL":      "https://c.example.lan",
		"GENMON_GUARD_GENMON_PASSWORD": "pw",
		"GENMON_GUARD_MAX_RETRIES":     "0",
		"GENMON_GUARD_RETRY_DELAY":     "90s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if *c.DryRun || c.Genmon.URL != "https://c.example.lan" || c.GenmonPassword != "pw" || *c.MaxRetries != 0 || c.RetryDelay != 90*time.Second {
		t.Errorf("env overrides not applied: dry=%v url=%q retries=%d delay=%v", *c.DryRun, c.Genmon.URL, *c.MaxRetries, c.RetryDelay)
	}
}

func TestEnvOnlyNoFile(t *testing.T) {
	c, err := load("", env(map[string]string{
		"GENMON_GUARD_GENMON_URL":         "https://a.example.lan",
		"GENMON_GUARD_DEMAND_METRICS_URL": "http://b.example/metrics",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !*c.DryRun {
		t.Error("dry run default")
	}
}

func TestValidation(t *testing.T) {
	for name, body := range map[string]string{
		"missing urls":     "poll_interval: 10s\n",
		"unknown key":      "genmon: {url: x}\ndemand: {metrics_url: y}\nbogus: 1\n",
		"bad timezone":     "genmon: {url: x, timezone: Mars/Base}\ndemand: {metrics_url: y}\n",
		"negative retries": "genmon: {url: x}\ndemand: {metrics_url: y}\nmax_retries: -1\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := load(write(t, "c.yaml", body), env(nil)); err == nil {
				t.Error("expected error")
			}
		})
	}
	if _, err := load(write(t, "c.yaml", "genmon: {url: x, password_file: /nonexistent/pw}\ndemand: {metrics_url: y}\n"), env(nil)); err == nil {
		t.Error("missing secret file must fail")
	}
}
