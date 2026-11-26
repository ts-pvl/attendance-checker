# QR presence check-in

**English** · [Русский](README.ru.md) · [Қазақша](README.kk.md)

Attendance for a school with shared Linux computers. Students check in by
scanning a QR code on the computer screen with their phone. Staff see who is
in the room and how many hours each student attended.

## How it looks for a student

1. The student logs in to a school computer. The browser opens a page with a QR code.
2. The student scans the code with the phone camera. Telegram opens the school's bot.
3. The bot answers: "Checked in at PC-17 ✅".
4. When the student logs out, or the computer is idle for 30 minutes, the
   visit ends by itself. The bot sends a message with the time.

The code changes every 30 seconds and works once, so a photo of the screen
cannot be reused.

## What staff get

A web page (the admin UI), open only inside the school network:

* who is checked in right now and on which computer;
* hours of every student per day, week and month, with export to a spreadsheet (CSV);
* charts of attendance, an attendance table by day, and a list of unusual visits;
* corrections with a reason, and a log of every change.

## What you need

| | Requirement |
|---|---|
| Student computers | Ubuntu (or another Debian-based Linux) with the GNOME desktop. Each has a fixed IP address (a DHCP reservation is fine). |
| Server | One Ubuntu computer that is always on, in the same network, with a fixed IP address and internet access. The administrator's PC is fine. A separate machine is not needed. |
| Access | From the server you can connect to every student computer with SSH and run `sudo` there. The installer uses Ansible for this and installs it if needed. |
| Telegram | A bot for the school (free, made in two minutes with @BotFather). Every student has Telegram on their phone. |

The bot and the web pages are in Russian or English (you choose during
installation). The documentation is also in Kazakh.

## Installation

Do this on the server computer, as the user that has SSH access to the
student computers.

**1. Create the Telegram bot.** In Telegram, open @BotFather, send `/newbot`
and follow the questions. Keep the token it gives you (it looks like
`123456789:AAE…`). Treat it like a password.

**2. List the student computers** in an Ansible inventory file. If you
already manage the computers with Ansible, use your own inventory. Otherwise
create `~/ansible/hosts.yml`:

```yaml
students:
  hosts:
    pc-01: { ansible_host: 192.168.1.101 }
    pc-02: { ansible_host: 192.168.1.102 }
    pc-03: { ansible_host: 192.168.1.103 }
  vars:
    ansible_user: admin        # the user you log in with over SSH
```

The name on the left (shown as `PC-01`) is what students see in the bot. The
SSH user must be able to run `sudo` without a password prompt, or add
`ansible_become_password` as the Ansible documentation describes.

**3. Download and run the installer.**

```bash
sudo apt install git
git clone https://github.com/ts-pvl/attendance-checker.git
cd attendance-checker
./deploy/install.sh
```

The installer asks a few questions:

| Question | What to answer |
|---|---|
| Inventory file | The file from step 2. |
| This PC's IP address | The server's address in the school network. It must not change later. |
| Bot token | The token from step 1 (typing is hidden). |
| Time zone, language | For example `Asia/Almaty`, `ru`. |
| Alert ids | Optional. Telegram ids of staff who get a message when the system is down. Leave empty for now. |
| Username and password | Your first account for the admin UI. |
| Deploy to the PCs now? | `y`. To try one computer first, run `./deploy/install.sh --limit pc-01`. |

At the end it prints the address of the admin UI, for example
`http://192.168.1.10:8080/admin`.

**4. Add the students.** Open the admin UI in a browser on the server or on a
staff computer (not on a student computer: those are refused). Log in, then:

1. *Students → Import or update from CSV.* The file needs the columns `name` and
   `group`, for example:

   ```csv
   name,group
   Aigerim Sadykova,Python-1
   Timur Akhmetov,Go-2
   ```

2. *Create links for everyone not yet linked*, then print the page with slips.
3. Give each student their own slip. The student opens the link (or scans
   the QR on the slip) once, and the bot answers "Linked as …".

**5. Try it.** Log in to a student computer, scan the QR code, and watch the
student appear on the *Today* page.

## Updating

```bash
cd attendance-checker
git pull
./deploy/install.sh
```

It remembers your earlier answers. `./deploy/install.sh --pcs-only` deploys
only to the student computers (for example after reinstalling one), and
`./deploy/install.sh --help` lists all options.

## Everyday use

* [Staff runbook](docs/staff-runbook.md): the morning check, corrections,
  lost phones, accounts for teachers, backups, restart.
* [Instructions for students](docs/student-instructions.en.md): one page to
  print or send.

## Common questions

**The admin page says "forbidden".** It opens only from the school network,
and never from a student computer. Open it on the server or a staff computer.

**A student computer was off during installation.** The installer lists such
computers at the end, with the command to run later.

**The QR page does not open on a computer.** Check that the computer's IP
address is the one in the inventory, then run `./deploy/install.sh --pcs-only`.

**A student has no phone today.** A staff member with the admin role adds the
visit by hand on the student's page, with a reason.

**What is stored about students?** Name, group, Telegram id, arrival and
departure times, and staff corrections. Nothing about what they do on the
computer. All data stays on your server.

**Does it work without the internet?** The server needs to reach Telegram.
The student computers only need to reach the server.

## For developers

How it works inside, how to run it locally and how to build it:
[docs/development.md](docs/development.md). Design choices:
[docs/decisions.md](docs/decisions.md).
