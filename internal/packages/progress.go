package packages

import (
	"regexp"
	"strings"
)

// Installed is one package the package manager reported as installed or
// upgraded while an update runs.
type Installed struct {
	Name    string
	Version string
}

var (
	// apt/dpkg: "Setting up openssl (3.0.13-0ubuntu3.4) ..."
	aptSetup = regexp.MustCompile(`^Setting up ([^\s:]+)(?::\S+)? \(([^)]+)\)`)
	// dnf4/yum: "  Upgrading        : openssl-libs-1:3.0.7-27.el9.x86_64        1/8"
	dnf4Step = regexp.MustCompile(`^\s*(?:Upgrading|Installing|Reinstalling|Downgrading)\s*:\s*(\S+)\s+\d+/\d+`)
	// dnf5: "[3/8] Upgrading openssl-libs-1:3.0.7-27.el9.x86_64 100% |..."
	dnf5Step = regexp.MustCompile(`^\[\s*\d+/\d+\]\s+(?:Upgrading|Installing|Reinstalling|Downgrading)\s+(\S+)`)
	// apk: "(1/3) Upgrading busybox (1.36.1-r29 -> 1.36.1-r31)"
	apkStep = regexp.MustCompile(`^\(\s*\d+/\d+\)\s+(?:Upgrading|Installing|Replacing)\s+(\S+)\s+\(([^)]+)\)`)
	// pacman: "(1/3) upgrading linux                       [####]"
	pacmanStep = regexp.MustCompile(`^\(\s*\d+/\d+\)\s+(?:upgrading|installing|reinstalling)\s+(\S+)`)
)

// ParseInstalled recognises a "package installed" line in a manager's output.
func ParseInstalled(manager, line string) (Installed, bool) {
	switch manager {
	case "apt":
		if m := aptSetup.FindStringSubmatch(line); m != nil {
			return Installed{Name: m[1], Version: m[2]}, true
		}
	case "dnf", "yum":
		m := dnf4Step.FindStringSubmatch(line)
		if m == nil {
			m = dnf5Step.FindStringSubmatch(line)
		}
		if m != nil {
			if name, version, ok := splitNEVRA(m[1]); ok {
				return Installed{Name: name, Version: version}, true
			}
		}
	case "apk":
		if m := apkStep.FindStringSubmatch(line); m != nil {
			v := m[2]
			if _, after, ok := strings.Cut(v, "-> "); ok {
				v = after
			}
			return Installed{Name: m[1], Version: v}, true
		}
	case "pacman":
		if m := pacmanStep.FindStringSubmatch(line); m != nil {
			return Installed{Name: m[1]}, true
		}
	}
	return Installed{}, false
}

// splitNEVRA turns "openssl-libs-1:3.0.7-27.el9.x86_64" into name and
// "1:3.0.7-27.el9".
func splitNEVRA(s string) (name, version string, ok bool) {
	rest := s
	if r, arch, ok := cutLast(s, "."); ok && rpmArches[arch] {
		rest = r
	}
	nv, rel, ok := cutLast(rest, "-")
	if !ok {
		return "", "", false
	}
	name, ver, ok := cutLast(nv, "-")
	if !ok {
		return "", "", false
	}
	return name, ver + "-" + rel, true
}
