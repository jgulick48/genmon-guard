// Package genmon is a small client for the genmon web API (github.com/jgyates/genmon).
package genmon

import (
	"strings"
	"time"
)

// Switch State values reported by an Evolution controller.
const (
	SwitchAuto    = "Auto"
	SwitchOff     = "Off"
	SwitchManual  = "Manual"
	SwitchAlarm   = "System in Alarm"
	SwitchTwoWire = "Two Wire Stop"
)

// Status is the subset of /cmd/status_json that genmon-guard uses.
type Status struct {
	SwitchState    string // e.g. "Auto", "System in Alarm"
	EngineState    string // e.g. "Stopped", "Running", "Cranking"
	RPM            int    // -1 when not reported
	BatteryVoltage string // as displayed, e.g. "13.20 V"
}

// InAlarm reports whether the controller says it is stopped in alarm.
func (s Status) InAlarm() bool { return strings.EqualFold(s.SwitchState, SwitchAlarm) }

// LogEntry is one parsed alarm-log line, for example
// "11/15/26 07:00:59 RPM Sense Loss : Alarm Code: 1500".
type LogEntry struct {
	Raw  string    // the full line as genmon returned it
	Time time.Time // controller local time, in the configured location
	Text string    // e.g. "RPM Sense Loss"
	Code int       // e.g. 1500; 0 when the line carries no code
}

// IsZero reports whether e is the empty entry.
func (e LogEntry) IsZero() bool { return e.Raw == "" }
