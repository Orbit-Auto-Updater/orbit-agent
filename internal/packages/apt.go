package packages

import (
	"bufio"
	"context"
	"os"
	"strings"

	"linux-dash-agent/internal/execx"
	"linux-dash-agent/internal/protocol"
)

// Apt supports Debian, Ubuntu and derivatives.
type Apt struct{ r execx.Runner }

func (a *Apt) Name() string { return "apt" }

var aptEnv = []string{
	"DEBIAN_FRONTEND=noninteractive",
	"APT_LISTCHANGES_FRONTEND=none",
	"NEEDRESTART_MODE=l", // list services needing restart, never prompt
}

var aptOpts = []string{
	"-y", "-q",
	"-o", "Dpkg::Options::=--force-confdef",
	"-o", "Dpkg::Options::=--force-confold",
	"-o", "DPkg::Lock::Timeout=300", // wait for unattended-upgrades etc.
}

func (a *Apt) Refresh(ctx context.Context) error {
	_, err := run(ctx, a.r, execx.Cmd{Name: "apt-get", Args: []string{"-q", "-o", "DPkg::Lock::Timeout=300", "update"}, Env: aptEnv, Timeout: refreshTimeout})
	return err
}

func (a *Apt) List(ctx context.Context) ([]protocol.Package, error) {
	res, err := run(ctx, a.r, execx.Cmd{Name: "apt", Args: []string{"list", "--upgradable"}, Env: aptEnv, Timeout: refreshTimeout})
	if err != nil {
		return nil, err
	}
	pkgs := ParseAptUpgradable(string(res.Stdout))
	if len(pkgs) == 0 {
		return pkgs, nil
	}
	// Source package names (best effort) for vulnerability lookups.
	args := []string{"-W", "-f", "${Package}\t${source:Package}\n", "--"}
	for _, p := range pkgs {
		args = append(args, p.Name)
	}
	if res, err := a.r.Run(ctx, execx.Cmd{Name: "dpkg-query", Args: args}); err == nil {
		src := ParseDpkgSources(string(res.Stdout))
		for i := range pkgs {
			pkgs[i].Source = src[pkgs[i].Name]
		}
	}
	return pkgs, nil
}

// ParseDpkgSources parses "binary<TAB>source" lines from dpkg-query.
func ParseDpkgSources(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if bin, src, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok && bin != "" && src != "" {
			m[strings.SplitN(bin, ":", 2)[0]] = src
		}
	}
	return m
}

func (a *Apt) UpgradeAll(ctx context.Context, exclude []string) (string, error) {
	// --with-new-pkgs lets kernel meta-packages pull in new versions without
	// the removals dist-upgrade could perform.
	args := append(append([]string{}, aptOpts...), "upgrade", "--with-new-pkgs")
	res, err := run(ctx, a.r, execx.Cmd{Name: "apt-get", Args: args, Env: aptEnv, Timeout: upgradeTimeout})
	return res.Combined(), err
}

func (a *Apt) UpgradePackages(ctx context.Context, names []string, _ []protocol.Package) (string, error) {
	args := append(append([]string{}, aptOpts...), "install", "--only-upgrade", "--")
	res, err := run(ctx, a.r, execx.Cmd{Name: "apt-get", Args: append(args, names...), Env: aptEnv, Timeout: upgradeTimeout})
	return res.Combined(), err
}

func (a *Apt) RebootRequired(context.Context) bool {
	for _, p := range []string{"/run/reboot-required", "/var/run/reboot-required"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// ParseAptUpgradable parses `apt list --upgradable`, e.g.
//
//	openssl/noble-updates,noble-security 3.0.13-0ubuntu3.4 amd64 [upgradable from: 3.0.13-0ubuntu3.3]
func ParseAptUpgradable(out string) []protocol.Package {
	var pkgs []protocol.Package
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		nameRepo, rest, ok := strings.Cut(line, " ")
		if !ok || !strings.Contains(nameRepo, "/") {
			continue // "Listing..." and warnings
		}
		name, repos, _ := strings.Cut(nameRepo, "/")
		f := strings.Fields(rest)
		if len(f) < 2 {
			continue
		}
		p := protocol.Package{Name: name, Available: f[0], Arch: f[1], Repo: repos}
		if _, from, ok := strings.Cut(rest, "upgradable from: "); ok {
			p.Current = strings.TrimSuffix(strings.TrimSpace(from), "]")
		}
		for _, r := range strings.Split(repos, ",") {
			if strings.HasSuffix(r, "-security") || strings.Contains(r, "/updates") {
				p.Security = true
			}
		}
		pkgs = append(pkgs, p)
	}
	return pkgs
}
