# Decisions and assumptions

Where the spec offers a choice (section 10.2) or is silent, this is what the
implementation does. Each item says where to change it.

## Decided with the school

| Question | Decision | Change in |
|---|---|---|
| Language | Switchable: `lang = ru` or `en` covers the bot, the QR page and the admin UI. The agent's idle warning follows `AGENT_LANG`. | `attendance.conf`, `agent.conf` |
| How hours are counted (10.2 #5) | Exact minutes from the scan to the last activity. No rounding, no minimum visit length, no daily cap. All three exist as settings. | `hours_rounding`, `hours_min_visit`, `hours_daily_cap` |
| Nightly cleanup (3.3) | At 04:00 **every** open visit is closed at its PC's last activity and flagged auto-closed, even if the student is still working. The PC session stays alive and the student scans again. The bot tells them so. | `cleanup_at` |
| Browsers on the lab PCs | Firefox, Chrome and Chromium all get the QR page as a locked homepage. | `attendance_browsers` |

## Spec recommendations followed

* One open visit per student. A new scan on another PC closes the earlier visit at that PC's last activity (10.2 #2).
* Two students on one PC session are allowed and flagged on the Anomalies page (10.2 #3).
* `/checkout` is included (10.2 #4). It records "now" if the PC was active in the last 2 minutes (`checkout_slack`). Otherwise it records the last activity, so `/checkout` sent from the bus does not add the trip home.
* Teachers have read-only access. Admins can edit, correct, unlink and see the audit log (10.2 #6).
* The host key is readable under the shared student profile (10.2 #11). Lab PCs are still refused on the admin UI.
* Visits are split at local midnight (10.2 #12). DST is handled.
* The Telegram alert script is included (10.2 #13).
* "Present" means the moment of the scan (10.2 #8).

## Left open or out of scope for version 1

* **Late scans / grace periods (10.2 #9).** There is no lesson schedule in the system (the area is open 24/7 with sit-anywhere), so there is no "late" flag.
* **Data retention (10.2 #7).** Nothing is deleted automatically except, optionally, raw heartbeat events (`heartbeat_retention_days`). Decide with whoever owns data protection.
* **Fallback without a phone (10.2 #10).** Staff add the visit manually with a reason (Student page → Add a missed visit).
* **Agent event queue.** Spec 10.1 suggests the agent queue events while the server is down. Delayed events would carry the wrong time because the server uses only its own clock. So the agent retries briefly (idle logout 3 times) and gives up. An outage longer than 3 minutes closes visits at the last activity seen, and staff correct them manually.

## Implementation choices

* **Telegram client.** The bot calls the Bot API directly with `net/http` (getUpdates long polling, sendMessage, getMe, setMyCommands) instead of `go-telegram/bot`. That keeps the dependencies to the SQLite driver and bcrypt, and every call is visible in `internal/bot/api.go`.
* **PC identity is the source IP**, mapped through `hosts.csv`, which Ansible generates from the inventory. The PCs need fixed addresses (DHCP reservations are fine). Host ids live in the database so they stay stable when the file is reordered. The server reloads the file within a minute of a change.
* **Session id** = boot id + `XDG_SESSION_ID`. An agent that restarts inside the same login continues the same session. A new login on the same PC while the old one still looks alive closes the old visits as "PC restarted".
* **Deterministic timeout.** A session is dead once more than 3 minutes pass between its events, whether or not the periodic sweep has run. This makes live processing and `replay` give identical results.
* **A heartbeat without an idle time** (flaky D-Bus call) keeps the session alive but does not move the last activity forward.
* **QR tokens.** The format is `c` + base64url(version, host id, issue second, 6-byte nonce, 12-byte HMAC-SHA256), 35 characters. Tokens rotate every 30 s, are accepted for 60 s, and rotate immediately after use so a pair-programming partner gets a fresh code. Valid but superseded codes are accepted and flagged "old code".
* **Scan checks run in this order:** signature → known host → age → account linked → single use → live session. An unlinked phone does not burn a code.
* **Admin UI hardening.** Lab PCs cannot reach `/admin` (`admin_from_lab_pcs`). Logins are throttled (5 failures in 15 min lock the IP and the username for 15 min). Sessions use HttpOnly SameSite=Strict cookies with a 2 h idle and 12 h absolute timeout. POSTs need a CSRF token. The Content-Security-Policy allows no inline script.
* **Backups** use SQLite `VACUUM INTO` at 03:30, 14 copies kept. Copy `/var/backups/attendance` to another machine for real off-site safety.
* **Health and alerts.** `/healthz` (localhost only) turns red when the bot has not reached Telegram for 15 minutes, or when no PC has reported for 15 minutes **and** the last session ended by timeout. An empty lab after normal logouts is not an alert.
* **Anomaly "one student on several PCs"** means two scans on different PCs within 15 minutes, after which the first PC kept being used for more than 2 minutes after the second scan.
* **Server on the admin PC, installed by `deploy/install.sh`.** One script, run by the admin user. It downloads a GitHub release (the repository is private, so it uses the `gh` login or a pasted read-only token that is never saved), verifies SHA256 sums, and runs the same Ansible roles. The server play runs locally as root and the PC play runs as the admin user with the existing inventory (`hosts.yml`, group `students`, SSH on port 512). Secrets never appear on a command line: they go to 0600 files in a private temporary directory.
* **overlayroot on the lab PCs.** `/` is a RAM overlay over a read-only disk, so the agent role also writes its files to that lower disk. It remounts the disk read-write, copies the files, checks them and remounts it read-only. It does not copy through `overlayroot-chroot`: on the lab PCs that tool often cannot unmount its `/run` and `/sys` binds afterwards ("target is busy"). It then leaves the disk read-write and fails every later call. The chroot is used only to install missing packages. Its exit status is ignored, the result is checked instead, and any `/dev`, `/proc`, `/run` or `/sys` mounts it leaves on the lower disk are detached. The first time a file there is replaced, the original is kept as `<file>.orig`, for example `idle_logout.orig`. Only files with "attendance" in the name are ever removed from the lower disk.
* **`keepalive.yml` defeats idle detection.** The admin playbook that injects mouse movement every 60 s makes the agent see constant activity. While it runs, idle time counts as presence. Use it only when attendance does not matter (exams, demos).
* **CLI outside systemd.** `host_key_file` is required only by `serve` and `check-config`. The other commands (`user add`, `students import`, `code`, …) run with `sudo -u attendance attendance … -config /etc/attendance/attendance.conf`, where the systemd credential directory does not exist.
