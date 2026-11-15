// Package config loads genmon-guard's YAML config, applies environment
// overrides and reads secrets from files.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the service configuration. Secrets are never stored in the YAML
// itself: they come from *_file paths (a mounted Kubernetes Secret) or env.
type Config struct {
	Genmon struct {
		URL                string        `yaml:"url"`
		Username           string        `yaml:"username"`
		UsernameFile       string        `yaml:"username_file"`
		PasswordFile       string        `yaml:"password_file"`
		InsecureSkipVerify bool          `yaml:"insecure_skip_verify"`
		CAFile             string        `yaml:"ca_file"`
		Timezone           string        `yaml:"timezone"`
		Timeout            time.Duration `yaml:"timeout"`
	} `yaml:"genmon"`
	Demand struct {
		MetricsURL string            `yaml:"metrics_url"`
		Metric     string            `yaml:"metric"`
		Labels     map[string]string `yaml:"labels"`
		Timeout    time.Duration     `yaml:"timeout"`
	} `yaml:"demand"`
	Slack struct {
		WebhookURLFile string `yaml:"webhook_url_file"`
	} `yaml:"slack"`

	// Reset holds every option that controls the automatic alarm reset.
	Reset Reset `yaml:"reset"`

	PollInterval     time.Duration `yaml:"poll_interval"`
	OutageAlertAfter time.Duration `yaml:"outage_alert_after"`
	Listen           string        `yaml:"listen"`
	LogLevel         string        `yaml:"log_level"`
	LogFormat        string        `yaml:"log_format"` // json (default) or text

	// Resolved secrets (never read from YAML).
	GenmonPassword  string `yaml:"-"`
	SlackWebhookURL string `yaml:"-"`
}

// Reset configures when and how genmon-guard resets a failed start.
type Reset struct {
	DryRun       *bool         `yaml:"dry_run"`       // nil → true: decide and alert, never send setremote
	AlarmCodes   []int         `yaml:"alarm_codes"`   // codes that may be reset (default [1500])
	MaxRetries   *int          `yaml:"max_retries"`   // resets per demand period (default 3; 0 = alert only)
	RetryDelay   time.Duration `yaml:"retry_delay"`   // wait after the failure before resetting (3m)
	Settle       time.Duration `yaml:"settle"`        // pause between resetalarm and auto (3s)
	ConfirmDelay time.Duration `yaml:"confirm_delay"` // pause after auto before checking Switch State (3s)
	SuccessHold  time.Duration `yaml:"success_hold"`  // engine must run this long to count as recovered (2m)
	RunningRPM   int           `yaml:"running_rpm"`   // RPM at or above which the engine counts as running (1500)
	StartupGrace time.Duration `yaml:"startup_grace"` // alarms older than this at startup are not reset (5m)
}

// NeverResetCodes are alarms that must never be reset automatically:
// E-Stop (2800), Low Fuel (2700) and Low Fuel Level (2680).
var NeverResetCodes = []int{2800, 2700, 2680}

// Env names. Secrets may be given directly by env (handy for local runs);
// in Kubernetes prefer the *_FILE variants or the YAML *_file keys.
const (
	EnvPrefix = "GENMON_GUARD_"
)

// Load reads path (optional when empty), applies env overrides, defaults,
// resolves secrets and validates.
func Load(path string) (*Config, error) {
	return load(path, os.Getenv)
}

func load(path string, getenv func(string) string) (*Config, error) {
	c := &Config{}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading config: %w", err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) /* empty file */ {
			return nil, fmt.Errorf("parsing config %s: %w", path, err)
		}
	}
	if err := c.applyEnv(getenv); err != nil {
		return nil, err
	}
	c.defaults()
	if err := c.resolveSecrets(getenv); err != nil {
		return nil, err
	}
	return c, c.validate()
}

func (c *Config) applyEnv(getenv func(string) string) error {
	str := map[string]*string{
		"GENMON_URL":             &c.Genmon.URL,
		"GENMON_USERNAME":        &c.Genmon.Username,
		"GENMON_USERNAME_FILE":   &c.Genmon.UsernameFile,
		"GENMON_PASSWORD_FILE":   &c.Genmon.PasswordFile,
		"GENMON_CA_FILE":         &c.Genmon.CAFile,
		"GENMON_TIMEZONE":        &c.Genmon.Timezone,
		"DEMAND_METRICS_URL":     &c.Demand.MetricsURL,
		"DEMAND_METRIC":          &c.Demand.Metric,
		"SLACK_WEBHOOK_URL_FILE": &c.Slack.WebhookURLFile,
		"LISTEN":                 &c.Listen,
		"LOG_LEVEL":              &c.LogLevel,
		"LOG_FORMAT":             &c.LogFormat,
	}
	for k, p := range str {
		if v := getenv(EnvPrefix + k); v != "" {
			*p = v
		}
	}
	dur := map[string]*time.Duration{
		"POLL_INTERVAL":       &c.PollInterval,
		"OUTAGE_ALERT_AFTER":  &c.OutageAlertAfter,
		"RETRY_DELAY":         &c.Reset.RetryDelay,
		"RESET_SETTLE":        &c.Reset.Settle,
		"RESET_CONFIRM_DELAY": &c.Reset.ConfirmDelay,
		"SUCCESS_HOLD":        &c.Reset.SuccessHold,
		"STARTUP_GRACE":       &c.Reset.StartupGrace,
	}
	for k, p := range dur {
		if v := getenv(EnvPrefix + k); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("%s%s: %w", EnvPrefix, k, err)
			}
			*p = d
		}
	}
	if v := getenv(EnvPrefix + "MAX_RETRIES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%sMAX_RETRIES: %w", EnvPrefix, err)
		}
		c.Reset.MaxRetries = &n
	}
	if v := getenv(EnvPrefix + "RUNNING_RPM"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%sRUNNING_RPM: %w", EnvPrefix, err)
		}
		c.Reset.RunningRPM = n
	}
	if v := getenv(EnvPrefix + "RESET_ALARM_CODES"); v != "" {
		var codes []int
		for _, f := range strings.Split(v, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil {
				return fmt.Errorf("%sRESET_ALARM_CODES: %w", EnvPrefix, err)
			}
			codes = append(codes, n)
		}
		c.Reset.AlarmCodes = codes
	}
	for k, p := range map[string]*bool{"DRY_RUN": nil, "GENMON_INSECURE_SKIP_VERIFY": &c.Genmon.InsecureSkipVerify} {
		v := getenv(EnvPrefix + k)
		if v == "" {
			continue
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%s%s: %w", EnvPrefix, k, err)
		}
		if p == nil {
			c.Reset.DryRun = &b
		} else {
			*p = b
		}
	}
	return nil
}

func (c *Config) defaults() {
	setDur := func(p *time.Duration, d time.Duration) {
		if *p == 0 {
			*p = d
		}
	}
	setDur(&c.PollInterval, 10*time.Second)
	setDur(&c.OutageAlertAfter, 10*time.Minute)
	setDur(&c.Reset.RetryDelay, 3*time.Minute)
	setDur(&c.Reset.Settle, 3*time.Second)
	setDur(&c.Reset.ConfirmDelay, 3*time.Second)
	setDur(&c.Reset.SuccessHold, 2*time.Minute)
	setDur(&c.Reset.StartupGrace, 5*time.Minute)
	setDur(&c.Genmon.Timeout, 15*time.Second)
	setDur(&c.Demand.Timeout, 5*time.Second)
	if c.Reset.MaxRetries == nil {
		n := 3
		c.Reset.MaxRetries = &n
	}
	if c.Reset.DryRun == nil {
		t := true
		c.Reset.DryRun = &t
	}
	if len(c.Reset.AlarmCodes) == 0 {
		c.Reset.AlarmCodes = []int{1500}
	}
	if c.Reset.RunningRPM == 0 {
		c.Reset.RunningRPM = 1500
	}
	if c.Demand.Metric == "" {
		c.Demand.Metric = "lxp_gen_dry_contact"
	}
	if c.Genmon.Timezone == "" {
		c.Genmon.Timezone = "Local"
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.LogFormat == "" {
		c.LogFormat = "json"
	}
}

// resolveSecrets reads the genmon username/password and Slack webhook.
// Order: direct env var, then *_FILE env / YAML *_file path.
func (c *Config) resolveSecrets(getenv func(string) string) error {
	var err error
	if v := getenv(EnvPrefix + "GENMON_PASSWORD"); v != "" {
		c.GenmonPassword = v
	} else if c.GenmonPassword, err = readSecret(c.Genmon.PasswordFile); err != nil {
		return fmt.Errorf("genmon password: %w", err)
	}
	if c.Genmon.Username == "" {
		if c.Genmon.Username, err = readSecret(c.Genmon.UsernameFile); err != nil {
			return fmt.Errorf("genmon username: %w", err)
		}
	}
	if v := getenv(EnvPrefix + "SLACK_WEBHOOK_URL"); v != "" {
		c.SlackWebhookURL = v
	} else if c.SlackWebhookURL, err = readSecret(c.Slack.WebhookURLFile); err != nil {
		return fmt.Errorf("slack webhook: %w", err)
	}
	return nil
}

// readSecret reads a secret file and strips surrounding whitespace, in
// particular the trailing newline most editors and `kubectl create secret
// --from-file` leave behind (genmon rejects a password with a newline).
func readSecret(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func (c *Config) validate() error {
	var errs []error
	if c.Genmon.URL == "" {
		errs = append(errs, errors.New("genmon.url is required"))
	}
	if c.Demand.MetricsURL == "" {
		errs = append(errs, errors.New("demand.metrics_url is required"))
	}
	r := c.Reset
	if *r.MaxRetries < 0 {
		errs = append(errs, errors.New("reset.max_retries must be >= 0"))
	}
	for _, code := range r.AlarmCodes {
		if slices.Contains(NeverResetCodes, code) {
			errs = append(errs, fmt.Errorf("reset.alarm_codes: %d must never be reset automatically", code))
		}
	}
	for name, d := range map[string]time.Duration{"retry_delay": r.RetryDelay, "settle": r.Settle,
		"confirm_delay": r.ConfirmDelay, "success_hold": r.SuccessHold, "startup_grace": r.StartupGrace} {
		if d < 0 {
			errs = append(errs, fmt.Errorf("reset.%s must not be negative", name))
		}
	}
	if r.Settle > time.Minute || r.ConfirmDelay > time.Minute {
		errs = append(errs, errors.New("reset.settle and reset.confirm_delay must be at most 1m"))
	}
	if r.RunningRPM < 0 {
		errs = append(errs, errors.New("reset.running_rpm must not be negative"))
	}
	if c.PollInterval < time.Second {
		errs = append(errs, errors.New("poll_interval must be at least 1s"))
	}
	if _, err := c.Location(); err != nil {
		errs = append(errs, fmt.Errorf("genmon.timezone: %w", err))
	}
	switch c.LogFormat {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("log_format %q: want json or text", c.LogFormat))
	}
	return errors.Join(errs...)
}

// Location returns the controller's time zone for alarm-log timestamps.
func (c *Config) Location() (*time.Location, error) {
	return time.LoadLocation(c.Genmon.Timezone)
}
