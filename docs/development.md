# Technical reference and development

For installing and using the system, start with the [README](../README.md)
([русский](../README.ru.md), [қазақша](../README.kk.md)). This page is for
people who change the code or want to know how it works inside. Choices made
where the spec left things open are in [decisions.md](decisions.md).
The original spec is the PDF in the repository root.

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
| Local development, sample data | `scripts/dev.sh`, `cmd/devseed` |
| Staff runbook, student instructions (English, Russian, Kazakh), decisions | `docs/` |

## Run it locally (`make dev`)

Needs Go 1.26+ and `curl`. From the repository root:

```bash
make dev
```

This builds the binary and creates a local setup in `.dev/` the first time
(config, host key, an `admin` user, sample students). It also adds four weeks of
invented attendance on PC-01…PC-12, so the admin UI has data to show. The data
is made with `cmd/devseed` through the normal events, once per `.dev/`;
`SEED=0 make dev` skips it. It starts the server and
simulates one lab PC (this machine plays PC-01). Then open:

* http://127.0.0.1:8080/admin to log in as `admin`, password `dev-password-123`
* http://127.0.0.1:8080/qr to see the rotating check-in QR code

**WSL 2:** a browser on Windows connects through the WSL address and arrives
from the Windows host, so `make dev` detects WSL and adjusts. It listens on
all interfaces, lets only the Windows host into admin, and makes the Windows
host play PC-01. The simulated agent sends its events with Windows `curl.exe`.
Use the URLs it prints (`http://<WSL address>:8080/...`), not `localhost`.

To scan and link with a real phone, create a test bot with @BotFather and pass it in:

```bash
BOT_TOKEN=123456:your-test-token BOT_USERNAME=YourTestBot make dev
```

`Ctrl+C` stops everything. Run `rm -rf .dev` to start from scratch. `PORT`
and `ADMIN_PASSWORD` can be overridden the same way.

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
* **One student per PC.** When another student scans on a PC whose session
  already has a checked-in student, the earlier visit ends at the PC's last
  activity and that student is told in Telegram.
* **Check-out** is the last activity: heartbeat receive time minus reported
  idle. Idle logout does not credit 30 phantom minutes. A PC that is silent
  for more than 3 minutes is closed at its last activity. At 04:00 every visit
  still open is closed and flagged *auto-closed*.
* **Hours** are computed from visits plus staff corrections. Corrections never
  overwrite raw rows. Overlaps are merged and visits are split at midnight.
  Rounding, a minimum visit length and a daily cap are configurable, and all
  are off by default. The Hours screen, the student page, the bot and the CSV
  export all use the same query.
* **Admin UI** has two interfaces, switched at the top right. The *Dashboard*
  shows charts, the attendance matrix, a sortable student table, and the
  Students, Anomalies and Journal tabs. The older *Admin* pages have Today,
  Hours, Students, Anomalies and Audit log. Teachers can read; admins can also
  change data. Staff instructions are in [staff-runbook.md](staff-runbook.md).

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

## Run the binary by hand

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

## Deploy by hand with Ansible

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
attendance version
```

All commands take `-config` (default `$ATTENDANCE_CONFIG`, else `/etc/attendance/attendance.conf`).

## Releases

A release is a version tag `vYYYY.MM.DD` (a second one on the same day gets
`.1`, `.2`, …). `deploy/install.sh` installs the newest tag: it clones the tag
with git and downloads `attendance-linux-<arch>` and `SHA256SUMS` from the
GitHub release of the same name. Without those files it builds from source.

```bash
make release      # dist/: binaries for amd64 and arm64, the deploy bundle, SHA256SUMS
gh release create vYYYY.MM.DD dist/attendance-linux-* dist/attendance-deploy.tar.gz dist/install.sh dist/SHA256SUMS
```

`attendance-deploy.tar.gz` is for offline installs: copy the `dist/` files to
the admin PC and run `./install.sh --from-dir <folder>`.
