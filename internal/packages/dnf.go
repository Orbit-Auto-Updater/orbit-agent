package packages

import (
	"bufio"
	"context"
	"strings"

	"linux-dash-agent/internal/execx"
	"linux-dash-agent/internal/protocol"
)

// DNF supports Fedora, RHEL, Rocky, Alma and CentOS (dnf or legacy yum).
type DNF struct {
	r   execx.Runner
	bin string
}

func (d *DNF) Name() string { return d.bin }

func (d *DNF) Refresh(ctx context.Context) error {
	_, err := run(ctx, d.r, execx.Cmd{Name: d.bin, Args: []string{"-q", "makecache"}, Timeout: refreshTimeout})
	return err
}

func (d *DNF) List(ctx context.Context) ([]protocol.Package, error) {
	// check-update exits 100 when updates are available.
	res, err := run(ctx, d.r, execx.Cmd{Name: d.bin, Args: []string{"-q", "check-update"}, Timeout: refreshTimeout}, 100)
	if err != nil {
		return nil, err
	}
	pkgs := ParseDNFCheckUpdate(string(res.Stdout))
	if len(pkgs) == 0 {
		return pkgs, nil
	}

	// Installed [epoch:]version-release and source RPM, keyed by name.arch.
	names := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		names = append(names, p.Name)
	}
	args := append([]string{"-q", "--qf", rpmQueryFormat, "--"}, names...)
	if res, err := d.r.Run(ctx, execx.Cmd{Name: "rpm", Args: args}); err == nil {
		installed := ParseRPMQuery(string(res.Stdout))
		for i := range pkgs {
			if inst, ok := installed[pkgs[i].Name+"."+pkgs[i].Arch]; ok {
				pkgs[i].Current, pkgs[i].Source = inst.version, inst.source
			}
		}
	}

	// Security advisories (best effort: needs updateinfo metadata in repos).
	secArgs := []string{"-q", "updateinfo", "list", "--security"}
	if d.bin == "yum" {
		secArgs = []string{"-q", "updateinfo", "list", "security"}
	}
	if res, err := d.r.Run(ctx, execx.Cmd{Name: d.bin, Args: secArgs, Timeout: refreshTimeout}); err == nil && res.ExitCode == 0 {
		adv := ParseDNFSecurity(string(res.Stdout))
		for i := range pkgs {
			if a, ok := adv[pkgs[i].Name+"."+pkgs[i].Arch]; ok {
				pkgs[i].Security, pkgs[i].Advisory = true, a
			}
		}
	}
	return pkgs, nil
}

func (d *DNF) UpgradeAll(ctx context.Context, exclude []string) (string, error) {
	args := []string{"-y", "upgrade"}
	for _, e := range exclude {
		args = append(args, "--exclude="+e)
	}
	res, err := run(ctx, d.r, execx.Cmd{Name: d.bin, Args: args, Timeout: upgradeTimeout})
	return res.Combined(), err
}

func (d *DNF) UpgradePackages(ctx context.Context, names []string, _ []protocol.Package) (string, error) {
	res, err := run(ctx, d.r, execx.Cmd{Name: d.bin, Args: append([]string{"-y", "upgrade", "--"}, names...), Timeout: upgradeTimeout})
	return res.Combined(), err
}

func (d *DNF) RebootRequired(ctx context.Context) bool {
	name, args := d.bin, []string{"needs-restarting", "-r"}
	if d.bin == "yum" {
		name, args = "needs-restarting", []string{"-r"}
	}
	if res, err := d.r.Run(ctx, execx.Cmd{Name: name, Args: args}); err == nil {
		switch res.ExitCode {
		case 1:
			return true
		case 0:
			return false
		}
	}
	return kernelUpgraded()
}

// rpmQueryFormat prints "name.arch [epoch:]version-release source-rpm".
// The epoch conditional works on every supported rpm version.
const rpmQueryFormat = "%{NAME}.%{ARCH} %|EPOCH?{%{EPOCH}:}:{}|%{VERSION}-%{RELEASE} %{SOURCERPM}\n"

type rpmInstalled struct{ version, source string }

// ParseRPMQuery parses rpmQueryFormat output. The source name is derived
// from the SRPM file name (name-version-release.src.rpm).
func ParseRPMQuery(out string) map[string]rpmInstalled {
	m := map[string]rpmInstalled{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		inst := rpmInstalled{version: f[1]}
		if len(f) >= 3 {
			srpm := strings.TrimSuffix(strings.TrimSuffix(f[2], ".rpm"), ".src")
			if rest, _, ok := cutLast(srpm, "-"); ok {
				if name, _, ok := cutLast(rest, "-"); ok {
					inst.source = name
				}
			}
		}
		m[f[0]] = inst
	}
	return m
}

// ParseDNFCheckUpdate parses `dnf -q check-update`. Long names wrap onto the
// following line; the "Obsoleting Packages" section is ignored.
func ParseDNFCheckUpdate(out string) []protocol.Package {
	var pkgs []protocol.Package
	var carry string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "Obsoleting") || strings.HasPrefix(line, "Security:") {
			break
		}
		if line == "" || strings.HasPrefix(line, "Last metadata") {
			continue
		}
		f := strings.Fields(carry + " " + line)
		if len(f) == 1 {
			carry = f[0]
			continue
		}
		carry = ""
		if len(f) < 3 {
			continue
		}
		name, arch, ok := cutLast(f[0], ".")
		if !ok {
			continue
		}
		pkgs = append(pkgs, protocol.Package{Name: name, Arch: arch, Available: f[1], Repo: f[2]})
	}
	return pkgs
}

// ParseDNFSecurity maps name.arch → advisory ID from `updateinfo list`
// output (dnf4, dnf5 and yum layouts).
func ParseDNFSecurity(out string) map[string]string {
	adv := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 {
			continue
		}
		for _, field := range f[1:] {
			if name, arch, ok := parseNEVRA(field); ok {
				adv[name+"."+arch] = f[0]
				break
			}
		}
	}
	return adv
}

var rpmArches = map[string]bool{"x86_64": true, "aarch64": true, "noarch": true, "i686": true, "ppc64le": true, "s390x": true, "armv7hl": true}

// parseNEVRA splits name-[epoch:]version-release.arch.
func parseNEVRA(s string) (name, arch string, ok bool) {
	rest, arch, ok := cutLast(s, ".")
	if !ok || !rpmArches[arch] {
		return "", "", false
	}
	rest, _, ok = cutLast(rest, "-") // release
	if !ok {
		return "", "", false
	}
	name, _, ok = cutLast(rest, "-") // version
	return name, arch, ok && name != ""
}

func cutLast(s, sep string) (before, after string, ok bool) {
	i := strings.LastIndex(s, sep)
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}
