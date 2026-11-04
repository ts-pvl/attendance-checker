# QR presence check-in

Attendance for a programming school with shared Linux PCs. A student logs into
a PC, the browser shows a QR code that changes every 30 seconds, the student
scans it with the phone camera, Telegram opens the school's bot and the bot
answers "Checked in at PC-17". When the student leaves, idle logout (or a
normal logout, or the PC going silent) closes the visit at the last moment of
activity. Staff see hours per day, week and month in a web UI.

The spec is `QR_Presence_Check_in_System_Development,_Testing_and_Rollout_Plan.pdf`.
Choices made where the spec left things open are in [docs/decisions.md](docs/decisions.md).

```
 lab PC (GNOME)                         server (Pi / mini PC / admin PC)             phone
┌──────────────────────┐  POST /event  ┌───────────────────────────────┐  long poll  ┌──────────┐
│ idle_logout agent    │──────────────▶│ attendance (one Go binary)    │◀───────────▶│ Telegram │
│  session_start,      │  host key +   │  • rules engine + SQLite      │  outbound   │   bot    │
│  heartbeat <idle_ms>,│  IP allowlist │  • Telegram bot (goroutine)   │  only       └──────────┘
│  idle_logout, logout │               │  • /qr page for the PCs       │                  ▲
│ browser → /qr  ◀─────┼── GET /qr ────│  • /admin UI for staff (LAN)  │                  │ scan
│  (rotating QR code) ─┼───────────────┼───────────────────────────────┼──────────────────┘
└──────────────────────┘               └───────────────────────────────┘
```

| Part | Where |
|---|---|
| Server, bot, admin UI | `cmd/attendance`, `internal/*` (Go, standard library + `modernc.org/sqlite` + `x/crypto/bcrypt`) |
| PC agent (revised `idle_logout`) | `agent/idle_logout` |
| Deployment | `deploy/ansible` (roles `attendance_server`, `attendance_agent`), `deploy/systemd`, `deploy/alert` |
| Staff runbook, student instructions | `docs/` |

## How it works

* **Identity** is the Telegram account. Each student links it once with a
  personal one-time link (`https://t.me/<bot>?start=<code>`) printed or sent
  privately by staff.
* **Location** is the PC screen. The QR encodes `https://t.me/<bot>?start=<token>`.
  The token is HMAC-signed (host, issue time, nonce), 35 characters long, valid
  for 60 s and usable once. The `/qr` page and `/event` only answer the
  addresses listed in `hosts.csv`.
* **Raw events first.** Every agent report, scan, `/checkout` and nightly
  cleanup is stored in an append-only `events` table. Sessions and visits are
  derived from it by a single deterministic function, and
  `attendance replay` rebuilds them from scratch.
* **Check-out** is the last activity: heartbeat receive time minus reported
  idle. Idle logout does not credit 30 phantom minutes. A PC that is silent
  for more than 3 minutes is closed at its last activity. At 04:00 every visit
  still open is closed and flagged *auto-closed*.
* **Hours** are computed from visits plus staff corrections. Corrections never
  overwrite raw rows. Overlaps are merged and visits are split at midnight.
  Rounding, a minimum visit length and a daily cap are configurable, and all
  are off by default. The Hours screen, the student page, the bot and the CSV
  export all use the same query.

## Build and test

Go 1.26+ (the module pins `toolchain go1.27.3`; older `go` commands download it).

```bash
make test          # unit, scenario, security and replay tests
make race          # same with the race detector
make load          # 50 PCs × 20 heartbeats + a burst of 50 scans
make fuzz          # fuzz the token parser for 60 s
make check         # vet, staticcheck, govulncheck, gosec, shellcheck, race
make pi            # dist/attendance-linux-arm64 (pi32 / amd64 for other targets)
```

## Try it locally

```bash
mkdir -p /tmp/att && cd /tmp/att
printf 'name,ip\nPC-01,127.0.0.1\n' > hosts.csv
attendance gen-key > host_key
cat > att.conf <<EOF
listen = 127.0.0.1:8080
db = /tmp/att/att.db
bot_username = YourTestBot
host_key_file = /tmp/att/host_key
hosts_file = /tmp/att/hosts.csv
token_secret_file = /tmp/att/token_secret
backup_dir = /tmp/att/backups
admin_networks = 127.0.0.0/8
admin_from_lab_pcs = true
EOF
attendance user add admin -role admin -config att.conf
attendance serve -no-bot -config att.conf      # or add bot_token_file = … for a real test bot
```

Open http://127.0.0.1:8080/admin. To make the QR page show a code, pretend to be the PC agent:

```bash
curl -H "X-Host-Key: $(cat /tmp/att/host_key)" -d kind=session_start -d sid=test-1 http://127.0.0.1:8080/event
```

then open http://127.0.0.1:8080/qr.

## Deploy

1. Create the bot with @BotFather (plus a separate test bot for development).
2. Build: `make pi` (or `make amd64`).
3. Copy `deploy/ansible/inventory.example.ini` to `inventory.ini` and list the
   server and every lab PC with its fixed address. Copy
   `group_vars/all/vault.yml.example` to `vault.yml`, set the host key
   (`attendance gen-key`) and the bot token, then `ansible-vault encrypt` it.
4. `cd deploy/ansible && ansible-playbook -i inventory.ini site.yml --ask-vault-pass`
5. On the server: `sudo -u attendance attendance user add <name> -role admin -config /etc/attendance/attendance.conf`
6. Import students (admin UI → Students → CSV), create links, print slips.
7. Set the server's BIOS/firmware to power on after a power failure.

The server runs under systemd with `Restart=always` and no start limit, so it
comes back by itself after crashes and power cuts. The nightly cleanup and
backup run at configurable hours and catch up after downtime. The optional alert timer
messages staff in Telegram when the server is down, the bot cannot reach
Telegram, or the PCs stop reporting mid-session.

## Configuration

See `deploy/attendance.conf.example`. Secrets (bot token, host key) are
root-only files that systemd passes to the service with `LoadCredential=`.
The QR token HMAC key is generated on first start in `/var/lib/attendance/token_secret`.

## CLI

```
attendance serve [-no-bot]
attendance check-config
attendance user add|passwd|disable|enable|list
attendance students import <file.csv> | students list
attendance code <student-id>            # one-time enrollment link
attendance unlink <student-id> -reason "…"
attendance replay                       # rebuild visits from raw events (stop the service first)
attendance backup <file>
attendance gen-key
```

All commands take `-config` (default `/etc/attendance/attendance.conf`).
