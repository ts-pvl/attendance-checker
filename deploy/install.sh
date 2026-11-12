#!/usr/bin/env bash
# Install, start and set up the QR attendance system from the admin PC
# (Ubuntu). One file: it fetches a release with git, installs the server on
# this PC (systemd, starts at boot) and then deploys the agent to the lab PCs
# listed in an Ansible inventory.
#
#   git clone https://github.com/ts-pvl/attendance-checker.git
#   cd attendance-checker
#   ./deploy/install.sh                  first install, or upgrade to the latest release
#   ./deploy/install.sh --limit PC-01    try the agent on one PC first
#   ./deploy/install.sh --help           all options
#
# The release is the newest version tag (vYYYY.MM.DD) of the repository. The
# code comes from a shallow git clone of that tag. The server binary is the
# one published with the release, checked against its SHA256 sum, or it is
# built from the cloned source with Go (--build, or when none is published).
#
# Run it as the admin user that normally runs Ansible (it uses that user's SSH
# key for the lab PCs); it asks for the sudo password once. Running it again is
# safe: answers are remembered in /etc/attendance/install.env (no secrets),
# secrets stay in root-only files under /etc/attendance.
set -euo pipefail

DEFAULT_REPO_URL="https://github.com/ts-pvl/attendance-checker.git"
PREFIX=/opt/attendance
ETC=/etc/attendance
SETTINGS="$ETC/install.env"
CONF="$ETC/attendance.conf"

usage() {
  cat <<'EOF'
Usage: install.sh [options]

  --version TAG      release to install (default: the newest version tag)
  --repo URL         git repository to install from (default: the origin of
                     the clone this script is in, else the project's repository)
  --build            build the server from the source with Go instead of
                     downloading the published binary
  --inventory FILE   Ansible inventory of the lab PCs
  --group NAME       inventory group of the lab PCs (default: students)
  --limit PATTERN    deploy the agent only to these PCs, e.g. PC-01 or 'PC-0*'
  --server-only      install or upgrade the server, skip the lab PCs
  --pcs-only         deploy the agent to the lab PCs only
  --reconfigure      ask the setup questions again (bot, address, time zone, …)
  --yes              do not ask before deploying to the lab PCs
  --from-dir DIR     use release files from DIR instead of downloading
                     (attendance-linux-<arch>, attendance-deploy.tar.gz, SHA256SUMS)
  -h, --help         this help

Environment: ATTENDANCE_BOT_TOKEN, ANSIBLE_BIN (directory of another Ansible
install; default /usr/bin, from apt).
EOF
}

# ── output ─────────────────────────────────────────────────────────────────
if [[ -t 1 ]]; then B=$'\e[1m' G=$'\e[32m' Y=$'\e[33m' R=$'\e[31m' N=$'\e[0m'; else B="" G="" Y="" R="" N=""; fi
step() { printf '\n%s==> %s%s\n' "$B" "$*" "$N"; }
info() { printf '    %s\n' "$*"; }
ok()   { printf '    %s✓%s %s\n' "$G" "$N" "$*"; }
warn() { printf '    %s!%s %s\n' "$Y" "$N" "$*" >&2; }
die()  { printf '\n%serror:%s %s\n' "$R" "$N" "$*" >&2; exit 1; }

ask() {  # ask VAR "question" "default"
  local __v="$1" q="$2" def="${3:-}" a
  [[ -t 0 ]] || die "no terminal to ask: $q (run it interactively)"
  if [[ -n "$def" ]]; then read -rp "    $q [$def]: " a; else read -rp "    $q: " a; fi
  printf -v "$__v" '%s' "${a:-$def}"
}
ask_secret() {  # ask_secret VAR "question"
  local __v="$1" a
  [[ -t 0 ]] || die "no terminal to ask: $2 (run it interactively)"
  read -rsp "    $2: " a; echo
  printf -v "$__v" '%s' "$a"
}
confirm() {  # confirm "question" → 0 for yes
  local a
  [[ -t 0 ]] || return 1
  read -rp "    $1 [y/N]: " a
  [[ "$a" == [yY]* ]]
}

# ── options ────────────────────────────────────────────────────────────────
TAG="" REPO_URL="" INVENTORY="" GROUP="" LIMIT="" FROM_DIR=""
DO_SERVER=1 DO_PCS=1 RECONFIGURE=0 YES=0 BUILD=0
while (($#)); do
  case "$1" in
    --version) TAG="${2:?}"; shift ;;
    --repo) REPO_URL="${2:?}"; shift ;;
    --build) BUILD=1 ;;
    --inventory) INVENTORY="${2:?}"; shift ;;
    --group) GROUP="${2:?}"; shift ;;
    --limit) LIMIT="${2:?}"; shift ;;
    --server-only) DO_PCS=0 ;;
    --pcs-only) DO_SERVER=0 ;;
    --reconfigure) RECONFIGURE=1 ;;
    --yes|-y) YES=1 ;;
    --from-dir) FROM_DIR="${2:?}"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; die "unknown option: $1" ;;
  esac
  shift
done

# ── preflight ──────────────────────────────────────────────────────────────
step "Checking this PC"
[[ "$(uname -s)" == Linux ]] || die "this script is for the Ubuntu admin PC"
if [[ -r /etc/os-release ]]; then
  # shellcheck disable=SC1091
  . /etc/os-release
  [[ "${ID:-}" == ubuntu || "${ID_LIKE:-}" == *ubuntu* || "${ID_LIKE:-}" == *debian* ]] \
    || warn "not Ubuntu (${PRETTY_NAME:-unknown}); continuing anyway"
  info "${PRETTY_NAME:-Linux}"
fi
case "$(uname -m)" in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported CPU: $(uname -m)" ;;
esac
[[ $EUID -ne 0 ]] || warn "running as root: the lab PCs will be reached with root's SSH key"
for c in curl tar sha256sum python3 ip systemctl sudo; do
  command -v "$c" >/dev/null || die "missing command: $c"
done
[[ -n "$FROM_DIR" ]] || command -v git >/dev/null || die "missing command: git (install it with: sudo apt install git)"
[[ -d /run/systemd/system ]] || die "systemd is not running on this PC"

WORK=$(mktemp -d)
chmod 700 "$WORK"
SUDO_KEEPALIVE=""
cleanup() {
  [[ -n "$SUDO_KEEPALIVE" ]] && kill "$SUDO_KEEPALIVE" 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

info "sudo is needed to install the server"
sudo -v || die "sudo failed"
( while sleep 50; do sudo -n true 2>/dev/null || exit; done ) &
SUDO_KEEPALIVE=$!
ok "sudo"

# Remembered answers from an earlier run (no secrets in this file).
S_INVENTORY="" S_GROUP="" S_SERVER_IP="" S_PORT="" S_BOT_USERNAME="" S_TZ="" S_LANG="" S_ALERT_IDS="" S_TAG="" S_REPO_URL=""
if sudo test -f "$SETTINGS"; then
  eval "$(sudo cat "$SETTINGS" | grep -E '^S_[A-Z_]+=' || true)"
  info "found an earlier install (${S_TAG:-unknown version})"
fi
INVENTORY="${INVENTORY:-$S_INVENTORY}"
GROUP="${GROUP:-${S_GROUP:-students}}"

# ── Ansible ────────────────────────────────────────────────────────────────
step "Ansible"
# The distribution's Ansible is used for both plays: the server play runs as
# root, where a per-user pip install would not be found.
# ANSIBLE_BIN selects another Ansible directory (it must work under sudo too).
ANSIBLE_BIN="${ANSIBLE_BIN:-/usr/bin}"
ANSIBLE_PLAYBOOK="$ANSIBLE_BIN/ansible-playbook"
ansible_ok() {
  [[ -x $ANSIBLE_PLAYBOOK ]] || return 1
  local v
  v=$($ANSIBLE_PLAYBOOK --version 2>/dev/null | sed -n '1s/.*core \([0-9]*\.[0-9]*\).*/\1/p')
  [[ -n "$v" ]] && python3 -c "import sys; sys.exit(0 if tuple(map(int, '$v'.split('.'))) >= (2, 14) else 1)"
}
if ! ansible_ok; then
  info "installing ansible-core from the Ubuntu archive"
  sudo apt-get update -qq
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ansible-core >/dev/null
  ansible_ok || die "ansible-core 2.14 or newer is needed ($($ANSIBLE_PLAYBOOK --version 2>/dev/null | head -1))"
fi
ok "$($ANSIBLE_PLAYBOOK --version | head -1)"

# ── release ────────────────────────────────────────────────────────────────
step "Getting the release"
DL="$WORK/dl"
SRC="$WORK/src"
mkdir -p "$DL"
BIN_ASSET="attendance-linux-$ARCH"

# check_sums FILE…: every file must be listed in SHA256SUMS and match it.
check_sums() {
  local f
  ( cd "$DL"
    for f in "$@"; do
      grep -E "^[0-9a-f]{64}  $f\$" SHA256SUMS > "sum.$f" || die "$f is not listed in SHA256SUMS"
      sha256sum -c --quiet "sum.$f" >/dev/null 2>&1 || die "checksum mismatch: $f is damaged"
    done )
}

# release_url REPO_URL TAG FILE: where GitHub publishes a release file.
# Prints nothing for a repository that is not on github.com.
release_url() {
  local path
  case "$1" in
    https://github.com/*) path="${1#https://github.com/}" ;;
    git@github.com:*) path="${1#git@github.com:}" ;;
    ssh://git@github.com/*) path="${1#ssh://git@github.com/}" ;;
    *) return 0 ;;
  esac
  path="${path%/}"
  printf 'https://github.com/%s/releases/download/%s/%s\n' "${path%.git}" "$2" "$3"
}

# Build the server from the cloned source. Go downloads the toolchain version
# named in go.mod when the installed one is older.
build_binary() {
  command -v go >/dev/null || return 1
  info "building the server from source with $(go version | cut -d' ' -f3)"
  ( cd "$SRC" && GOTOOLCHAIN="${GOTOOLCHAIN:-auto}" CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" GOFLAGS=-buildvcs=false \
      go build -trimpath -ldflags "-s -w -X main.version=${TAG#v}" -o "$DL/$BIN_ASSET" ./cmd/attendance ) 2>&1 | sed 's/^/    /'
  [[ -x "$DL/$BIN_ASSET" ]]
}

if [[ -n "$FROM_DIR" ]]; then
  for f in "$BIN_ASSET" attendance-deploy.tar.gz SHA256SUMS; do
    [[ -f "$FROM_DIR/$f" ]] || die "$FROM_DIR/$f not found"
    cp "$FROM_DIR/$f" "$DL/"
  done
  check_sums "$BIN_ASSET" attendance-deploy.tar.gz
  TAG="${TAG:-local-$(date +%Y%m%d%H%M%S)}"
  ok "release $TAG from $FROM_DIR (checksums verified)"
else
  # Which repository: --repo, the clone this script is run from, the one
  # used last time, or the project's own.
  if [[ -z "$REPO_URL" ]]; then
    here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
    if git -C "$here" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
      REPO_URL=$(git -C "$here" remote get-url origin 2>/dev/null || true)
    fi
    REPO_URL="${REPO_URL:-${S_REPO_URL:-$DEFAULT_REPO_URL}}"
  fi
  [[ "$REPO_URL" != -* ]] || die "bad repository: $REPO_URL"
  info "repository: $REPO_URL"
  # Git may ask for a login (a private repository). Without a terminal it
  # must fail instead of waiting forever.
  [[ -t 0 ]] || export GIT_TERMINAL_PROMPT=0

  if [[ -z "$TAG" ]]; then
    tags=$(git ls-remote --tags --refs "$REPO_URL" 'v[0-9]*') \
      || die "cannot read $REPO_URL (check the address and your access to it)"
    TAG=$(printf '%s\n' "$tags" | sed -n 's|.*refs/tags/||p' | sort -V | tail -n 1)
    [[ -n "$TAG" ]] || die "$REPO_URL has no version tags (vYYYY.MM.DD); name a tag or branch with --version"
  fi
  [[ "$TAG" =~ ^[A-Za-z0-9._-]+$ && "$TAG" != -* ]] || die "bad version: $TAG"
  git -c advice.detachedHead=false clone --quiet --depth 1 --branch "$TAG" "$REPO_URL" "$SRC" \
    || die "git clone of $TAG from $REPO_URL failed"
  [[ -f "$SRC/deploy/ansible/site.yml" && -f "$SRC/agent/idle_logout" && -f "$SRC/go.mod" ]] \
    || die "$REPO_URL at $TAG is not the attendance system"
  ok "source $TAG (commit $(git -C "$SRC" rev-parse --short HEAD))"

  # The server binary: the one published with the release, else built here.
  got=""
  bin_url=$(release_url "$REPO_URL" "$TAG" "$BIN_ASSET")
  if (( ! BUILD )) && [[ -n "$bin_url" ]]; then
    if curl -fsSL --max-time 300 -o "$DL/$BIN_ASSET.part" "$bin_url" 2>/dev/null \
       && curl -fsSL --max-time 60 -o "$DL/SHA256SUMS" "$(release_url "$REPO_URL" "$TAG" SHA256SUMS)" 2>/dev/null; then
      mv "$DL/$BIN_ASSET.part" "$DL/$BIN_ASSET"
      check_sums "$BIN_ASSET"
      chmod 0755 "$DL/$BIN_ASSET"
      got="downloaded, checksum verified"
    else
      info "no published binary for $TAG at ${bin_url%/*}"
    fi
  fi
  if [[ -z "$got" ]]; then
    build_binary || die "the server binary is not available.
       It is downloaded from the release page, which works only when the
       repository is public and the release has the file $BIN_ASSET.
       Otherwise install Go and run again with --build:  sudo apt install golang-go"
    got="built from source"
  fi
  ok "server binary $BIN_ASSET ($got)"
  # The same files a release bundle holds, taken from the clone.
  git -C "$SRC" archive --format=tar.gz -o "$DL/attendance-deploy.tar.gz" HEAD deploy agent docs ':(glob)README*.md' \
    || die "cannot pack the release files"
fi

REL="$PREFIX/releases/$TAG"
sudo rm -rf "$REL.new"
sudo mkdir -p "$REL.new/dist"
sudo tar -xzf "$DL/attendance-deploy.tar.gz" -C "$REL.new" --no-same-owner
sudo install -m 0755 "$DL/$BIN_ASSET" "$REL.new/dist/$BIN_ASSET"
sudo rm -rf "$REL"
sudo mv "$REL.new" "$REL"
sudo ln -sfn "$REL" "$PREFIX/current"
[[ -f "$REL/deploy/ansible/site.yml" && -f "$REL/agent/idle_logout" ]] || die "the release bundle is incomplete"
BIN="$REL/dist/$BIN_ASSET"
VERSION=$("$BIN" version 2>/dev/null || echo "$TAG")
ok "unpacked to $REL ($VERSION)"

# ── inventory ──────────────────────────────────────────────────────────────
step "Lab PCs (Ansible inventory)"
if [[ -z "$INVENTORY" || $RECONFIGURE -eq 1 ]]; then
  def="$INVENTORY"
  if [[ -z "$def" ]]; then
    for c in "$HOME/ansible/hosts.yml" "$HOME/ansible/inventory.ini" "$PWD/hosts.yml" "$PWD/inventory.ini"; do
      [[ -f "$c" ]] && { def="$c"; break; }
    done
  fi
  ask INVENTORY "Inventory file with the lab PCs" "$def"
fi
INVENTORY_ABS=$(realpath -e "$INVENTORY" 2>/dev/null) || die "inventory not found: $INVENTORY"
INVENTORY="$INVENTORY_ABS"
ANSIBLE_INVENTORY_BIN="$ANSIBLE_BIN/ansible-inventory"
"$ANSIBLE_INVENTORY_BIN" -i "$INVENTORY" --list > "$WORK/inventory.json" 2>"$WORK/inventory.err" \
  || { cat "$WORK/inventory.err" >&2; die "cannot read the inventory"; }
python3 - "$WORK/inventory.json" "$GROUP" > "$WORK/pcs.tsv" <<'PY' || die "inventory group '$GROUP' has no hosts (use --group)"
import json, sys
inv, group = json.load(open(sys.argv[1])), sys.argv[2]
def hosts(g, seen=set()):
    if g in seen or g not in inv: return []
    seen.add(g)
    out = list(inv[g].get("hosts", []))
    for c in inv[g].get("children", []): out += hosts(c, seen)
    return out
hv = inv.get("_meta", {}).get("hostvars", {})
hs = sorted(set(hosts(group)))
if not hs: sys.exit(1)
for h in hs:
    v = hv.get(h, {})
    print(f'{v.get("attendance_pc_name", h.upper())}\t{v.get("ansible_host", h)}')
PY
PC_COUNT=$(wc -l < "$WORK/pcs.tsv")
python3 - "$WORK/pcs.tsv" <<'PY' || die "every lab PC needs a unique fixed IP address (ansible_host) in the inventory"
import ipaddress, sys
seen = {}
bad = False
for line in open(sys.argv[1]):
    name, ip = line.rstrip("\n").split("\t")
    try: ipaddress.ip_address(ip)
    except ValueError:
        print(f"    ! {name}: ansible_host '{ip}' is not an IP address", file=sys.stderr); bad = True; continue
    if ip in seen:
        print(f"    ! {name} and {seen[ip]} share {ip}", file=sys.stderr); bad = True
    seen[ip] = name
sys.exit(1 if bad else 0)
PY
ok "$PC_COUNT PCs in group '$GROUP' of $INVENTORY"
FIRST_PC_IP=$(head -1 "$WORK/pcs.tsv" | cut -f2)

# ── settings ───────────────────────────────────────────────────────────────
SERVER_IP="$S_SERVER_IP" PORT="${S_PORT:-8080}" BOT_USERNAME="$S_BOT_USERNAME"
TZ_NAME="$S_TZ" UI_LANG="${S_LANG:-ru}" ALERT_IDS="$S_ALERT_IDS"
BOT_TOKEN="${ATTENDANCE_BOT_TOKEN:-}"
HAVE_TOKEN=0
sudo test -s "$ETC/bot_token" && HAVE_TOKEN=1

if (( DO_SERVER )); then
  step "Settings"
  if [[ -z "$SERVER_IP" || $RECONFIGURE -eq 1 ]]; then
    def=$(ip -4 route get "$FIRST_PC_IP" 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p')
    info "The lab PCs reach the server at this PC's address. It must not change:"
    info "give this PC a static IP or a DHCP reservation."
    ask SERVER_IP "This PC's IP address on the lab network" "${SERVER_IP:-$def}"
    ask PORT "Port" "$PORT"
  fi
  python3 -c 'import ipaddress,sys; ipaddress.IPv4Address(sys.argv[1])' "$SERVER_IP" 2>/dev/null || die "not an IPv4 address: $SERVER_IP"
  [[ "$PORT" =~ ^[0-9]+$ ]] && (( PORT >= 1024 && PORT <= 65535 )) || die "port must be 1024-65535"
  ip -4 -o addr show | grep -q " $SERVER_IP/" || warn "$SERVER_IP is not an address of this PC"
  if awk -F'\t' -v ip="$SERVER_IP" '$2 == ip {f=1} END {exit !f}' "$WORK/pcs.tsv"; then
    die "$SERVER_IP is listed as a lab PC in the inventory"
  fi

  if [[ -n "$BOT_TOKEN" || $HAVE_TOKEN -eq 0 || $RECONFIGURE -eq 1 ]]; then
    if [[ -z "$BOT_TOKEN" ]]; then
      info "Telegram bot token from @BotFather (input is hidden)."
      [[ $HAVE_TOKEN -eq 1 ]] && info "Leave empty to keep the current token."
      ask_secret BOT_TOKEN "Bot token"
    fi
  fi
  if [[ -n "$BOT_TOKEN" ]]; then
    [[ "$BOT_TOKEN" =~ ^[0-9]+:[A-Za-z0-9_-]{30,}$ ]] || die "that does not look like a bot token (123456789:AA…)"
    # Check it with Telegram and read the bot's username. The token is part of
    # the URL, so the URL goes to curl in a 0600 file, not on the command line.
    ( umask 077; printf 'url = "https://api.telegram.org/bot%s/getMe"\n' "$BOT_TOKEN" > "$WORK/tg.curl" )
    me=$(curl -sS --max-time 20 -K "$WORK/tg.curl" 2>/dev/null || true)
    rm -f "$WORK/tg.curl"
    BOT_USERNAME=$(printf '%s' "$me" | python3 -c 'import json,sys
try: d = json.load(sys.stdin)
except Exception: sys.exit(0)
print(d["result"]["username"] if d.get("ok") else "")' 2>/dev/null || true)
    if [[ -z "$BOT_USERNAME" ]]; then
      if printf '%s' "$me" | grep -q '"ok":false'; then die "Telegram rejected the bot token"; fi
      warn "could not reach Telegram to check the token"
      ask BOT_USERNAME "Bot username (without @)" "$S_BOT_USERNAME"
    fi
    ok "bot @$BOT_USERNAME"
  elif [[ $HAVE_TOKEN -eq 1 ]]; then
    BOT_TOKEN=$(sudo cat "$ETC/bot_token")
    [[ -n "$BOT_USERNAME" ]] || ask BOT_USERNAME "Bot username (without @)" ""
    ok "keeping the bot token (@$BOT_USERNAME)"
  else
    die "a bot token is needed"
  fi
  BOT_USERNAME="${BOT_USERNAME#@}"
  [[ "$BOT_USERNAME" =~ ^[A-Za-z0-9_]{5,32}$ ]] || die "bad bot username: $BOT_USERNAME"

  if [[ -z "$TZ_NAME" || $RECONFIGURE -eq 1 ]]; then
    def=$(timedatectl show -p Timezone --value 2>/dev/null || true)
    ask TZ_NAME "Time zone of the school" "${TZ_NAME:-${def:-Asia/Almaty}}"
    ask UI_LANG "Language of the bot and the PCs (ru/en)" "$UI_LANG"
    info "Telegram user ids of staff to message when the system is down"
    info "(each must have pressed Start in the bot; /id shows the number). Optional."
    ask ALERT_IDS "Alert ids, separated by spaces" "$ALERT_IDS"
  fi
  [[ -f "/usr/share/zoneinfo/$TZ_NAME" ]] || die "unknown time zone: $TZ_NAME"
  [[ "$UI_LANG" == ru || "$UI_LANG" == en ]] || die "language must be ru or en"
  [[ "$ALERT_IDS" =~ ^[0-9\ ]*$ ]] || die "alert ids must be numbers"
elif [[ -z "$SERVER_IP" ]]; then
  die "the server is not installed yet: run without --pcs-only first"
fi
SERVER_URL="http://$SERVER_IP:$PORT"

# Host key shared by the lab PCs: generated once, kept root-only.
if sudo test -s "$ETC/host_key"; then
  HOST_KEY=$(sudo cat "$ETC/host_key")
else
  (( DO_SERVER )) || die "no host key yet: run without --pcs-only first"
  HOST_KEY=$("$BIN" gen-key)
fi

# Variables for the playbooks. Secrets go in their own 0600 files.
write_json() {  # write_json FILE KEY=VALUE…  (values from the environment of python)
  local f="$1"; shift
  ( umask 077; python3 - "$@" > "$f" <<'PY'
import json, os, sys
out = {}
for kv in sys.argv[1:]:
    k, _, envname = kv.partition("=")
    v = os.environ[envname]
    if k == "attendance_alert_chat_ids":
        v = [int(x) for x in v.split()]
    out[k] = v
print(json.dumps(out))
PY
  )
}
export _ARCH="$ARCH" _LISTEN="0.0.0.0:$PORT" _URL="$SERVER_URL" _BOT="$BOT_USERNAME" _LANG="$UI_LANG" \
  _TZ="$TZ_NAME" _GROUP="$GROUP" _ALERT="$ALERT_IDS"
write_json "$WORK/vars.json" attendance_arch=_ARCH attendance_listen=_LISTEN attendance_server_url=_URL \
  attendance_bot_username=_BOT attendance_lang=_LANG attendance_timezone=_TZ attendance_pc_group=_GROUP \
  attendance_alert_chat_ids=_ALERT
_HK="$HOST_KEY" write_json "$WORK/pc-secrets.json" attendance_host_key=_HK
_HK="$HOST_KEY" _BT="$BOT_TOKEN" write_json "$WORK/server-secrets.json" attendance_host_key=_HK attendance_bot_token=_BT
unset BOT_TOKEN HOST_KEY

PLAYBOOK="$REL/deploy/ansible/site.yml"
# Use the admin's own Ansible settings for the lab PCs (port, key, forks).
PC_ANSIBLE_CFG=""
[[ -f "$(dirname "$INVENTORY")/ansible.cfg" ]] && PC_ANSIBLE_CFG="$(dirname "$INVENTORY")/ansible.cfg"

# ── server ─────────────────────────────────────────────────────────────────
if (( DO_SERVER )); then
  step "Installing the server on this PC"
  printf '[attendance_server]\nlocalhost ansible_connection=local ansible_python_interpreter=/usr/bin/python3\n' > "$WORK/local.ini"
  # The PC inventory is passed too: hosts.csv is generated from it.
  sudo env ANSIBLE_FORCE_COLOR="${B:+1}" ANSIBLE_RETRY_FILES_ENABLED=0 \
    "$ANSIBLE_PLAYBOOK" -i "$WORK/local.ini" -i "$INVENTORY" "$PLAYBOOK" --limit attendance_server \
    -e "@$WORK/vars.json" -e "@$WORK/server-secrets.json" \
    || die "the server play failed (see above)"

  if sudo ufw status 2>/dev/null | grep -q '^Status: active'; then
    sudo ufw allow "$PORT/tcp" comment 'attendance' >/dev/null && ok "firewall: port $PORT open"
  fi

  info "waiting for the server to answer"
  health=000
  for _ in $(seq 1 30); do
    health=$(curl -sS --max-time 3 -o "$WORK/health.json" -w '%{http_code}' "http://127.0.0.1:$PORT/healthz" 2>/dev/null) || health=000
    [[ "$health" == 000 ]] || break
    sleep 1
  done
  if [[ "$health" == 200 || "$health" == 503 ]]; then
    ok "server running: $(systemctl is-active attendance.service), enabled at boot: $(systemctl is-enabled attendance.service)"
    [[ "$health" == 200 ]] || warn "health check: $(cat "$WORK/health.json")"
  else
    sudo journalctl -u attendance.service -n 20 --no-pager >&2 || true
    die "the server does not answer on port $PORT (log above)"
  fi

  att() { sudo -u attendance /usr/local/bin/attendance "$@" -config "$CONF"; }
  if [[ $(att user list | wc -l) -le 1 ]]; then
    step "First admin account for the web UI"
    ask ADMIN_USER "Username" "admin"
    while :; do
      ask_secret ADMIN_PW "Password (at least 10 characters)"
      ask_secret ADMIN_PW2 "Repeat"
      [[ "$ADMIN_PW" == "$ADMIN_PW2" ]] || { warn "passwords differ"; continue; }
      (( ${#ADMIN_PW} >= 10 )) || { warn "too short"; continue; }
      break
    done
    printf '%s\n' "$ADMIN_PW" | att user add "$ADMIN_USER" -role admin >/dev/null 2>&1 || die "could not create the admin account"
    unset ADMIN_PW ADMIN_PW2
    ok "admin account '$ADMIN_USER' created"
  fi

  sudo install -d -m 0750 -o root -g attendance "$ETC"
  {
    echo "# Written by install.sh: answers for the next run. No secrets."
    for v in INVENTORY GROUP SERVER_IP PORT BOT_USERNAME TZ_NAME:TZ UI_LANG:LANG ALERT_IDS TAG REPO_URL; do
      src="${v%%:*}" dst="${v#*:}"
      printf 'S_%s=%q\n' "$dst" "${!src}"
    done
  } | sudo tee "$SETTINGS" >/dev/null
  sudo chmod 0644 "$SETTINGS"
fi

# ── lab PCs ────────────────────────────────────────────────────────────────
PC_STATUS="skipped"
if (( DO_PCS )); then
  step "Agent on the lab PCs"
  target="$GROUP"
  [[ -z "$LIMIT" ]] || target="$GROUP:&$LIMIT"
  mapfile -t TARGETS < <(env ${PC_ANSIBLE_CFG:+ANSIBLE_CONFIG="$PC_ANSIBLE_CFG"} "$ANSIBLE_BIN/ansible" -i "$INVENTORY" "$target" --list-hosts 2>/dev/null | sed -n 's/^ \{4,\}//p')
  (( ${#TARGETS[@]} > 0 )) || die "no lab PCs match '$target'"
  info "${#TARGETS[@]} PCs: ${TARGETS[*]:0:8}$( (( ${#TARGETS[@]} > 8 )) && echo " …")"
  info "This installs the agent as /usr/local/bin/idle_logout and points the"
  info "browsers' home page at $SERVER_URL/qr. On PCs with overlayroot it is"
  info "also written to the disk, so it survives reboots (a file it replaces"
  info "there is kept as <name>.orig). Students get the agent at their next login."
  if (( YES )) || confirm "Deploy to these ${#TARGETS[@]} PCs now?"; then
    set +e
    env ${PC_ANSIBLE_CFG:+ANSIBLE_CONFIG="$PC_ANSIBLE_CFG"} ANSIBLE_FORCE_COLOR="${B:+1}" ANSIBLE_RETRY_FILES_ENABLED=0 \
      "$ANSIBLE_PLAYBOOK" -i "$INVENTORY" "$PLAYBOOK" --limit "$target" \
      -e "@$WORK/vars.json" -e "@$WORK/pc-secrets.json" 2>&1 | tee "$WORK/pcs.log"
    set -e
    mapfile -t FAILED < <(sed 's/\x1b\[[0-9;]*m//g' "$WORK/pcs.log" \
      | awk '/PLAY RECAP/ {r=1; next} r && / : ok=/ && (/unreachable=[1-9]/ || /failed=[1-9]/) {print $1}')
    if (( ${#FAILED[@]} )); then
      PC_STATUS="${#FAILED[@]} of ${#TARGETS[@]} PCs failed or were off"
      warn "$PC_STATUS: ${FAILED[*]}"
      warn "when they are on, run:  $0 --pcs-only --limit '$(IFS=,; echo "${FAILED[*]}")'"
    else
      PC_STATUS="all ${#TARGETS[@]} PCs done"
      ok "$PC_STATUS"
    fi
  else
    PC_STATUS="not deployed (run: $0 --pcs-only)"
    info "skipped"
  fi
fi

# ── summary ────────────────────────────────────────────────────────────────
step "Done: $VERSION"
cat <<EOF
    Admin UI      $SERVER_URL/admin   (from this PC: http://127.0.0.1:$PORT/admin)
    QR page       $SERVER_URL/qr      (opens on the lab PCs after login)
    Telegram bot  @$BOT_USERNAME
    Lab PCs       $PC_STATUS
    Server        sudo systemctl status attendance   ·   journalctl -u attendance -f
    Docs          $REL/docs/  (staff-runbook: .md English, .ru.md русский, .kk.md қазақша)

    Next: Admin UI → Students → import the CSV, create links and print the
    slips, then have a student scan the QR code on a PC.
    Run this script again to upgrade; answers are remembered.
EOF
