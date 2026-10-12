package packages

import (
	"context"
	"slices"
	"strings"
	"testing"

	"linux-dash-agent/internal/execx"
	"linux-dash-agent/internal/protocol"
)

func TestParseApt(t *testing.T) {
	out := `Listing... Done
openssl/noble-updates,noble-security 3.0.13-0ubuntu3.4 amd64 [upgradable from: 3.0.13-0ubuntu3.3]
tzdata/noble-updates 2024b-0ubuntu0.24.04 all [upgradable from: 2024a-3ubuntu1.1]
libssl3t64/bookworm-security 3.0.15-1~deb12u1 arm64 [upgradable from: 3.0.14-1~deb12u2]
`
	pkgs := ParseAptUpgradable(out)
	if len(pkgs) != 3 {
		t.Fatalf("want 3, got %d: %+v", len(pkgs), pkgs)
	}
	o := pkgs[0]
	if o.Name != "openssl" || o.Available != "3.0.13-0ubuntu3.4" || o.Current != "3.0.13-0ubuntu3.3" || o.Arch != "amd64" || !o.Security {
		t.Fatalf("openssl = %+v", o)
	}
	if pkgs[1].Security {
		t.Fatal("tzdata from -updates is not a security update")
	}
	if !pkgs[2].Security {
		t.Fatal("debian -security suite should be flagged")
	}
}

func TestParseDNF(t *testing.T) {
	out := `
openssl.x86_64                       1:3.0.7-28.el9_4          baseos
kernel-core.x86_64                   5.14.0-503.11.1.el9_5     baseos
python3-some-really-long-package-name.noarch
                                     2.1-3.el9                 appstream
Obsoleting Packages
grub2-tools.x86_64                   1:2.06-80.el9             baseos
`
	pkgs := ParseDNFCheckUpdate(out)
	if len(pkgs) != 3 {
		t.Fatalf("want 3, got %d: %+v", len(pkgs), pkgs)
	}
	if pkgs[0].Name != "openssl" || pkgs[0].Arch != "x86_64" || pkgs[0].Available != "1:3.0.7-28.el9_4" || pkgs[0].Repo != "baseos" {
		t.Fatalf("%+v", pkgs[0])
	}
	if pkgs[2].Name != "python3-some-really-long-package-name" || pkgs[2].Repo != "appstream" {
		t.Fatalf("wrapped line: %+v", pkgs[2])
	}

	sec := ParseDNFSecurity(`RHSA-2024:7848 Important/Sec. openssl-1:3.0.7-28.el9_4.x86_64
RHSA-2024:8000 Moderate/Sec.  kernel-core-5.14.0-503.11.1.el9_5.x86_64
FEDORA-2024-1a2b3c4d5e security  Moderate  curl-8.6.0-10.fc40.aarch64  2024-10-01 12:00:00
`)
	if sec["openssl.x86_64"] != "RHSA-2024:7848" || sec["kernel-core.x86_64"] != "RHSA-2024:8000" || sec["curl.aarch64"] != "FEDORA-2024-1a2b3c4d5e" {
		t.Fatalf("%v", sec)
	}
}

func TestSourceNames(t *testing.T) {
	src := ParseDpkgSources("libssl3t64\topenssl\nopenssl\topenssl\nlibc6:amd64\tglibc\n")
	if src["libssl3t64"] != "openssl" || src["libc6"] != "glibc" {
		t.Fatalf("dpkg sources: %v", src)
	}
	rpm := ParseRPMQuery("openssl.x86_64 1:3.0.7-27.el9 openssl-3.0.7-27.el9.src.rpm\nkernel-core.x86_64 5.14.0-427.el9 kernel-5.14.0-427.el9.src.rpm\n")
	if r := rpm["openssl.x86_64"]; r.version != "1:3.0.7-27.el9" || r.source != "openssl" {
		t.Fatalf("rpm openssl: %+v", r)
	}
	if rpm["kernel-core.x86_64"].source != "kernel" {
		t.Fatalf("rpm kernel: %+v", rpm["kernel-core.x86_64"])
	}
}

func TestParseApkAndPacman(t *testing.T) {
	apk := ParseApkUpgradable("busybox-1.36.1-r29 x86_64 {busybox} (GPL-2.0-only) [upgradable from: busybox-1.36.1-r28]\nlibcrypto3-3.3.2-r1 aarch64 {openssl} (Apache-2.0) [upgradable from: libcrypto3-3.3.2-r0]\n")
	if len(apk) != 2 || apk[0].Name != "busybox" || apk[0].Available != "1.36.1-r29" || apk[0].Current != "1.36.1-r28" || apk[1].Name != "libcrypto3" || apk[1].Source != "openssl" {
		t.Fatalf("%+v", apk)
	}
	pac := ParseCheckupdates("linux 6.11.1.arch1-1 -> 6.11.2.arch1-1\nopenssl 3.3.2-1 -> 3.4.0-1\n")
	if len(pac) != 2 || pac[0].Name != "linux" || pac[0].Current != "6.11.1.arch1-1" || pac[1].Available != "3.4.0-1" {
		t.Fatalf("%+v", pac)
	}
}

func TestSelect(t *testing.T) {
	pending := []protocol.Package{
		{Name: "openssl", Security: true}, {Name: "curl", Security: true}, {Name: "tzdata"}, {Name: "docker-ce"},
	}
	_, full, _ := Select(pending, UpgradeOptions{})
	if !full {
		t.Fatal("no options should be a full upgrade")
	}
	got, _, _ := Select(pending, UpgradeOptions{SecurityOnly: true, Exclude: []string{"curl"}})
	if !slices.Equal(got, []string{"openssl"}) {
		t.Fatalf("security minus excluded: %v", got)
	}
	got, _, _ = Select(pending, UpgradeOptions{Packages: []string{"tzdata", "not-pending"}})
	if !slices.Equal(got, []string{"tzdata"}) {
		t.Fatalf("explicit list should intersect pending: %v", got)
	}
	if _, _, err := Select(pending, UpgradeOptions{Packages: []string{"--allow-downgrades"}}); err == nil {
		t.Fatal("flag-like package name must be rejected")
	}
	if _, _, err := Select(pending, UpgradeOptions{Exclude: []string{"x; reboot"}}); err == nil {
		t.Fatal("bad exclude must be rejected")
	}
}

func TestAptApplyCommands(t *testing.T) {
	f := &execx.Fake{Responses: map[string]execx.Result{}}
	opts := "apt-get -y -q -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -o DPkg::Lock::Timeout=300"
	f.Responses[opts+" install --only-upgrade -- openssl curl"] = execx.Result{Stdout: []byte("ok")}
	f.Responses[opts+" upgrade --with-new-pkgs"] = execx.Result{ExitCode: 100, Stderr: []byte("E: Could not get lock")}

	a := &Apt{r: f}
	pending := []protocol.Package{{Name: "openssl", Security: true}, {Name: "curl", Security: true}, {Name: "tzdata"}}
	applied, _, err := Apply(context.Background(), a, pending, UpgradeOptions{SecurityOnly: true})
	if err != nil || !slices.Equal(applied, []string{"openssl", "curl"}) {
		t.Fatalf("applied=%v err=%v", applied, err)
	}
	_, _, err = Apply(context.Background(), a, pending, UpgradeOptions{})
	if err == nil || !strings.Contains(err.Error(), "Could not get lock") {
		t.Fatalf("expected lock error, got %v", err)
	}
}

func TestPacmanSelectiveUsesIgnore(t *testing.T) {
	f := &execx.Fake{Responses: map[string]execx.Result{"pacman -Syu --noconfirm --noprogressbar --ignore linux,docker": {}}}
	p := &Pacman{r: f}
	pending := []protocol.Package{{Name: "openssl"}, {Name: "linux"}, {Name: "docker"}}
	if _, _, err := Apply(context.Background(), p, pending, UpgradeOptions{Packages: []string{"openssl"}}); err != nil {
		t.Fatal(err)
	}
}

func TestDetect(t *testing.T) {
	m, err := Detect(&execx.Fake{Binaries: map[string]bool{"dnf": true, "yum": true}})
	if err != nil || m.Name() != "dnf" {
		t.Fatalf("%v %v", m, err)
	}
	if _, err := Detect(&execx.Fake{}); err != ErrNoManager {
		t.Fatalf("want ErrNoManager, got %v", err)
	}
}

func TestParseInstalled(t *testing.T) {
	cases := []struct{ mgr, line, name, version string }{
		{"apt", "Setting up openssl (3.0.13-0ubuntu3.4) ...", "openssl", "3.0.13-0ubuntu3.4"},
		{"apt", "Setting up libc6:amd64 (2.39-0ubuntu8.3) ...", "libc6", "2.39-0ubuntu8.3"},
		{"dnf", "  Upgrading        : openssl-libs-1:3.0.7-27.el9.x86_64        1/8 ", "openssl-libs", "1:3.0.7-27.el9"},
		{"dnf", "[3/8] Upgrading curl-8.6.0-10.fc40.aarch64 100% |  12.3 MiB/s", "curl", "8.6.0-10.fc40"},
		{"yum", "  Installing       : kernel-core-5.14.0-427.42.1.el9_4.x86_64     2/8", "kernel-core", "5.14.0-427.42.1.el9_4"},
		{"apk", "(1/3) Upgrading busybox (1.36.1-r29 -> 1.36.1-r31)", "busybox", "1.36.1-r31"},
		{"pacman", "(1/3) upgrading linux                       [######]", "linux", ""},
	}
	for _, c := range cases {
		got, ok := ParseInstalled(c.mgr, c.line)
		if !ok || got.Name != c.name || got.Version != c.version {
			t.Errorf("%s %q: got %+v ok=%v", c.mgr, c.line, got, ok)
		}
	}
	for _, line := range []string{"Unpacking openssl (3.0.13-0ubuntu3.4) over (3.0.13-0ubuntu3.3) ...", "  Cleanup          : openssl-libs-1:3.0.7-25.el9.x86_64   5/8", "Reading package lists..."} {
		if _, ok := ParseInstalled("apt", line); ok {
			t.Errorf("%q should not count as installed", line)
		}
		if _, ok := ParseInstalled("dnf", line); ok {
			t.Errorf("%q should not count as installed (dnf)", line)
		}
	}
}
