package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Slack posts alerts to a Slack incoming webhook.
type Slack struct {
	webhookURL string
	hc         *http.Client
	loc        *time.Location
}

// NewSlack builds a Slack alerter. A nil hc gets a 10s-timeout client; a nil
// loc formats times in time.Local.
func NewSlack(webhookURL string, hc *http.Client, loc *time.Location) *Slack {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	if loc == nil {
		loc = time.Local
	}
	return &Slack{webhookURL: webhookURL, hc: hc, loc: loc}
}

func slackEmoji(l Level) string {
	switch l {
	case Warning:
		return ":warning:"
	case Error:
		return ":rotating_light:"
	default:
		return ":white_check_mark:"
	}
}

func (s *Slack) format(a Alert) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s *genmon-guard: %s*", slackEmoji(a.Level), a.Title)
	if a.DryRun {
		b.WriteString("  [DRY RUN]")
	}
	b.WriteByte('\n')
	if a.Text != "" {
		b.WriteString(a.Text)
		b.WriteByte('\n')
	}
	if a.AlarmLine != "" {
		fmt.Fprintf(&b, "Alarm: `%s`\n", a.AlarmLine)
	}
	t := a.Time
	if t.IsZero() {
		t = time.Now()
	}
	var parts []string
	if a.BatteryVoltage != "" {
		parts = append(parts, "Battery: "+a.BatteryVoltage)
	}
	parts = append(parts, "Time: "+t.In(s.loc).Format("Mon 15:04:05 MST"))
	b.WriteString(strings.Join(parts, " · "))
	return b.String()
}

// Send posts the alert. Errors never contain the webhook URL.
func (s *Slack) Send(ctx context.Context, a Alert) error {
	body, err := json.Marshal(map[string]string{"text": s.format(a)})
	if err != nil {
		return fmt.Errorf("slack webhook: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.webhookURL, bytes.NewReader(body))
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("slack webhook: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("slack webhook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("slack webhook: status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}
