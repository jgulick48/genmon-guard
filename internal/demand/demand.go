// Package demand reads the generator start demand from a Prometheus
// text-format /metrics endpoint.
package demand

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Config configures a Scraper.
type Config struct {
	URL     string
	Metric  string            // default "lxp_gen_dry_contact"
	Labels  map[string]string // optional label matchers, all must match exactly
	Timeout time.Duration     // 0 -> 5s
}

// Scraper fetches the demand gauge.
type Scraper struct {
	cfg Config
	hc  *http.Client
}

// New builds a Scraper. A nil hc yields a default client using cfg.Timeout.
func New(cfg Config, hc *http.Client) *Scraper {
	if cfg.Metric == "" {
		cfg.Metric = "lxp_gen_dry_contact"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if hc == nil {
		hc = &http.Client{Timeout: cfg.Timeout}
	}
	return &Scraper{cfg: cfg, hc: hc}
}

// Demand reports whether the generator is wanted. Any error means demand is
// unknown and the caller should take no action.
func (s *Scraper) Demand(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.URL, nil)
	if err != nil {
		return false, fmt.Errorf("demand: build request: %w", err)
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return false, fmt.Errorf("demand: fetch metrics: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return false, fmt.Errorf("demand: metrics endpoint returned HTTP %d", resp.StatusCode)
	}
	v, err := ParseValue(io.LimitReader(resp.Body, 16<<20), s.cfg.Metric, s.cfg.Labels)
	if err != nil {
		return false, err
	}
	return v >= 0.5, nil
}

// ParseValue finds the sample of the named metric whose labels include all of
// the given matchers and returns its value. It errors if the metric is not
// found, the value is NaN, or several matching series disagree.
func ParseValue(r io.Reader, metric string, labels map[string]string) (float64, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var (
		found bool
		val   float64
	)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		name, lbls, v, ok := parseSample(line)
		if !ok || name != metric || !matches(lbls, labels) {
			continue
		}
		if math.IsNaN(v) {
			return 0, fmt.Errorf("demand: metric %q value is NaN", metric)
		}
		if found && v != val {
			return 0, fmt.Errorf("demand: metric %q has multiple series with different values; set labels to select one", metric)
		}
		found, val = true, v
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("demand: read metrics: %w", err)
	}
	if !found {
		return 0, errors.New("demand: metric " + strconv.Quote(metric) + " not found")
	}
	return val, nil
}

func matches(have, want map[string]string) bool {
	for k, v := range want {
		if got, ok := have[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// parseSample parses `name{labels} value [timestamp]` or `name value [timestamp]`.
func parseSample(line string) (name string, labels map[string]string, value float64, ok bool) {
	i := 0
	for i < len(line) && line[i] != '{' && line[i] != ' ' && line[i] != '\t' {
		i++
	}
	if i == 0 {
		return "", nil, 0, false
	}
	name = line[:i]
	labels = map[string]string{}
	if i < len(line) && line[i] == '{' {
		i++
		for {
			for i < len(line) && (line[i] == ' ' || line[i] == '\t' || line[i] == ',') {
				i++
			}
			if i >= len(line) {
				return "", nil, 0, false
			}
			if line[i] == '}' {
				i++
				break
			}
			start := i
			for i < len(line) && line[i] != '=' && line[i] != '}' {
				i++
			}
			if i >= len(line) || line[i] != '=' {
				return "", nil, 0, false
			}
			key := strings.TrimSpace(line[start:i])
			i++ // '='
			if i >= len(line) || line[i] != '"' {
				return "", nil, 0, false
			}
			i++
			var b strings.Builder
			closed := false
			for i < len(line) {
				c := line[i]
				if c == '\\' && i+1 < len(line) {
					switch line[i+1] {
					case 'n':
						b.WriteByte('\n')
					case '\\':
						b.WriteByte('\\')
					case '"':
						b.WriteByte('"')
					default:
						b.WriteByte('\\')
						b.WriteByte(line[i+1])
					}
					i += 2
					continue
				}
				if c == '"' {
					closed = true
					i++
					break
				}
				b.WriteByte(c)
				i++
			}
			if !closed || key == "" {
				return "", nil, 0, false
			}
			labels[key] = b.String()
		}
	}
	fields := strings.Fields(line[i:])
	if len(fields) == 0 || len(fields) > 2 {
		return "", nil, 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return "", nil, 0, false
	}
	if len(fields) == 2 {
		if _, err := strconv.ParseInt(fields[1], 10, 64); err != nil {
			return "", nil, 0, false
		}
	}
	return name, labels, v, true
}
