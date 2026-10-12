// Package packages wraps the host package manager (apt, dnf/yum, apk, pacman)
// behind one interface for listing and applying upgrades.
package packages

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"linux-dash-agent/internal/execx"
	"linux-dash-agent/internal/protocol"
)

const (
	refreshTimeout = 10 * time.Minute
	upgradeTimeout = 60 * time.Minute
)

type Manager interface {
	Name() string
	// Refresh updates repository metadata.
	Refresh(ctx context.Context) error
	// List returns packages with an available upgrade.
	List(ctx context.Context) ([]protocol.Package, error)
	// UpgradeAll upgrades everything except exclude.
	UpgradeAll(ctx context.Context, exclude []string) (string, error)
	// UpgradePackages upgrades only names (a subset of pending).
	UpgradePackages(ctx context.Context, names []string, pending []protocol.Package) (string, error)
	RebootRequired(ctx context.Context) bool
}

var ErrNoManager = errors.New("no supported package manager found (apt, dnf, yum, apk, pacman)")

// Detect picks the host's package manager.
func Detect(r execx.Runner) (Manager, error) {
	switch {
	case has(r, "apt-get"):
		return &Apt{r: r}, nil
	case has(r, "dnf"):
		return &DNF{r: r, bin: "dnf"}, nil
	case has(r, "yum"):
		return &DNF{r: r, bin: "yum"}, nil
	case has(r, "apk"):
		return &Apk{r: r}, nil
	case has(r, "pacman"):
		return &Pacman{r: r}, nil
	}
	return nil, ErrNoManager
}

func has(r execx.Runner, bin string) bool {
	_, err := r.LookPath(bin)
	return err == nil
}

type UpgradeOptions struct {
	Packages     []string
	SecurityOnly bool
	Exclude      []string
}

// Select decides what an upgrade will touch. full is true when everything
// pending should be upgraded with the manager's native "upgrade all".
func Select(pending []protocol.Package, opts UpgradeOptions) (targets []string, full bool, err error) {
	for _, n := range append(slices.Clone(opts.Packages), opts.Exclude...) {
		if !ValidPackageName(n) {
			return nil, false, fmt.Errorf("invalid package name %q", n)
		}
	}
	if len(opts.Packages) == 0 && !opts.SecurityOnly && len(opts.Exclude) == 0 {
		return nil, true, nil
	}
	for _, p := range pending {
		if len(opts.Packages) > 0 && !slices.Contains(opts.Packages, p.Name) {
			continue
		}
		if opts.SecurityOnly && !p.Security {
			continue
		}
		if slices.Contains(opts.Exclude, p.Name) {
			continue
		}
		if !slices.Contains(targets, p.Name) {
			targets = append(targets, p.Name)
		}
	}
	return targets, false, nil
}

// Apply runs an upgrade according to opts and returns what was targeted.
func Apply(ctx context.Context, m Manager, pending []protocol.Package, opts UpgradeOptions) (applied []string, output string, err error) {
	targets, full, err := Select(pending, opts)
	if err != nil {
		return nil, "", err
	}
	if full {
		out, err := m.UpgradeAll(ctx, nil)
		for _, p := range pending {
			applied = append(applied, p.Name)
		}
		return applied, out, err
	}
	if len(targets) == 0 {
		return nil, "nothing to upgrade", nil
	}
	out, err := m.UpgradePackages(ctx, targets, pending)
	return targets, out, err
}

var pkgNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+._:@~-]{0,254}$`)

func ValidPackageName(s string) bool { return pkgNameRe.MatchString(s) }

// run executes a package-manager command and turns a non-zero exit into an
// error carrying the tail of its output.
func run(ctx context.Context, r execx.Runner, c execx.Cmd, okCodes ...int) (execx.Result, error) {
	res, err := r.Run(ctx, c)
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 && !slices.Contains(okCodes, res.ExitCode) {
		return res, fmt.Errorf("%s exited %d: %s", c.Name, res.ExitCode, execx.Tail(res.Combined(), 1500))
	}
	return res, nil
}

// kernelUpgraded reports whether the running kernel's modules are gone, which
// happens after a kernel upgrade removes the old version (Arch, Alpine).
func kernelUpgraded() bool {
	if _, err := os.Stat("/lib/modules"); err != nil {
		return false
	}
	rel, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return false
	}
	_, err = os.Stat("/lib/modules/" + strings.TrimSpace(string(rel)))
	return errors.Is(err, os.ErrNotExist)
}
