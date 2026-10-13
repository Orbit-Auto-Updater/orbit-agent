// Package config loads the agent's YAML configuration.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"orbit-agent/internal/apps"
	"orbit-agent/internal/protocol"
)

const DefaultPath = "/etc/orbit-agent/config.yaml"

type Config struct {
	Server struct {
		URL                string `yaml:"url"`
		EnrollToken        string `yaml:"enroll_token"`
		CAFile             string `yaml:"ca_file"`
		InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
		AllowInsecureHTTP  bool   `yaml:"allow_insecure_http"`
	} `yaml:"server"`

	StateDir string `yaml:"state_dir"`
	LogLevel string `yaml:"log_level"`

	Intervals struct {
		Report      time.Duration `yaml:"report"`
		Apps        time.Duration `yaml:"apps"`
		UpdateCheck time.Duration `yaml:"update_check"`
	} `yaml:"intervals"`

	Metrics struct {
		ExcludeInterfaces []string `yaml:"exclude_interfaces"`
		ExcludeMounts     []string `yaml:"exclude_mounts"`
	} `yaml:"metrics"`

	Docker struct {
		Enabled bool   `yaml:"enabled"`
		Socket  string `yaml:"socket"`
	} `yaml:"docker"`

	PM2 struct {
		Enabled   bool               `yaml:"enabled"`
		Instances []apps.PM2Instance `yaml:"instances"`
	} `yaml:"pm2"`

	Systemd struct {
		Enabled    bool     `yaml:"enabled"`
		Watch      []string `yaml:"watch"`
		ControlAny bool     `yaml:"control_any"`
	} `yaml:"systemd"`

	Journal struct {
		Enabled    bool   `yaml:"enabled"`
		Priority   string `yaml:"priority"`
		MaxEntries int    `yaml:"max_entries"`
	} `yaml:"journal"`

	Updates struct {
		Enabled            bool            `yaml:"enabled"`
		AllowRemotePolicy  bool            `yaml:"allow_remote_policy"`
		MinFreeDiskPercent float64         `yaml:"min_free_disk_percent"`
		Policy             protocol.Policy `yaml:"policy"`
	} `yaml:"updates"`

	// Actions is the local allowlist for remote commands. The dashboard can
	// never do more on this host than what is enabled here.
	Actions struct {
		Updates bool `yaml:"updates"`
		Reboot  bool `yaml:"reboot"`
		Docker  bool `yaml:"docker"`
		PM2     bool `yaml:"pm2"`
		Systemd bool `yaml:"systemd"`
		// AgentUpdate lets the dashboard install a new agent version.
		AgentUpdate bool `yaml:"agent_update"`
	} `yaml:"actions"`
}

func Default() *Config {
	c := &Config{StateDir: "/var/lib/orbit-agent", LogLevel: "info"}
	c.Intervals.Report = 10 * time.Second
	c.Intervals.Apps = 30 * time.Second
	c.Intervals.UpdateCheck = 6 * time.Hour
	c.Metrics.ExcludeInterfaces = []string{"lo", "veth*", "docker0", "br-*", "virbr*", "cni*", "flannel*", "cali*"}
	c.Metrics.ExcludeMounts = []string{"/snap/*", "/var/lib/docker/*", "/var/lib/containers/*"}
	c.Docker.Enabled = true
	c.Docker.Socket = "/var/run/docker.sock"
	c.Systemd.Enabled = true
	c.Journal.Enabled = true
	c.Journal.Priority = "err"
	c.Journal.MaxEntries = 200
	c.Updates.Enabled = true
	c.Updates.AllowRemotePolicy = true
	c.Updates.MinFreeDiskPercent = 10
	c.Updates.Policy = protocol.Policy{Scope: "security"}
	c.Actions.Updates = true
	c.Actions.Docker = true
	c.Actions.PM2 = true
	c.Actions.Systemd = true
	c.Actions.AgentUpdate = true
	return c
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := Default()
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func (c *Config) Validate() error {
	var errs []error
	u, err := url.Parse(c.Server.URL)
	switch {
	case c.Server.URL == "":
		errs = append(errs, errors.New("server.url is required"))
	case err != nil || u.Host == "":
		errs = append(errs, fmt.Errorf("server.url %q is not a valid URL", c.Server.URL))
	case u.Scheme == "http" && !c.Server.AllowInsecureHTTP:
		errs = append(errs, errors.New("server.url must use https (set server.allow_insecure_http for local development only)"))
	case u.Scheme != "http" && u.Scheme != "https":
		errs = append(errs, fmt.Errorf("server.url scheme %q is not supported", u.Scheme))
	}
	if c.Intervals.Report < 2*time.Second {
		errs = append(errs, errors.New("intervals.report must be at least 2s"))
	}
	if c.Intervals.Apps < c.Intervals.Report {
		c.Intervals.Apps = c.Intervals.Report
	}
	if c.Intervals.UpdateCheck < 10*time.Minute {
		errs = append(errs, errors.New("intervals.update_check must be at least 10m"))
	}
	if c.Updates.MinFreeDiskPercent < 0 || c.Updates.MinFreeDiskPercent > 90 {
		errs = append(errs, errors.New("updates.min_free_disk_percent must be between 0 and 90"))
	}
	if err := ValidatePolicy(c.Updates.Policy); err != nil {
		errs = append(errs, fmt.Errorf("updates.policy: %w", err))
	}
	return errors.Join(errs...)
}

// ValidatePolicy checks a policy from config or from the control plane.
func ValidatePolicy(p protocol.Policy) error {
	if p.Scope != "security" && p.Scope != "all" {
		return fmt.Errorf("scope must be \"security\" or \"all\", got %q", p.Scope)
	}
	if p.Window != nil {
		if _, err := time.Parse("15:04", p.Window.Start); err != nil {
			return fmt.Errorf("window.start %q must be HH:MM", p.Window.Start)
		}
		if p.Window.DurationMinutes < 0 || p.Window.DurationMinutes > 24*60 {
			return errors.New("window.duration_minutes must be between 0 and 1440")
		}
		if _, err := time.LoadLocation(p.Window.Timezone); err != nil {
			return fmt.Errorf("window.timezone: %w", err)
		}
		for _, d := range p.Window.Days {
			if _, ok := ParseWeekday(d); !ok {
				return fmt.Errorf("window.days: unknown day %q", d)
			}
		}
	} else if p.Enabled && p.IntervalHours <= 0 {
		return errors.New("an enabled policy needs a window or interval_hours")
	}
	return nil
}

// ParseWeekday accepts "sun", "Sunday", "SUN", etc.
func ParseWeekday(s string) (time.Weekday, bool) {
	if len(s) < 3 {
		return 0, false
	}
	days := map[string]time.Weekday{"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday}
	b := []byte(s[:3])
	for i := range b {
		b[i] |= 0x20
	}
	d, ok := days[string(b)]
	return d, ok
}
