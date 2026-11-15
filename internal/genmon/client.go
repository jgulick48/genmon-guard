package genmon

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sentinel errors.
var (
	// ErrLoginFailed is returned when genmon rejects the credentials (or the UI is locked).
	ErrLoginFailed = errors.New("genmon: login failed")
	// ErrLoginBackoff is returned without contacting the server while a login backoff is active.
	ErrLoginBackoff = errors.New("genmon: login suppressed by backoff after failed login")
	// errUnknownCommand: genmon answered with its "Unknown Command" error page.
	errUnknownCommand = errors.New("genmon: unknown command")
)

// Config configures a Client.
type Config struct {
	URL                string         // base URL, e.g. https://genmon.example.lan
	Username, Password string         //
	InsecureSkipVerify bool           // skip TLS verification
	CAFile             string         // optional PEM CA bundle
	Location           *time.Location // for log timestamps; nil -> time.Local
	Timeout            time.Duration  // per request; 0 -> 15s
	LoginBackoffMin    time.Duration  // 0 -> 1m
	LoginBackoffMax    time.Duration  // 0 -> 15m
}

// Client talks to the genmon web API. It is safe for concurrent use.
type Client struct {
	base     string
	user     string
	pass     string
	loc      *time.Location
	backMin  time.Duration
	backMax  time.Duration
	hc       *http.Client
	now      func() time.Time
	mu       sync.Mutex
	loggedIn bool
	backoff  time.Duration // current backoff length (0 = none yet)
	// legacy is true for genmon releases up to 2.0.03, whose /cmd/<command> route is
	// GET-only and has no csrf_token command: setremote is a GET with a query
	// string. Newer genmon requires POST + X-CSRFToken. Detected on first SetRemote.
	legacy, legacyKnown bool
	nextTry             time.Time // no login before this time
}

// New builds a Client.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("genmon: invalid URL %q", cfg.URL)
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.InsecureSkipVerify}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("genmon: reading CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("genmon: no certificates found in CA file")
		}
		tlsCfg.RootCAs = pool
	}
	c := &Client{
		base:    strings.TrimRight(cfg.URL, "/"),
		user:    cfg.Username,
		pass:    cfg.Password,
		loc:     cfg.Location,
		backMin: cfg.LoginBackoffMin,
		backMax: cfg.LoginBackoffMax,
		now:     time.Now,
	}
	if c.loc == nil {
		c.loc = time.Local
	}
	if c.backMin <= 0 {
		c.backMin = time.Minute
	}
	if c.backMax <= 0 {
		c.backMax = 15 * time.Minute
	}
	if c.backMax < c.backMin {
		c.backMax = c.backMin
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	c.hc = &http.Client{
		Timeout:       timeout,
		Transport:     &http.Transport{TLSClientConfig: tlsCfg},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	c.resetJar()
	return c, nil
}

func (c *Client) resetJar() {
	jar, _ := cookiejar.New(nil)
	c.hc.Jar = jar
}

type response struct {
	status int
	ctype  string
	body   []byte
}

func (r response) isHTML() bool {
	if strings.Contains(strings.ToLower(r.ctype), "text/html") {
		return true
	}
	return bytes.HasPrefix(bytes.TrimSpace(r.body), []byte("<"))
}

// isLoginPage reports whether genmon sent its login form (session missing or
// expired), as opposed to another HTML page such as "Unknown Command".
func (r response) isLoginPage() bool {
	if !r.isHTML() {
		return false
	}
	b := bytes.ToLower(r.body)
	return bytes.Contains(b, []byte(`name="password"`)) || bytes.Contains(b, []byte(`type="password"`))
}

func (r response) isUnknownCommand() bool {
	return r.isHTML() && bytes.Contains(r.body, []byte("Unknown Command"))
}

func snippet(b []byte) string {
	if len(b) > 200 {
		b = b[:200]
	}
	return strings.TrimSpace(string(b))
}

func (c *Client) rawDo(ctx context.Context, method, path string, form url.Values, hdr map[string]string) (response, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return response{}, err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		// url.Error never contains the form body, so the password cannot leak here.
		return response{}, fmt.Errorf("genmon: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return response{}, fmt.Errorf("genmon: reading %s: %w", path, err)
	}
	return response{status: resp.StatusCode, ctype: resp.Header.Get("Content-Type"), body: b}, nil
}

// login must be called with c.mu held.
func (c *Client) login(ctx context.Context) error {
	if now := c.now(); now.Before(c.nextTry) {
		return fmt.Errorf("%w (retry in %s)", ErrLoginBackoff, c.nextTry.Sub(now).Round(time.Second))
	}
	c.resetJar()
	r, err := c.rawDo(ctx, http.MethodPost, "/", url.Values{"username": {c.user}, "password": {c.pass}}, nil)
	if err != nil {
		return err
	}
	if r.status == http.StatusFound {
		c.loggedIn = false
		c.backoff, c.nextTry = 0, time.Time{}
		c.loggedIn = true
		return nil
	}
	c.loggedIn = false
	if c.backoff == 0 {
		c.backoff = c.backMin
	} else {
		c.backoff *= 2
		if c.backoff > c.backMax {
			c.backoff = c.backMax
		}
	}
	c.nextTry = c.now().Add(c.backoff)
	if bytes.Contains(r.body, []byte("locked")) || bytes.Contains(r.body, []byte("Locked")) {
		return fmt.Errorf("%w: account locked out (HTTP %d)", ErrLoginFailed, r.status)
	}
	return fmt.Errorf("%w: HTTP %d", ErrLoginFailed, r.status)
}

// cmd performs an authenticated request, re-logging in once if the session died.
// Must be called with c.mu held. It returns the response for any HTTP status.
func (c *Client) cmd(ctx context.Context, method, path string, form url.Values, hdr map[string]string) (response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if !c.loggedIn {
			if err := c.login(ctx); err != nil {
				return response{}, err
			}
		}
		r, err := c.rawDo(ctx, method, path, form, hdr)
		if err != nil {
			return response{}, err
		}
		if r.isLoginPage() {
			c.loggedIn = false
			continue
		}
		if r.isUnknownCommand() {
			return response{}, fmt.Errorf("%w: %s %s", errUnknownCommand, method, path)
		}
		if r.isHTML() {
			return response{}, fmt.Errorf("genmon: %s %s: unexpected HTML response (HTTP %d)", method, path, r.status)
		}
		return r, nil
	}
	return response{}, fmt.Errorf("genmon: %s %s: server keeps returning the login page", method, path)
}

func (c *Client) getJSON(ctx context.Context, path string) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, err := c.cmd(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	if r.status < 200 || r.status > 299 {
		return nil, fmt.Errorf("genmon: GET %s: HTTP %d: %s", path, r.status, snippet(r.body))
	}
	var v any
	if err := json.Unmarshal(r.body, &v); err != nil {
		return nil, fmt.Errorf("genmon: GET %s: decoding JSON: %w", path, err)
	}
	return v, nil
}

// Status fetches /cmd/status_json.
func (c *Client) Status(ctx context.Context) (Status, error) {
	v, err := c.getJSON(ctx, "/cmd/status_json")
	if err != nil {
		return Status{}, err
	}
	s := Status{RPM: -1}
	if x, ok := findKey(v, "Switch State"); ok {
		s.SwitchState = scalarString(x)
	}
	if x, ok := findKey(v, "Engine State"); ok {
		s.EngineState = scalarString(x)
	}
	if x, ok := findKey(v, "Battery Voltage"); ok {
		s.BatteryVoltage = scalarString(x)
	}
	if x, ok := findKey(v, "RPM"); ok {
		s.RPM = parseRPM(x)
	}
	return s, nil
}

// AlarmLog fetches /cmd/logs_json and returns the parsed Alarm Log, newest first.
func (c *Client) AlarmLog(ctx context.Context) ([]LogEntry, error) {
	v, err := c.getJSON(ctx, "/cmd/logs_json")
	if err != nil {
		return nil, err
	}
	var out []LogEntry
	x, ok := findKey(v, "Alarm Log")
	if !ok {
		return out, nil
	}
	var lines []any
	switch t := x.(type) {
	case []any:
		lines = t
	case string:
		lines = []any{t}
	}
	for _, l := range lines {
		s, ok := l.(string)
		if !ok {
			continue
		}
		if e, ok := ParseLogLine(s, c.loc); ok {
			out = append(out, e)
		}
	}
	return out, nil
}

// SetRemote sends a remote command. Only "resetalarm" and "auto" are permitted.
func (c *Client) SetRemote(ctx context.Context, cmd string) error {
	if cmd != "resetalarm" && cmd != "auto" {
		return fmt.Errorf("genmon: remote command %q not allowed", cmd)
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	var r response
	for attempt := 0; attempt < 2; attempt++ {
		if c.legacyKnown && c.legacy {
			var err error
			r, err = c.cmd(ctx, http.MethodGet, "/cmd/setremote?"+url.Values{"setremote": {cmd}}.Encode(), nil, nil)
			if err != nil {
				return err
			}
			break
		}
		tok, err := c.csrfToken(ctx)
		if errors.Is(err, errUnknownCommand) {
			// genmon <= 2.0.03: no CSRF token, setremote is a GET.
			c.legacy, c.legacyKnown = true, true
			attempt--
			continue
		}
		if err != nil {
			return err
		}
		c.legacy, c.legacyKnown = false, true
		r, err = c.cmd(ctx, http.MethodPost, "/cmd/setremote", url.Values{"setremote": {cmd}}, map[string]string{"X-CSRFToken": tok})
		if err != nil {
			return err
		}
		if r.status != http.StatusForbidden {
			break
		}
	}
	if r.status < 200 || r.status > 299 {
		return fmt.Errorf("genmon: POST /cmd/setremote: HTTP %d: %s", r.status, snippet(r.body))
	}
	text := strings.TrimSpace(string(r.body))
	var js string
	if json.Unmarshal(r.body, &js) == nil {
		text = strings.TrimSpace(js)
	}
	if strings.HasPrefix(text, "Remote command sent successfully") {
		return nil
	}
	if len(text) > 200 {
		text = text[:200]
	}
	return fmt.Errorf("genmon: setremote %s rejected: %s", cmd, text)
}

// csrfToken must be called with c.mu held.
func (c *Client) csrfToken(ctx context.Context) (string, error) {
	r, err := c.cmd(ctx, http.MethodGet, "/cmd/csrf_token", nil, nil)
	if err != nil {
		return "", err
	}
	if r.status < 200 || r.status > 299 {
		return "", fmt.Errorf("genmon: GET /cmd/csrf_token: HTTP %d: %s", r.status, snippet(r.body))
	}
	var t struct {
		Token string `json:"csrf_token"`
	}
	if err := json.Unmarshal(r.body, &t); err != nil || t.Token == "" {
		return "", errors.New("genmon: csrf_token response missing token")
	}
	return t.Token, nil
}

var (
	logLineRe = regexp.MustCompile(`^(\d{2}/\d{2}/\d{2} \d{2}:\d{2}:\d{2})\s+(.*)$`)
	logCodeRe = regexp.MustCompile(`^(.*?)\s*:\s*Alarm Code:\s*(\d+)\s*$`)
)

// ParseLogLine parses "MM/DD/YY HH:MM:SS <text> : Alarm Code: <n>".
func ParseLogLine(line string, loc *time.Location) (LogEntry, bool) {
	if loc == nil {
		loc = time.Local
	}
	raw := strings.TrimSpace(line)
	m := logLineRe.FindStringSubmatch(raw)
	if m == nil {
		return LogEntry{}, false
	}
	t, err := time.ParseInLocation("01/02/06 15:04:05", m[1], loc)
	if err != nil {
		return LogEntry{}, false
	}
	e := LogEntry{Raw: line, Time: t, Text: strings.TrimSpace(m[2])}
	if cm := logCodeRe.FindStringSubmatch(m[2]); cm != nil {
		e.Text = strings.TrimSpace(cm[1])
		e.Code, _ = strconv.Atoi(cm[2])
	}
	return e, true
}

// findKey walks decoded JSON depth-first and returns the first value for key.
func findKey(v any, key string) (any, bool) {
	switch t := v.(type) {
	case map[string]any:
		if x, ok := t[key]; ok {
			return x, true
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if x, ok := findKey(t[k], key); ok {
				return x, true
			}
		}
	case []any:
		for _, e := range t {
			if x, ok := findKey(e, key); ok {
				return x, true
			}
		}
	}
	return nil, false
}

func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	return ""
}

func parseRPM(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case string:
		s := strings.TrimSpace(t)
		end := 0
		for end < len(s) && (s[end] >= '0' && s[end] <= '9' || (end == 0 && s[end] == '-')) {
			end++
		}
		if n, err := strconv.Atoi(s[:end]); err == nil {
			return n
		}
	}
	return -1
}
