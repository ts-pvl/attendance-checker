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
* **No pair programming** (changes 10.2 #3). One PC session has one student at a time. When another student scans on the same PC, the earlier student's visit closes at the PC's last activity (reason "another student checked in on this PC"), the new visit starts, and the earlier student is told in Telegram. Sessions where this happened are listed on the Anomalies page. The rule is in the replay path, so running `attendance replay` also applies it to visits recorded while pairs were allowed.
* `/checkout` is included (10.2 #4). It records "now" if the PC was active in the last 2 minutes (`checkout_slack`). Otherwise it records the last activity, so `/checkout` sent from the bus does not add the trip home.
* Teachers have read-only access. Admins can edit, correct, unlink and see the audit log (10.2 #6).
* The host key is readable under the shared student profile (10.2 #11). Lab PCs are still refused on the admin UI.
* Visits are split at local midnight (10.2 #12). DST is handled.
* The Telegram alert script is included (10.2 #13).
* "Present" means the moment of the scan (10.2 #8).

## Left open or out of scope for version 1

* **Late scans / grace periods (10.2 #9).** There is no lesson schedule in the system (the area is open 24/7 with sit-anywhere), so arrivals are never marked late against a lesson. The new admin UI only lists check-ins late in the day (21:00–05:00 by default) as an anomaly (see *New anomaly categories*).
* **Data retention (10.2 #7).** Nothing is deleted automatically except, optionally, raw heartbeat events (`heartbeat_retention_days`). Decide with whoever owns data protection.
* **Fallback without a phone (10.2 #10).** Staff add the visit manually with a reason (Student page → Add a missed visit).
* **Agent event queue.** Spec 10.1 suggests the agent queue events while the server is down. Delayed events would carry the wrong time because the server uses only its own clock. So the agent retries briefly (idle logout 3 times) and gives up. An outage longer than 3 minutes closes visits at the last activity seen, and staff correct them manually.

## Implementation choices

* **Telegram client.** The bot calls the Bot API directly with `net/http` (getUpdates long polling, sendMessage, getMe, setMyCommands) instead of `go-telegram/bot`. That keeps the dependencies to the SQLite driver and bcrypt, and every call is visible in `internal/bot/api.go`.
* **PC identity is the source IP**, mapped through `hosts.csv`, which Ansible generates from the inventory. The PCs need fixed addresses (DHCP reservations are fine). Host ids live in the database so they stay stable when the file is reordered. The server reloads the file within a minute of a change.
* **Session id** = boot id + `XDG_SESSION_ID`. An agent that restarts inside the same login continues the same session. A new login on the same PC while the old one still looks alive closes the old visits as "PC restarted".
* **Deterministic timeout.** A session is dead once more than 3 minutes pass between its events, whether or not the periodic sweep has run. This makes live processing and `replay` give identical results.
* **A heartbeat without an idle time** (flaky D-Bus call) keeps the session alive but does not move the last activity forward.
* **QR tokens.** The format is `c` + base64url(version, host id, issue second, 6-byte nonce, 12-byte HMAC-SHA256), 35 characters. Tokens rotate every 30 s, are accepted for 60 s, and rotate immediately after use so the next student at that PC gets a fresh code. Valid but superseded codes are accepted and flagged "old code".
* **Scan checks run in this order:** signature → known host → age → account linked → single use → live session. An unlinked phone does not burn a code.
* **Admin UI hardening.** Lab PCs cannot reach `/admin` (`admin_from_lab_pcs`). Logins are throttled (5 failures in 15 min lock the IP and the username for 15 min). Sessions use HttpOnly SameSite=Strict cookies with a 2 h idle and 12 h absolute timeout. POSTs need a CSRF token. The Content-Security-Policy allows no inline script.
* **Backups** use SQLite `VACUUM INTO` at 03:30, 14 copies kept. Copy `/var/backups/attendance` to another machine for real off-site safety.
* **Health and alerts.** `/healthz` (localhost only) turns red when the bot has not reached Telegram for 15 minutes, or when no PC has reported for 15 minutes **and** the last session ended by timeout. An empty lab after normal logouts is not an alert.
* **Anomaly "one student on several PCs"** means two scans on different PCs within 15 minutes, after which the first PC kept being used for more than 2 minutes after the second scan.
* **Server on the admin PC, installed by `deploy/install.sh`.** One script, run by the admin user. It uses only `git` and `curl`, with no GitHub CLI and no token. It takes the newest version tag of the repository (`git ls-remote`), makes a shallow clone of it, downloads the server binary published with that release and verifies its SHA256 sum. When no binary is published (a fork, a private repository) or with `--build`, it builds the binary from the clone with Go. The repository defaults to the origin of the clone the script is run from, so a fork installs itself. It then runs the same Ansible roles. The server play runs locally as root and the PC play runs as the admin user with the school's own inventory and Ansible settings (default group `students`, `--group` to change). Secrets never appear on a command line: they go to 0600 files in a private temporary directory.
* **overlayroot on the lab PCs.** `/` is a RAM overlay over a read-only disk, so the agent role also writes its files to that lower disk. It remounts the disk read-write, copies the files, checks them and remounts it read-only. It does not copy through `overlayroot-chroot`: on the lab PCs that tool often cannot unmount its `/run` and `/sys` binds afterwards ("target is busy"). It then leaves the disk read-write and fails every later call. The chroot is used only to install missing packages. Its exit status is ignored, the result is checked instead, and any `/dev`, `/proc`, `/run` or `/sys` mounts it leaves on the lower disk are detached. The first time a file there is replaced, the original is kept as `<file>.orig`, for example `idle_logout.orig`. Only files with "attendance" in the name are ever removed from the lower disk.
* **Keep-awake tools defeat idle detection.** Anything that injects mouse or keyboard input on the lab PCs (a "keep alive" playbook, a mouse jiggler) makes the agent see constant activity. While it runs, idle time counts as presence. Use such tools only when attendance does not matter (exams, demos).
* **CLI outside systemd.** `host_key_file` is required only by `serve` and `check-config`. The other commands (`user add`, `students import`, `code`, …) run with `sudo -u attendance attendance … -config /etc/attendance/attendance.conf`, where the systemd credential directory does not exist.
* **Analytics dashboard (new UI).** `/admin/dashboard` sits next to the existing admin pages. A *Dashboard | Admin* switch at the top right of both moves between them and remembers the choice for the next login. Without a choice, staff land on the old UI. It is read-only, open to teachers, and built from the same `hours.Load` + `Calc.Summarize` as the Hours screen (`internal/dash`). Its definitions are not in the spec:
  * The default period is the current week, Monday to Sunday. The attendance matrix has its own period, chosen in its card, which defaults to the current month. The Anomalies tab also defaults to the current week.
  * A *school day* is a day on which any student was present.
  * A student is *active* if they were present on one of the last 3 days of the period (ending today at most) and are not archived; otherwise they have *left*. "Active now" on the Students card is that count.
  * *Attendance* is days present / school days, and the dashboard shows the average over students.
  * *Arrival* is the start of the first visit of the day.
  * *Load by hour* is the average number of students in school during each hour, over school days. It uses the merged visit time, without the daily rounding and cap.
  * The header shows the optional `school_name` setting.
* **New admin UI tabs.** Besides the dashboard, the new UI has Students (with the student page and the link slips), Anomalies and Journal (admins only, as before). They keep the old pages' layout and actions. Actions post to the same endpoints as the old UI; a `return` field brings the browser back to the page it came from, and only same-site `/admin/...` paths are accepted there. Every list is paged on the server: the dashboard table (30 rows by default), the attendance matrix (10), the students list (30; sortable by name, group or Telegram status) and the journal (30, paged in SQL). The page size can be changed or set to "all". The matrix and the table page separately: an htmx request for either list returns both, so every link keeps the other list's settings.
* **New anomaly categories** (new UI; the old UI keeps its own). All look at visits checked in during the chosen period, by default the current week. Manual visits added by staff are left out, since they have no check-in and no pings.
  * *Low activity:* the agent pings every 60 s with the time since the last input, and a ping with less than 90 s of idle time is active. The moments of activity are the check-in, the last input before each active ping, and the end of the visit. A visit is flagged when the median gap between them is at least `anomaly_low_activity_gap` (10 min). Visits under 30 min, or with no pings (pruned or manual), are not judged.
  * *Long:* the visit is longer than `anomaly_long_visit` (6 h).
  * *Late scan:* the check-in falls between `anomaly_late_from` and `anomaly_late_to` (21:00–05:00, local time). The window may cross midnight.
  * *2–3 anomalies:* visits in two or three of the lists above.
