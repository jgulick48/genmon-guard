// Package metrics is a tiny Prometheus text-format registry (no dependencies),
// sized for genmon-guard's handful of series.
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type kind string

const (
	counter kind = "counter"
	gauge   kind = "gauge"
)

type family struct {
	help   string
	kind   kind
	values map[string]float64 // keyed by rendered label set
}

// Registry holds counters and gauges.
type Registry struct {
	mu       sync.Mutex
	families map[string]*family
}

// New returns an empty registry.
func New() *Registry { return &Registry{families: map[string]*family{}} }

// Labels is an ordered list of name/value pairs, e.g. Labels{"result", "sent"}.
type Labels []string

func (l Labels) render() string {
	if len(l) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i+1 < len(l); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%s", l[i], strconv.Quote(l[i+1]))
	}
	b.WriteByte('}')
	return b.String()
}

func (r *Registry) fam(name, help string, k kind) *family {
	f, ok := r.families[name]
	if !ok {
		f = &family{help: help, kind: k, values: map[string]float64{}}
		r.families[name] = f
	}
	return f
}

// Describe registers a series so it is exported (at 0) before its first update.
func (r *Registry) Describe(name, help string, isCounter bool, labels Labels) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := gauge
	if isCounter {
		k = counter
	}
	f := r.fam(name, help, k)
	if _, ok := f.values[labels.render()]; !ok {
		f.values[labels.render()] = 0
	}
}

// Inc adds 1 to a counter.
func (r *Registry) Inc(name string, labels Labels) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fam(name, "", counter).values[labels.render()]++
}

// Set sets a gauge.
func (r *Registry) Set(name string, v float64, labels Labels) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fam(name, "", gauge).values[labels.render()] = v
}

// SetBool sets a gauge to 1 or 0.
func (r *Registry) SetBool(name string, v bool) {
	f := 0.0
	if v {
		f = 1
	}
	r.Set(name, f, nil)
}

// ServeHTTP writes the registry in the Prometheus text exposition format.
func (r *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.families))
	for n := range r.families {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f := r.families[n]
		if f.help != "" {
			fmt.Fprintf(w, "# HELP %s %s\n", n, f.help)
		}
		fmt.Fprintf(w, "# TYPE %s %s\n", n, f.kind)
		keys := make([]string, 0, len(f.values))
		for k := range f.values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "%s%s %s\n", n, k, strconv.FormatFloat(f.values[k], 'g', -1, 64))
		}
	}
}
