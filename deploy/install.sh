#!/bin/sh
# Linux Dash agent installer.
#
# Interactive (asks for the dashboard address, token and options):
#   curl -q -fsSL https://dash.example.com/install.sh | sudo sh
#   sudo sh install.sh
#
# Unattended (every question has a flag; --yes accepts defaults for the rest):
#   sudo sh install.sh --yes --server 10.0.0.5:3000 --token <TOKEN> --allow-http
#
# Preview the generated config without installing anything:
#   sh install.sh --dry-run
#
# Run with --help for all options.
set -eu

BIN_PATH=/usr/local/bin/linux-dash-agent
CONF_DIR=/etc/linux-dash-agent
CONF_FILE=$CONF_DIR/config.yaml

# Answers. "" means "not given yet"; flags fill these in and skip the prompt.
SERVER=${LINUX_DASH_SERVER:-}
TOKEN=${LINUX_DASH_TOKEN:-}
CA_FILE=""
INSECURE_TLS=""
ALLOW_HTTP=""
BINARY=""
DOWNLOAD_URL=""
ALLOW_REBOOT=""
WATCH=""
WATCH_SET=0
PM2_USERS=""
PM2_SET=0
AUTO_UPDATES=""
UPDATE_DAYS=""
UPDATE_TIME=""
UPDATE_TZ=""
AUTO_REBOOT=""
ASSUME_YES=0
DRY_RUN=0
FORCE_CONFIG=0
KEEP_CONFIG=0

usage() {
	cat <<'EOF'
Linux Dash agent installer

Usage: install.sh [options]

Connection
  --server ADDR          Dashboard address: IP, hostname or URL (e.g. 10.0.0.5:3000,
                         dash.example.com, https://dash.example.com)
  --token TOKEN          Enrolment token from the Agents page
  --ca-file PATH         CA certificate that signed the dashboard's TLS certificate
  --insecure-tls         Don't verify the dashboard's TLS certificate (not recommended)
  --allow-http           Permit an unencrypted http:// dashboard address

Agent binary
  --binary PATH          Use a local agent binary instead of downloading one
  --download-url URL     Download the binary from this URL

What the dashboard may do on this server
  --allow-reboot         Allow remote reboots (off by default)
  --watch LIST           systemd units to monitor, comma-separated, or "none"
  --pm2-users LIST       Users whose PM2 processes to monitor, comma-separated, or "none"

Automatic updates
  --auto-updates MODE    off | security | all
  --update-days LIST     e.g. "sun", "mon,thu" or "daily"
  --update-time HH:MM    Start of the maintenance window
  --update-timezone TZ   e.g. UTC, Europe/London (default: this server's timezone)
  --auto-reboot          Reboot after updates when required (needs --allow-reboot)

General
  -y, --yes              Don't ask; use defaults for anything not given
  --dry-run              Print the config that would be written; change nothing
  --force-config         Replace an existing config instead of keeping it
  -h, --help             Show this help
EOF
}

# ------------------------------------------------------------------ output

if [ -t 2 ]; then
	B=$(printf '\033[1m'); D=$(printf '\033[2m'); P=$(printf '\033[35m'); G=$(printf '\033[32m')
	Y=$(printf '\033[33m'); R=$(printf '\033[31m'); N=$(printf '\033[0m')
else
	B="" D="" P="" G="" Y="" R="" N=""
fi
say()  { printf '%s\n' "$*" >&2; }
info() { printf '%s==>%s %s\n' "$P" "$N" "$*" >&2; }
ok()   { printf '%s ✓ %s %s\n' "$G" "$N" "$*" >&2; }
warn() { printf '%s ! %s %s\n' "$Y" "$N" "$*" >&2; }
die()  { printf '%serror:%s %s\n' "$R" "$N" "$*" >&2; exit 1; }
section() { printf '\n%s%s%s\n' "$B" "$*" "$N" >&2; }

matches() { printf '%s\n' "$1" | grep -Eq "$2"; }
lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }
trim() { printf '%s' "$1" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//'; }

# ---------------------------------------------------------------- arguments

need_arg() { [ $# -ge 2 ] && [ -n "$2" ] || die "$1 needs a value"; }
while [ $# -gt 0 ]; do
	case "$1" in
		--server) need_arg "$@"; SERVER=$2; shift 2 ;;
		--token) need_arg "$@"; TOKEN=$2; shift 2 ;;
		--ca-file) need_arg "$@"; CA_FILE=$2; shift 2 ;;
		--insecure-tls) INSECURE_TLS=yes; shift ;;
		--allow-http) ALLOW_HTTP=yes; shift ;;
		--binary) need_arg "$@"; BINARY=$2; shift 2 ;;
		--download-url) need_arg "$@"; DOWNLOAD_URL=$2; shift 2 ;;
		--allow-reboot) ALLOW_REBOOT=yes; shift ;;
		--watch) need_arg "$@"; WATCH=$2; WATCH_SET=1; shift 2 ;;
		--pm2-users) need_arg "$@"; PM2_USERS=$2; PM2_SET=1; shift 2 ;;
		--auto-updates) need_arg "$@"; AUTO_UPDATES=$(lower "$2"); shift 2 ;;
		--update-days) need_arg "$@"; UPDATE_DAYS=$2; shift 2 ;;
		--update-time) need_arg "$@"; UPDATE_TIME=$2; shift 2 ;;
		--update-timezone) need_arg "$@"; UPDATE_TZ=$2; shift 2 ;;
		--auto-reboot) AUTO_REBOOT=yes; shift ;;
		-y|--yes) ASSUME_YES=1; shift ;;
		--dry-run) DRY_RUN=1; shift ;;
		--force-config) FORCE_CONFIG=1; shift ;;
		-h|--help) usage; exit 0 ;;
		*) die "unknown option: $1 (see --help)" ;;
	esac
done

# ------------------------------------------------------------------ prompts

# Read answers from the terminal even when the script itself arrives on
# stdin (curl … | sh). Without a terminal, run non-interactively.
INTERACTIVE=0
if [ "$ASSUME_YES" -eq 0 ]; then
	if [ -t 0 ]; then
		exec 3<&0; INTERACTIVE=1
	elif { exec 3</dev/tty; } 2>/dev/null; then
		INTERACTIVE=1
	fi
fi

TMP=""
cleanup() {
	[ -n "$TMP" ] && rm -f "$TMP"
	[ "$INTERACTIVE" -eq 1 ] && stty echo <&3 2>/dev/null
	return 0
}
trap cleanup EXIT
trap 'say ""; exit 130' INT TERM

# ask VAR "Question" [default]
ask() {
	_def=${3:-}
	if [ "$INTERACTIVE" -eq 0 ]; then
		eval "$1=\$_def"
		return
	fi
	if [ -n "$_def" ]; then
		printf '%s?%s %s %s[%s]%s ' "$P" "$N" "$2" "$D" "$_def" "$N" >&2
	else
		printf '%s?%s %s ' "$P" "$N" "$2" >&2
	fi
	IFS= read -r _ans <&3 || die "no answer (input closed)"
	_ans=$(trim "$_ans")
	[ -n "$_ans" ] || _ans=$_def
	eval "$1=\$_ans"
}

# ask_secret VAR "Question" — input is not echoed
ask_secret() {
	[ "$INTERACTIVE" -eq 1 ] || { eval "$1=''"; return; }
	printf '%s?%s %s ' "$P" "$N" "$2" >&2
	_restore=$(stty -g <&3 2>/dev/null || true)
	stty -echo <&3 2>/dev/null || true
	IFS= read -r _ans <&3 || _ans=""
	[ -n "$_restore" ] && stty "$_restore" <&3 2>/dev/null || true
	printf '\n' >&2
	_ans=$(trim "$_ans")
	eval "$1=\$_ans"
}

# ask_yn VAR "Question" y|n — sets VAR to yes/no
ask_yn() {
	_d=$3
	_hint="y/N"; [ "$_d" = y ] && _hint="Y/n"
	while :; do
		ask _yn "$2 ($_hint)" ""
		_yn=$(lower "${_yn:-$_d}")
		case "$_yn" in
			y|yes) eval "$1=yes"; return ;;
			n|no) eval "$1=no"; return ;;
		esac
		say "  Please answer y or n."
	done
}

# ---------------------------------------------------------------- validation

TOKEN_RE='^[A-Za-z0-9._~-]{8,255}$'
HOSTPORT_RE='^([A-Za-z0-9.-]+|\[[0-9A-Fa-f:]+\])(:[0-9]{1,5})?(/[A-Za-z0-9._~/-]*)?$'
URL_RE='^https?://([A-Za-z0-9.-]+|\[[0-9A-Fa-f:]+\])(:[0-9]{1,5})?(/[A-Za-z0-9._~/-]*)?$'
UNIT_RE='^[A-Za-z0-9][A-Za-z0-9_.@:-]{0,250}$'
USER_RE='^[a-z_][a-z0-9_-]{0,31}$'
TIME_RE='^([01][0-9]|2[0-3]):[0-5][0-9]$'
DAY_RE='^(sun|mon|tue|wed|thu|fri|sat)[a-z]*$'

# normalize_server ADDR — turn "10.0.0.5:3000" or a URL into a base URL.
# Prints nothing if the address is invalid. May ask about HTTPS.
normalize_server() {
	_s=$(trim "$1")
	_s=${_s%/}
	case "$_s" in
		http://*|https://*) ;;
		*)
			matches "$_s" "$HOSTPORT_RE" || return 0
			if [ "$ALLOW_HTTP" = yes ]; then
				_s="http://$_s"
			elif [ "$INTERACTIVE" -eq 1 ]; then
				ask_yn _https "Does the dashboard use HTTPS?" y
				if [ "$_https" = yes ]; then _s="https://$_s"; else _s="http://$_s"; fi
			else
				_s="https://$_s"
			fi
			;;
	esac
	if matches "$_s" "$URL_RE"; then printf '%s' "$_s"; fi
}

is_local_url() {
	case "$1" in
		http://localhost*|http://127.*|http://\[::1\]*) return 0 ;;
	esac
	return 1
}

# split_list "a, b,c" → one item per line
split_list() { printf '%s\n' "$1" | tr ',' '\n' | sed 's/^[[:space:]]*//; s/[[:space:]]*$//' | grep -v '^$' || true; }

# yaml_list "a,b" → ["a", "b"]  (items are pre-validated)
yaml_list() {
	_out=""
	for _i in $(split_list "$1"); do
		_out="$_out${_out:+, }\"$_i\""
	done
	printf '[%s]' "$_out"
}

# ---------------------------------------------------------------- detection

detect_arch() {
	case "$(uname -m)" in
		x86_64|amd64) echo amd64 ;;
		aarch64|arm64) echo arm64 ;;
		armv7l|armv6l) echo arm ;;
		i386|i686) echo 386 ;;
		*) echo "" ;;
	esac
}

has_systemd() { command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; }

# Enabled services that are usually worth watching.
detect_units() {
	has_systemd || return 0
	systemctl list-unit-files --type=service --state=enabled --no-legend 2>/dev/null |
		awk '{print $1}' |
		grep -E '^(nginx|apache2|httpd|caddy|haproxy|traefik|postgresql([@-][0-9.]+)?|mysql|mysqld|mariadb|mongod|redis(-server)?|valkey|memcached|rabbitmq-server|elasticsearch|docker|containerd|php[0-9.]*-fpm|pm2-[a-z_][a-z0-9_-]*|gunicorn|uwsgi|supervisor)(@[^.]*)?\.service$' |
		tr '\n' ',' | sed 's/,$//; s/,/, /g'
}

# Users that have a PM2 home directory.
detect_pm2_users() {
	_found=""
	for _home in /root /home/*; do
		[ -d "$_home/.pm2" ] || continue
		_u=$(basename "$_home")
		matches "$_u" "$USER_RE" || continue
		_found="$_found${_found:+, }$_u"
	done
	printf '%s' "$_found"
}

# Best guess at a user's pm2 binary (nvm installs aren't on root's PATH).
pm2_binary_for() {
	_home=/home/$1
	[ "$1" = root ] && _home=/root
	_bin=$(ls -1d "$_home"/.nvm/versions/node/*/bin/pm2 2>/dev/null | tail -n 1 || true)
	[ -n "$_bin" ] || { [ -x "$_home/.npm-global/bin/pm2" ] && _bin="$_home/.npm-global/bin/pm2"; } || true
	[ -n "$_bin" ] || _bin=$(command -v pm2 2>/dev/null || true)
	printf '%s' "${_bin:-pm2}"
}

detect_timezone() {
	_tz=""
	if command -v timedatectl >/dev/null 2>&1; then
		_tz=$(timedatectl show -p Timezone --value 2>/dev/null || true)
	fi
	if [ -z "$_tz" ] && [ -f /etc/timezone ]; then
		_tz=$(head -n 1 /etc/timezone)
	fi
	if [ -z "$_tz" ] && [ -L /etc/localtime ]; then
		_tz=$(readlink /etc/localtime | sed 's|.*/zoneinfo/||')
	fi
	matches "${_tz:-x}" '^[A-Za-z0-9_+/-]+$' || _tz=UTC
	printf '%s' "${_tz:-UTC}"
}

# probe URL — 0 reachable, 1 unreachable, 2 TLS not trusted, 3 can't check.
probe() {
	command -v curl >/dev/null 2>&1 || return 3
	# -q (first): ignore ~/.curlrc so a user's settings can't weaken TLS checks.
	_opts="-sS -o /dev/null --max-time 10"
	if [ -n "$CA_FILE" ]; then
		# shellcheck disable=SC2086
		curl -q $_opts --cacert "$CA_FILE" "$1/" 2>/dev/null && return 0
	elif [ "$INSECURE_TLS" = yes ]; then
		# shellcheck disable=SC2086
		curl -q $_opts -k "$1/" 2>/dev/null && return 0
	else
		# shellcheck disable=SC2086
		curl -q $_opts "$1/" 2>/dev/null && return 0
	fi
	case $? in
		35|51|58|60|77|83) return 2 ;;
	esac
	return 1
}

# ---------------------------------------------------------------- preflight

ARCH=$(detect_arch)
if [ "$DRY_RUN" -eq 0 ]; then
	[ "$(uname -s)" = Linux ] || die "the agent only runs on Linux (use --dry-run to preview the config)"
	[ "$(id -u)" -eq 0 ] || die "run as root, e.g. with sudo (the agent manages packages and services)"
	[ -n "$ARCH" ] || die "unsupported CPU architecture: $(uname -m)"
fi
[ -n "$ARCH" ] || ARCH=amd64

say ""
say "${B}Linux Dash agent installer${N}"
[ "$DRY_RUN" -eq 1 ] && say "${D}Dry run: nothing will be installed or changed.${N}"
if [ "$INTERACTIVE" -eq 0 ] && [ "$ASSUME_YES" -eq 0 ]; then
	say "${D}No terminal available; continuing with flags and defaults.${N}"
fi

if [ -f "$CONF_FILE" ] && [ "$FORCE_CONFIG" -eq 0 ] && [ "$DRY_RUN" -eq 0 ]; then
	if [ "$INTERACTIVE" -eq 1 ]; then
		ask_yn _keep "An existing configuration was found at $CONF_FILE. Keep it and only update the agent?" y
		[ "$_keep" = yes ] && KEEP_CONFIG=1
	else
		KEEP_CONFIG=1
	fi
fi

# ------------------------------------------------------------- 1. dashboard

if [ "$KEEP_CONFIG" -eq 0 ]; then
	section "1. Dashboard connection"
	say "${D}The agent connects out to the dashboard; no inbound ports are needed on this server.${N}"

	while :; do
		if [ -z "$SERVER" ]; then
			[ "$INTERACTIVE" -eq 1 ] || die "--server is required (dashboard IP, hostname or URL)"
			ask SERVER "Dashboard address (IP or hostname, with :port if not 443, or a full URL):"
			[ -n "$SERVER" ] || { say "  An address is required, e.g. 10.0.0.5:3000 or https://dash.example.com"; continue; }
		fi
		_url=$(normalize_server "$SERVER")
		if [ -z "$_url" ]; then
			[ "$INTERACTIVE" -eq 1 ] || die "invalid --server value: $SERVER"
			say "  '$SERVER' doesn't look like an address. Examples: 10.0.0.5, 10.0.0.5:3000, dash.example.com, https://dash.example.com"
			SERVER=""
			continue
		fi
		SERVER=$_url
		break
	done

	case "$SERVER" in
		http://*)
			if ! is_local_url "$SERVER" && [ "$ALLOW_HTTP" != yes ]; then
				warn "Over plain HTTP the enrolment token, credentials and server data travel unencrypted."
				if [ "$INTERACTIVE" -eq 1 ]; then
					ask_yn ALLOW_HTTP "Use unencrypted HTTP anyway (only on a trusted private network)?" n
					[ "$ALLOW_HTTP" = yes ] || die "aborted; re-run with an https:// address"
				else
					die "refusing http:// without --allow-http"
				fi
			fi
			ALLOW_HTTP=yes
			;;
	esac

	if [ -n "$CA_FILE" ]; then
		[ -r "$CA_FILE" ] || die "CA file not readable: $CA_FILE"
	fi

	info "Checking that $SERVER is reachable…"
	set +e; probe "$SERVER"; _p=$?; set -e
	if [ "$_p" -eq 2 ]; then
		warn "The dashboard's TLS certificate isn't trusted by this server (self-signed or private CA?)."
		if [ "$INTERACTIVE" -eq 1 ]; then
			say "  1) Provide the CA certificate file (recommended)"
			say "  2) Skip certificate verification (traffic is encrypted but the dashboard isn't authenticated)"
			say "  3) Abort"
			while :; do
				ask _choice "Choose 1, 2 or 3:" 1
				case "$_choice" in
					1)
						ask CA_FILE "Path to the CA certificate (PEM):"
						if [ -r "$CA_FILE" ]; then break; fi
						say "  Can't read '$CA_FILE'."; CA_FILE="" ;;
					2) INSECURE_TLS=yes; break ;;
					3) die "aborted" ;;
				esac
			done
			set +e; probe "$SERVER"; _p=$?; set -e
		else
			die "untrusted TLS certificate; pass --ca-file PATH (or --insecure-tls)"
		fi
	fi
	case "$_p" in
		0) ok "Dashboard reachable" ;;
		3) warn "curl isn't installed, so reachability wasn't checked" ;;
		2) die "TLS verification still fails with $CA_FILE" ;;
		*)
			warn "Couldn't reach $SERVER from this server."
			if [ "$INTERACTIVE" -eq 1 ]; then
				ask_yn _cont "Continue anyway? The agent keeps retrying until the dashboard is reachable." n
				[ "$_cont" = yes ] || die "aborted; check the address, port and firewall"
			fi
			;;
	esac

	while :; do
		if [ -z "$TOKEN" ]; then
			[ "$INTERACTIVE" -eq 1 ] || die "--token is required (the Agents page in the dashboard)"
			ask_secret TOKEN "Enrolment token (from the Agents page; input hidden):"
		fi
		if matches "$TOKEN" "$TOKEN_RE"; then break; fi
		[ "$INTERACTIVE" -eq 1 ] || die "invalid --token"
		say "  That doesn't look like an enrolment token (8+ characters: letters, digits, . _ ~ -)."
		TOKEN=""
	done
	ok "Token accepted"

	# ------------------------------------------------------- 2. permissions

	section "2. What the dashboard may do on this server"
	say "${D}Container, PM2 and service restarts and package updates are allowed. Remote reboots are optional.${N}"
	[ -n "$ALLOW_REBOOT" ] || ask_yn ALLOW_REBOOT "Allow the dashboard to reboot this server?" n

	# ------------------------------------------------------- 3. monitoring

	section "3. What to monitor"
	if [ "$WATCH_SET" -eq 0 ]; then
		_detected=$(detect_units)
		if has_systemd; then
			[ -n "$_detected" ] && say "${D}Detected services: $_detected${N}"
			say "${D}Failed services are always reported, whether or not they're watched.${N}"
			ask WATCH "systemd services to watch (comma-separated, or 'none'):" "${_detected:-none}"
		else
			WATCH=none
		fi
	fi
	[ "$(lower "$WATCH")" = none ] && WATCH=""
	_units=""
	for _u in $(split_list "$WATCH"); do
		case "$_u" in *.*) ;; *) _u="$_u.service" ;; esac
		matches "$_u" "$UNIT_RE" || die "invalid unit name: $_u"
		_units="$_units${_units:+,}$_u"
	done
	WATCH=$_units

	if [ "$PM2_SET" -eq 0 ]; then
		_detected=$(detect_pm2_users)
		if [ -n "$_detected" ]; then
			say "${D}PM2 found for: $_detected${N}"
			ask PM2_USERS "Monitor PM2 processes for which users? (comma-separated, or 'none'):" "$_detected"
		else
			ask PM2_USERS "Users running PM2 apps to monitor (comma-separated, or 'none'):" "none"
		fi
	fi
	[ "$(lower "$PM2_USERS")" = none ] && PM2_USERS=""
	for _u in $(split_list "$PM2_USERS"); do
		matches "$_u" "$USER_RE" || die "invalid user name: $_u"
	done

	# --------------------------------------------------- 4. auto updates

	section "4. Automatic updates"
	say "${D}You can also change this later from the dashboard's Updates page.${N}"
	if [ -z "$AUTO_UPDATES" ]; then
		if [ "$INTERACTIVE" -eq 1 ]; then
			say "  1) Off: updates only run when triggered from the dashboard"
			say "  2) Security updates only (recommended)"
			say "  3) All updates"
			while :; do
				ask _c "Choose 1, 2 or 3:" 1
				case "$_c" in 1) AUTO_UPDATES=off; break ;; 2) AUTO_UPDATES=security; break ;; 3) AUTO_UPDATES=all; break ;; esac
			done
		else
			AUTO_UPDATES=off
		fi
	fi
	case "$AUTO_UPDATES" in off|security|all) ;; *) die "--auto-updates must be off, security or all" ;; esac

	if [ "$AUTO_UPDATES" != off ]; then
		while :; do
			[ -n "$UPDATE_DAYS" ] || ask UPDATE_DAYS "Which days? (e.g. sun, mon,thu, or daily):" "sun"
			UPDATE_DAYS=$(lower "$UPDATE_DAYS")
			[ "$UPDATE_DAYS" = daily ] && break
			_bad=""
			for _d in $(split_list "$UPDATE_DAYS"); do matches "$_d" "$DAY_RE" || _bad=$_d; done
			[ -z "$_bad" ] && break
			[ "$INTERACTIVE" -eq 1 ] || die "invalid --update-days: $_bad"
			say "  Unknown day '$_bad'. Use sun, mon, tue, wed, thu, fri, sat or daily."
			UPDATE_DAYS=""
		done
		while :; do
			[ -n "$UPDATE_TIME" ] || ask UPDATE_TIME "Start time (24h HH:MM):" "02:00"
			matches "$UPDATE_TIME" "$TIME_RE" && break
			[ "$INTERACTIVE" -eq 1 ] || die "invalid --update-time: $UPDATE_TIME"
			say "  Use 24-hour HH:MM, e.g. 02:00 or 23:30."
			UPDATE_TIME=""
		done
		while :; do
			[ -n "$UPDATE_TZ" ] || ask UPDATE_TZ "Timezone:" "$(detect_timezone)"
			if matches "$UPDATE_TZ" '^[A-Za-z0-9_+/-]+$' && { [ "$UPDATE_TZ" = UTC ] || [ ! -d /usr/share/zoneinfo ] || [ -e "/usr/share/zoneinfo/$UPDATE_TZ" ]; }; then
				break
			fi
			[ "$INTERACTIVE" -eq 1 ] || die "unknown --update-timezone: $UPDATE_TZ"
			say "  Unknown timezone '$UPDATE_TZ'. Examples: UTC, Europe/London, America/New_York."
			UPDATE_TZ=""
		done
		if [ "$ALLOW_REBOOT" = yes ]; then
			[ -n "$AUTO_REBOOT" ] || ask_yn AUTO_REBOOT "Reboot automatically after updates when a reboot is required?" n
		else
			[ "$AUTO_REBOOT" = yes ] && warn "--auto-reboot ignored because remote reboots are not allowed"
			AUTO_REBOOT=no
		fi
	fi
	[ -n "$AUTO_REBOOT" ] || AUTO_REBOOT=no
fi

# ------------------------------------------------------------ build config

render_config() {
	_tls_ca=""
	[ -n "$CA_FILE" ] && _tls_ca="$CONF_DIR/ca.pem"
	_insecure=false; [ "$INSECURE_TLS" = yes ] && _insecure=true
	_http=false; [ "$ALLOW_HTTP" = yes ] && _http=true
	_reboot=false; [ "$ALLOW_REBOOT" = yes ] && _reboot=true
	_pm2_enabled=false; [ -n "$PM2_USERS" ] && _pm2_enabled=true

	cat <<EOF
# Linux Dash agent configuration, written by install.sh.
# Full reference: config.example.yaml. Validate changes with:
#   linux-dash-agent validate && systemctl restart linux-dash-agent

server:
  url: "$SERVER"
  enroll_token: "$TOKEN"
  ca_file: "$_tls_ca"
  insecure_skip_verify: $_insecure
  allow_insecure_http: $_http

# Remote commands this server accepts. The dashboard cannot exceed these.
actions:
  updates: true
  reboot: $_reboot
  docker: true
  pm2: true
  systemd: true
  agent_update: true

systemd:
  watch: $(yaml_list "$WATCH")

pm2:
  enabled: $_pm2_enabled
  instances:
EOF
	if [ -n "$PM2_USERS" ]; then
		for _u in $(split_list "$PM2_USERS"); do
			printf '    - user: "%s"\n      binary: "%s"\n' "$_u" "$(pm2_binary_for "$_u")"
		done
	else
		printf '    []\n'
	fi

	printf '\nupdates:\n  policy:\n'
	if [ "$AUTO_UPDATES" = off ]; then
		printf '    enabled: false\n    scope: security\n'
	else
		_days="[]"
		[ "$UPDATE_DAYS" = daily ] || _days=$(yaml_list "$UPDATE_DAYS")
		_ar=false; [ "$AUTO_REBOOT" = yes ] && _ar=true
		cat <<EOF
    enabled: true
    scope: $AUTO_UPDATES
    window:
      days: $_days
      start: "$UPDATE_TIME"
      duration_minutes: 120
      timezone: "$UPDATE_TZ"
    auto_reboot: $_ar
EOF
	fi
}

# ------------------------------------------------------------------ summary

if [ "$KEEP_CONFIG" -eq 0 ]; then
	section "Summary"
	say "  Dashboard       $SERVER"
	[ -n "$CA_FILE" ] && say "  TLS             trusting CA $CA_FILE"
	[ "$INSECURE_TLS" = yes ] && say "  TLS             ${Y}certificate not verified${N}"
	[ "$ALLOW_HTTP" = yes ] && ! is_local_url "$SERVER" && say "  Transport       ${Y}unencrypted HTTP${N}"
	say "  Remote reboot   $ALLOW_REBOOT"
	say "  Watch services  ${WATCH:-none}"
	say "  PM2 users       ${PM2_USERS:-none}"
	if [ -S /var/run/docker.sock ]; then say "  Docker          detected, containers will be monitored"; else say "  Docker          not found"; fi
	if [ "$AUTO_UPDATES" = off ]; then
		say "  Auto-updates    off"
	else
		say "  Auto-updates    $AUTO_UPDATES, $UPDATE_DAYS at $UPDATE_TIME $UPDATE_TZ (reboot: $AUTO_REBOOT)"
	fi
	if [ "$INTERACTIVE" -eq 1 ] && [ "$DRY_RUN" -eq 0 ]; then
		say ""
		ask_yn _go "Install with these settings?" y
		[ "$_go" = yes ] || die "aborted; nothing was changed"
	fi
fi

if [ "$DRY_RUN" -eq 1 ]; then
	section "Config that would be written to $CONF_FILE"
	if [ "$KEEP_CONFIG" -eq 1 ]; then say "(existing config kept)"; else render_config; fi
	exit 0
fi

# ------------------------------------------------------------------- binary

section "Installing"
# Stage next to the destination: /tmp may be mounted noexec, and mv is atomic.
mkdir -p "${BIN_PATH%/*}"
TMP=$(mktemp "${BIN_PATH%/*}/.linux-dash-agent.XXXXXX")

SCRIPT_DIR=""
case "$0" in */*) [ -f "$0" ] && SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd) ;; esac
if [ -z "$BINARY" ] && [ -n "$SCRIPT_DIR" ] && [ -f "$SCRIPT_DIR/linux-dash-agent-linux-$ARCH" ]; then
	BINARY="$SCRIPT_DIR/linux-dash-agent-linux-$ARCH"
	info "Using $BINARY"
fi

fetch() {
	if command -v curl >/dev/null 2>&1; then
		if [ -n "$CA_FILE" ]; then curl -q -fsSL --cacert "$CA_FILE" -o "$TMP" "$1"
		elif [ "$INSECURE_TLS" = yes ]; then curl -q -fsSLk -o "$TMP" "$1"
		else curl -q -fsSL -o "$TMP" "$1"; fi
	elif command -v wget >/dev/null 2>&1; then
		if [ "$INSECURE_TLS" = yes ]; then wget -q --no-check-certificate -O "$TMP" "$1"
		else wget -q -O "$TMP" "$1"; fi
	else
		die "curl or wget is required to download the agent (or pass --binary)"
	fi
}

if [ -n "$BINARY" ]; then
	[ -f "$BINARY" ] || die "binary not found: $BINARY"
	cp "$BINARY" "$TMP"
else
	if [ -z "$DOWNLOAD_URL" ]; then
		_base=$SERVER
		[ "$KEEP_CONFIG" -eq 1 ] && _base=$(sed -n 's/^  url: *"\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' "$CONF_FILE" | head -n 1)
		DOWNLOAD_URL="$_base/downloads/linux-dash-agent-linux-$ARCH"
	fi
	info "Downloading $DOWNLOAD_URL"
	if ! fetch "$DOWNLOAD_URL"; then
		warn "Download failed."
		[ "$INTERACTIVE" -eq 1 ] || die "couldn't download the agent; pass --binary PATH"
		while :; do
			ask BINARY "Path to a linux-dash-agent-linux-$ARCH binary on this server:"
			[ -f "$BINARY" ] && break
			say "  File not found: $BINARY"
		done
		cp "$BINARY" "$TMP"
	fi
fi
chmod 0755 "$TMP"
"$TMP" version >/dev/null 2>&1 || die "the agent binary doesn't run on this server (wrong architecture? expected linux-$ARCH)"
mv -f "$TMP" "$BIN_PATH"
TMP=""
ok "Installed agent $("$BIN_PATH" version) to $BIN_PATH"

# ------------------------------------------------------------------- config

install -d -m 0700 "$CONF_DIR"
if [ "$KEEP_CONFIG" -eq 1 ]; then
	ok "Kept existing $CONF_FILE"
else
	[ -n "$CA_FILE" ] && install -m 0644 "$CA_FILE" "$CONF_DIR/ca.pem"
	(umask 077 && render_config > "$CONF_FILE.new")
	if ! _err=$("$BIN_PATH" validate -config "$CONF_FILE.new" 2>&1); then
		rm -f "$CONF_FILE.new"
		die "generated config is invalid: $_err"
	fi
	[ -f "$CONF_FILE" ] && cp -p "$CONF_FILE" "$CONF_FILE.bak"
	mv "$CONF_FILE.new" "$CONF_FILE"
	ok "Wrote $CONF_FILE"
fi

# ------------------------------------------------------------------ service

if has_systemd; then
	cat > /etc/systemd/system/linux-dash-agent.service <<'EOF'
[Unit]
Description=Linux Dash agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/linux-dash-agent run -config /etc/linux-dash-agent/config.yaml
Restart=always
RestartSec=5
KillMode=process
TimeoutStopSec=30
StateDirectory=linux-dash-agent
StateDirectoryMode=0700
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF
	systemctl daemon-reload
	systemctl enable linux-dash-agent >/dev/null 2>&1
	systemctl restart linux-dash-agent
	sleep 2
	if systemctl is-active --quiet linux-dash-agent; then
		ok "Service running"
	else
		warn "Service didn't stay up. Recent logs:"
		journalctl -u linux-dash-agent -n 20 --no-pager >&2 || true
		exit 1
	fi
	LOGS="journalctl -u linux-dash-agent -f"
elif command -v rc-service >/dev/null 2>&1; then
	cat > /etc/init.d/linux-dash-agent <<'EOF'
#!/sbin/openrc-run
name="linux-dash-agent"
command="/usr/local/bin/linux-dash-agent"
command_args="run -config /etc/linux-dash-agent/config.yaml"
command_background=true
pidfile="/run/${RC_SVCNAME}.pid"
output_log="/var/log/linux-dash-agent.log"
error_log="/var/log/linux-dash-agent.log"
supervisor=supervise-daemon
respawn_delay=5
depend() { need net; }
start_pre() { checkpath --directory --mode 0700 /var/lib/linux-dash-agent; }
EOF
	chmod 0755 /etc/init.d/linux-dash-agent
	rc-update add linux-dash-agent default >/dev/null
	rc-service linux-dash-agent restart >/dev/null
	ok "Service running"
	LOGS="tail -f /var/log/linux-dash-agent.log"
else
	warn "No systemd or OpenRC found. Start the agent yourself: $BIN_PATH run -config $CONF_FILE"
	LOGS="(run the agent in the foreground to see logs)"
fi

say ""
say "${G}${B}Done.${N} This server should appear in the dashboard within a few seconds."
say "  Logs:          $LOGS"
say "  Check config:  $BIN_PATH validate"
say "  See metrics:   $BIN_PATH check"
say ""
