package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func load(t *testing.T, body string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestLoadAppliesDefaultsAndOverrides(t *testing.T) {
	c, err := load(t, `
server:
  url: https://dash.example.com
intervals:
  report: 5s
pm2:
  instances:
    - user: deploy
updates:
  policy:
    enabled: true
    scope: all
    window: {days: [Sun, wed], start: "02:30", duration_minutes: 90, timezone: Europe/London}
actions:
  reboot: true
`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Intervals.Report != 5*time.Second || c.Intervals.UpdateCheck != 6*time.Hour {
		t.Fatalf("intervals: %+v", c.Intervals)
	}
	if !c.Docker.Enabled || !c.Actions.Docker || !c.Actions.Reboot || c.PM2.Instances[0].User != "deploy" {
		t.Fatalf("defaults/overrides wrong: %+v", c)
	}
	if c.Updates.Policy.Window.DurationMinutes != 90 || c.Updates.Policy.Scope != "all" {
		t.Fatalf("policy: %+v", c.Updates.Policy)
	}
}

func TestValidateRejectsUnsafeOrBrokenConfig(t *testing.T) {
	cases := map[string]string{
		"https":   "server: {url: http://dash.example.com}",
		"missing": "log_level: debug",
		"scope":   "server: {url: https://x}\nupdates: {policy: {scope: everything}}",
		"start":   "server: {url: https://x}\nupdates: {policy: {scope: all, window: {start: '25:00'}}}",
		"tz":      "server: {url: https://x}\nupdates: {policy: {scope: all, window: {start: '02:00', timezone: Mars/Base}}}",
		"day":     "server: {url: https://x}\nupdates: {policy: {scope: all, window: {start: '02:00', days: [funday]}}}",
		"noplan":  "server: {url: https://x}\nupdates: {policy: {enabled: true, scope: all}}",
		"report":  "server: {url: https://x}\nintervals: {report: 500ms}",
	}
	for name, body := range cases {
		if _, err := load(t, body); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
	c, err := load(t, "server: {url: http://localhost:8080, allow_insecure_http: true}")
	if err != nil || !strings.HasPrefix(c.Server.URL, "http://") {
		t.Fatalf("explicit insecure http should be allowed: %v", err)
	}
}
