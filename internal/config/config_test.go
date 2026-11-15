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
reset:
  retry_delay: 4m
  max_retries: 2
  alarm_codes: [1500]
  settle: 5s
  confirm_delay: 10s
  running_rpm: 1200
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
	r := c.Reset
	if c.PollInterval != 15*time.Second || r.RetryDelay != 4*time.Minute || *r.MaxRetries != 2 ||
		r.Settle != 5*time.Second || r.ConfirmDelay != 10*time.Second || r.RunningRPM != 1200 {
		t.Errorf("reset options: poll=%v %+v retries=%d", c.PollInterval, r, *r.MaxRetries)
	}
	if !*r.DryRun {
		t.Error("dry_run must default to true")
	}
	if c.Demand.Metric != "lxp_gen_dry_contact" || c.Demand.Labels["deploymentID"] != "BA00000000" {
		t.Errorf("demand: %+v", c.Demand)
	}
	if r.SuccessHold != 2*time.Minute || r.StartupGrace != 5*time.Minute || !slices.Equal(r.AlarmCodes, []int{1500}) {
		t.Errorf("defaults: %+v", c)
	}
}

func TestEnvOverrides(t *testing.T) {
	cfg := write(t, "c.yaml", "genmon: {url: https://a.example.lan}\ndemand: {metrics_url: http://b.example/metrics}\nreset: {dry_run: true}\n")
	c, err := load(cfg, env(map[string]string{
		"GENMON_GUARD_DRY_RUN":             "false",
		"GENMON_GUARD_GENMON_URL":          "https://c.example.lan",
		"GENMON_GUARD_GENMON_PASSWORD":     "pw",
		"GENMON_GUARD_MAX_RETRIES":         "0",
		"GENMON_GUARD_RETRY_DELAY":         "90s",
		"GENMON_GUARD_RESET_CONFIRM_DELAY": "7s",
		"GENMON_GUARD_RESET_ALARM_CODES":   "1500, 1600",
		"GENMON_GUARD_RUNNING_RPM":         "1400",
	}))
	if err != nil {
		t.Fatal(err)
	}
	r := c.Reset
	if *r.DryRun || c.Genmon.URL != "https://c.example.lan" || c.GenmonPassword != "pw" || *r.MaxRetries != 0 ||
		r.RetryDelay != 90*time.Second || r.ConfirmDelay != 7*time.Second || !slices.Equal(r.AlarmCodes, []int{1500, 1600}) || r.RunningRPM != 1400 {
		t.Errorf("env overrides not applied: url=%q %+v retries=%d", c.Genmon.URL, r, *r.MaxRetries)
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
	if !*c.Reset.DryRun {
		t.Error("dry run default")
	}
}

func TestValidation(t *testing.T) {
	for name, body := range map[string]string{
		"missing urls":     "poll_interval: 10s\n",
		"unknown key":      "genmon: {url: x}\ndemand: {metrics_url: y}\nbogus: 1\n",
		"bad timezone":     "genmon: {url: x, timezone: Mars/Base}\ndemand: {metrics_url: y}\n",
		"negative retries": "genmon: {url: x}\ndemand: {metrics_url: y}\nreset: {max_retries: -1}\n",
		"old flat key":     "genmon: {url: x}\ndemand: {metrics_url: y}\nretry_delay: 3m\n",
		"e-stop reset":     "genmon: {url: x}\ndemand: {metrics_url: y}\nreset: {alarm_codes: [1500, 2800]}\n",
		"low fuel reset":   "genmon: {url: x}\ndemand: {metrics_url: y}\nreset: {alarm_codes: [2680]}\n",
		"negative delay":   "genmon: {url: x}\ndemand: {metrics_url: y}\nreset: {retry_delay: -1m}\n",
		"huge settle":      "genmon: {url: x}\ndemand: {metrics_url: y}\nreset: {settle: 5m}\n",
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

// The committed config.yaml must load as-is (secrets supplied by env here).
func TestRepoConfigYAML(t *testing.T) {
	c, err := load("../../config.yaml", env(map[string]string{
		"GENMON_GUARD_GENMON_USERNAME":   "admin",
		"GENMON_GUARD_GENMON_PASSWORD":   "pw",
		"GENMON_GUARD_SLACK_WEBHOOK_URL": "https://hooks.example.invalid/x",
	}))
	if err != nil {
		t.Fatal(err)
	}
	r := c.Reset
	if !*r.DryRun || *r.MaxRetries != 3 || r.RetryDelay != 3*time.Minute || r.Settle != 3*time.Second ||
		r.ConfirmDelay != 3*time.Second || r.SuccessHold != 2*time.Minute || r.StartupGrace != 5*time.Minute ||
		r.RunningRPM != 1500 || !slices.Equal(r.AlarmCodes, []int{1500}) {
		t.Errorf("config.yaml reset options differ from the documented defaults: %+v", r)
	}
}
