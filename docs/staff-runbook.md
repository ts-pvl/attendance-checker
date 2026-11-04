# Staff runbook

Admin UI: `http://<server>:8080/admin` (school LAN only, not from the lab PCs).

## Morning check (30 seconds)

Open **Today** and look at the four cards at the top:

| Card | Fine | Investigate |
|---|---|---|
| Last heartbeat | minutes ago (during opening hours) | hours ago while students were in the lab |
| Last check-in | today | nothing since yesterday although students came |
| Bot last reached Telegram | under a minute ago | red error, or "never" |
| Open visits | matches who is in the room | — |

In the PC table, yellow rows are PCs with a session but no check-in. Remind those students to scan.

If something looks wrong, check whether the service is running (see *Restart*).
Enter visits missed during an outage as corrections (below).

## Common tasks

**Import or update the student list.** Students → *Import or update from CSV*.
Columns: `name`, `group`, optional `id` (Russian headers `имя`/`ФИО`, `группа` work too).
Comma or semicolon separated. Existing students are matched by `id`, otherwise by exact name.
Leavers: open the student and press *Archive* (history stays).

**Enrollment day.** Students → *Create links for everyone not yet linked* → print the slips page.
Hand each slip to its student privately: whoever opens a link first gets that identity.
Watch the Telegram column turn green; the summary line counts who is left.

**Lost phone / new Telegram account / linked to the wrong person.** Open the
student → *Unlink* (enter a reason) → *New link* → give the new slip privately.

**Correct a visit or add a missed one** (admins). Open the student, find the
visit → *Edit* or *Delete*, or use *Add a missed visit* at the bottom. A
reason is always required. The original record stays and the change appears
under *Corrections* and in the Audit log.

**Hours.** Hours → choose day / week / month and a group → *Download CSV* for
the same numbers in a spreadsheet. Visits that cross midnight are split
between the two days.

**Anomalies** (weekly). Slow or old-code scans can mean a photo or a streamed
screen. *One student on several PCs* can mean a lent Telegram account. Several
students on one PC is allowed for pair programming but worth a look.
*Auto-closed* visits were still open at 04:00, so check their end time. *Failed
scans* shows rejected attempts.

## Staff accounts (on the server)

```bash
sudo -u attendance attendance user add maria -role teacher -config /etc/attendance/attendance.conf
sudo -u attendance attendance user add aidar -role admin   -config /etc/attendance/attendance.conf
sudo -u attendance attendance user passwd maria -config /etc/attendance/attendance.conf
sudo -u attendance attendance user disable maria -config /etc/attendance/attendance.conf
```

Teachers can read everything except the audit log; admins can also change data.

## Restart, logs, rollback

```bash
systemctl status attendance              # running?
sudo systemctl restart attendance        # restart server + bot (scans fail for a few seconds)
journalctl -u attendance --since today   # logs
```

Rollback after a bad update: `sudo mv /usr/local/bin/attendance.prev /usr/local/bin/attendance && sudo systemctl restart attendance`.

The service restarts by itself after a crash, and the machine should power on
after a power cut (BIOS setting). Do updates and restarts at a quiet hour.

## Backups

A copy of the database is written to `/var/backups/attendance` every night
at 03:30 and the last 14 are kept. Copy that folder to another machine regularly.
Also keep a safe copy of the bot token and the Ansible vault.

Restore: `sudo systemctl stop attendance`, copy a backup over
`/var/lib/attendance/attendance.db` (owner `attendance`), delete the
`attendance.db-wal` and `attendance.db-shm` files next to it, then start the service.

## Alerts

If `attendance_alert_chat_ids` is set in Ansible, staff get a Telegram message
when the server is unreachable, when the bot cannot reach Telegram, or when
the PCs stop reporting mid-session. The bot's `/id` command shows your Telegram id.
