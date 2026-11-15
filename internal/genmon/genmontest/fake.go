// Package genmontest provides a fake genmon server for tests.
package genmontest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const loginPage = `<html><head><title>Login</title></head><body><form method="post"><input name="username"><input name="password" type="password"></form></body></html>`

// unknownCommandPage mirrors genmon's error page for an unrecognised /cmd/ command.
const unknownCommandPage = `<!DOCTYPE html><html><head><title>Genmon &mdash; Error</title></head><body><h1>Unknown Command</h1><p>The command %s is not recognized.</p></body></html>`

const lockedPage = `<html><body>Account locked: too many failed login attempts</body></html>`

// Fake mimics the parts of the genmon web UI used by the client.
type Fake struct {
	mu sync.Mutex

	Server *httptest.Server

	Username, Password string
	CSRFToken          string
	SwitchState        string
	EngineState        string
	BatteryVoltage     string
	RPM                int
	AlarmLog           []string // newest first
	ReadOnly           bool
	Commands           []string // setremote values received, in order
	LoginAttempts      int
	FailedLogins       int
	// LockoutAfter, when > 0, makes the fake answer with a "locked" page once
	// FailedLogins reaches it (even for the right password).
	LockoutAfter int
	// Legacy mimics genmon <= 2.0.03 (the release tagged 2.0.03): /cmd/<command> is
	// GET-only (POST gets 405), there is no csrf_token command, and setremote
	// takes its value from the query string with no CSRF token.
	Legacy bool

	sessions map[string]bool
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// New starts a TLS fake server that is closed via t.Cleanup.
func New(t testing.TB) *Fake {
	t.Helper()
	f := &Fake{
		Username:       "admin",
		Password:       "secret",
		CSRFToken:      randHex(16),
		SwitchState:    "Auto",
		EngineState:    "Stopped",
		BatteryVoltage: "13.20 V",
		sessions:       map[string]bool{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", f.handleRoot)
	mux.HandleFunc("/cmd/", f.handleCmd)
	f.Server = httptest.NewTLSServer(mux)
	t.Cleanup(f.Server.Close)
	return f
}

// URL returns the base URL of the server.
func (f *Fake) URL() string { return f.Server.URL }

// ExpireSessions invalidates all sessions.
func (f *Fake) ExpireSessions() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions = map[string]bool{}
}

// Set mutates state under the lock.
func (f *Fake) Set(fn func(f *Fake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// CommandsSent returns a copy of the setremote values received.
func (f *Fake) CommandsSent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Commands...)
}

// Logins returns (attempts, failures).
func (f *Fake) Logins() (attempts, failed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.LoginAttempts, f.FailedLogins
}

func html(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(body))
}

func (f *Fake) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		html(w, loginPage)
		return
	}
	_ = r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.LoginAttempts++
	if f.LockoutAfter > 0 && f.FailedLogins >= f.LockoutAfter {
		f.FailedLogins++
		html(w, lockedPage)
		return
	}
	if r.PostForm.Get("username") != f.Username || r.PostForm.Get("password") != f.Password {
		f.FailedLogins++
		html(w, loginPage)
		return
	}
	sid := randHex(16)
	f.sessions[sid] = true
	http.SetCookie(w, &http.Cookie{Name: "session", Value: sid, Path: "/", Secure: true, HttpOnly: true})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (f *Fake) authed(r *http.Request) bool {
	c, err := r.Cookie("session")
	if err != nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[c.Value]
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type obj = map[string]any

func (f *Fake) handleCmd(w http.ResponseWriter, r *http.Request) {
	if !f.authed(r) {
		html(w, loginPage)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Legacy {
		f.handleLegacy(w, r)
		return
	}
	switch r.URL.Path {
	case "/cmd/status_json", "/cmd/logs_json":
		f.writeReadJSON(w, r.URL.Path)
	case "/cmd/csrf_token":
		writeJSON(w, 200, obj{"csrf_token": f.CSRFToken})
	case "/cmd/setremote":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("X-CSRFToken") != f.CSRFToken || f.CSRFToken == "" {
			writeJSON(w, 403, obj{"error": "CSRF validation failed"})
			return
		}
		_ = r.ParseForm()
		if f.ReadOnly {
			writeJSON(w, 200, "Read Only Mode")
			return
		}
		cmd := r.PostForm.Get("setremote")
		f.Commands = append(f.Commands, cmd)
		if cmd == "auto" {
			f.SwitchState = "Auto"
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("Remote command sent successfully"))
	default:
		html(w, fmt.Sprintf(unknownCommandPage, strings.TrimPrefix(r.URL.Path, "/cmd/")))
	}
}

// writeReadJSON serves status_json / logs_json in genmon's shapes. f.mu held.
func (f *Fake) writeReadJSON(w http.ResponseWriter, path string) {
	if path == "/cmd/status_json" {
		writeJSON(w, 200, obj{"Status": []any{
			obj{"Engine": []any{
				obj{"Switch State": f.SwitchState},
				obj{"Engine State": f.EngineState},
				obj{"Battery Voltage": f.BatteryVoltage},
				obj{"RPM": itoa(f.RPM)},
			}},
			obj{"Line": []any{obj{"Utility Voltage": "240 V"}}},
		}})
		return
	}
	alarm := f.AlarmLog
	if alarm == nil {
		alarm = []string{}
	}
	writeJSON(w, 200, obj{"Logs": []any{
		obj{"Alarm Log": alarm},
		obj{"Service Log": []string{}},
		obj{"Run Log": []string{}},
	}})
}

// handleLegacy serves /cmd/ like genmon 2.0.03. Called with f.mu held.
func (f *Fake) handleLegacy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch r.URL.Path {
	case "/cmd/status_json", "/cmd/logs_json":
		f.writeReadJSON(w, r.URL.Path)
	case "/cmd/setremote":
		if f.ReadOnly {
			writeJSON(w, 200, "Read Only Mode")
			return
		}
		cmd := r.URL.Query().Get("setremote")
		f.Commands = append(f.Commands, cmd)
		if cmd == "auto" {
			f.SwitchState = "Auto"
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("Remote command sent successfully"))
	default:
		html(w, fmt.Sprintf(unknownCommandPage, strings.TrimPrefix(r.URL.Path, "/cmd/")))
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
