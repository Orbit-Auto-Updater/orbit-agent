package packages

import (
	"bufio"
	"context"
	"errors"
	"slices"
	"strings"

	"orbit-agent/internal/execx"
	"orbit-agent/internal/protocol"
)

// ------------------------------------------------------------------ Alpine

type Apk struct{ r execx.Runner }

func (a *Apk) Name() string { return "apk" }

func (a *Apk) Refresh(ctx context.Context) error {
	_, err := run(ctx, a.r, execx.Cmd{Name: "apk", Args: []string{"update", "-q"}, Timeout: refreshTimeout})
	return err
}

func (a *Apk) List(ctx context.Context) ([]protocol.Package, error) {
	res, err := run(ctx, a.r, execx.Cmd{Name: "apk", Args: []string{"list", "-u"}, Timeout: refreshTimeout})
	if err != nil {
		return nil, err
	}
	return ParseApkUpgradable(string(res.Stdout)), nil
}

func (a *Apk) UpgradeAll(ctx context.Context, _ []string) (string, error) {
	res, err := run(ctx, a.r, execx.Cmd{Name: "apk", Args: []string{"upgrade", "--no-progress"}, Timeout: upgradeTimeout})
	return res.Combined(), err
}

func (a *Apk) UpgradePackages(ctx context.Context, names []string, _ []protocol.Package) (string, error) {
	res, err := run(ctx, a.r, execx.Cmd{Name: "apk", Args: append([]string{"add", "--upgrade", "--no-progress", "--"}, names...), Timeout: upgradeTimeout})
	return res.Combined(), err
}

func (a *Apk) RebootRequired(context.Context) bool { return kernelUpgraded() }

// ParseApkUpgradable parses `apk list -u`, e.g.
//
//	busybox-1.36.1-r29 x86_64 {busybox} (GPL-2.0-only) [upgradable from: busybox-1.36.1-r28]
func ParseApkUpgradable(out string) []protocol.Package {
	var pkgs []protocol.Package
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		name, ver, ok := splitApk(f[0])
		if !ok {
			continue
		}
		p := protocol.Package{Name: name, Available: ver, Arch: f[1]}
		if len(f) > 2 && strings.HasPrefix(f[2], "{") && strings.HasSuffix(f[2], "}") {
			p.Source = strings.Trim(f[2], "{}") // origin package
		}
		if _, from, ok := strings.Cut(sc.Text(), "upgradable from: "); ok {
			_, p.Current, _ = splitApk(strings.TrimSuffix(strings.TrimSpace(from), "]"))
		}
		pkgs = append(pkgs, p)
	}
	return pkgs
}

// splitApk splits "name-1.2.3-r4" into name and "1.2.3-r4".
func splitApk(s string) (name, version string, ok bool) {
	rest, rel, ok := cutLast(s, "-")
	if !ok || !strings.HasPrefix(rel, "r") {
		return "", "", false
	}
	name, ver, ok := cutLast(rest, "-")
	if !ok {
		return "", "", false
	}
	return name, ver + "-" + rel, true
}

// -------------------------------------------------------------------- Arch

type Pacman struct{ r execx.Runner }

func (p *Pacman) Name() string { return "pacman" }

// Refresh is a no-op: checkupdates syncs into a private database so the
// system's sync DB is never left ahead of installed packages.
func (p *Pacman) Refresh(context.Context) error { return nil }

func (p *Pacman) List(ctx context.Context) ([]protocol.Package, error) {
	if !has(p.r, "checkupdates") {
		return nil, errors.New("checkupdates not found: install pacman-contrib")
	}
	// exit 2 means no updates.
	res, err := run(ctx, p.r, execx.Cmd{Name: "checkupdates", Timeout: refreshTimeout}, 2)
	if err != nil {
		return nil, err
	}
	return ParseCheckupdates(string(res.Stdout)), nil
}

func (p *Pacman) UpgradeAll(ctx context.Context, exclude []string) (string, error) {
	args := []string{"-Syu", "--noconfirm", "--noprogressbar"}
	if len(exclude) > 0 {
		args = append(args, "--ignore", strings.Join(exclude, ","))
	}
	res, err := run(ctx, p.r, execx.Cmd{Name: "pacman", Args: args, Timeout: upgradeTimeout})
	return res.Combined(), err
}

// UpgradePackages performs a full sync upgrade, ignoring everything pending
// that wasn't selected (Arch does not support targeted partial upgrades).
func (p *Pacman) UpgradePackages(ctx context.Context, names []string, pending []protocol.Package) (string, error) {
	var ignore []string
	for _, pk := range pending {
		if !slices.Contains(names, pk.Name) {
			ignore = append(ignore, pk.Name)
		}
	}
	return p.UpgradeAll(ctx, ignore)
}

func (p *Pacman) RebootRequired(context.Context) bool { return kernelUpgraded() }

// ParseCheckupdates parses lines like "linux 6.11.1.arch1-1 -> 6.11.2.arch1-1".
func ParseCheckupdates(out string) []protocol.Package {
	var pkgs []protocol.Package
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 4 && f[2] == "->" {
			pkgs = append(pkgs, protocol.Package{Name: f[0], Current: f[1], Available: f[3]})
		}
	}
	return pkgs
}
