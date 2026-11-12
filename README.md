# QR presence check-in

*[Русская версия ниже](#отметка-посещаемости-по-qr-коду)*

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
| Local development, sample data | `scripts/dev.sh`, `cmd/devseed` |
| Staff runbook, student instructions (English and Russian), decisions | `docs/` |

## Quick start

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
  change data. Staff instructions are in [docs/staff-runbook.md](docs/staff-runbook.md).

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

### From the admin PC (one script)

On the Ubuntu admin PC, as the user that normally runs Ansible for the lab:

1. Create the bot with @BotFather.
2. Download `install.sh` from the latest
   [release](https://github.com/ts-pvl/attendance-checker/releases) (you are
   logged in to GitHub in the browser), or with `gh release download -R ts-pvl/attendance-checker -p install.sh`.
3. Run it:

```bash
chmod +x install.sh && ./install.sh
```

It asks for sudo once and does the following:

* Downloads the release, using the GitHub CLI login or a read-only token you
  paste. The token is not saved. It checks the SHA256 sums.
* Installs Ansible if needed and installs the server on this PC under systemd,
  so it starts at boot.
* Generates `hosts.csv` from your existing inventory (`hosts.yml`, group
  `students`). It asks for the bot token, which it checks with Telegram, and
  creates the first admin account.
* Deploys the agent to the lab PCs after a confirmation. Use `--limit TS-A01`
  to try one PC first, `--pcs-only` to deploy only the agent, or
  `--server-only` to skip the PCs. The PCs boot with overlayroot, so the files
  are also written to the read-only lower disk, which is remounted read-write
  for the copy. The old `idle_logout` is kept as `idle_logout.orig`.

Run it again to upgrade. It remembers the answers in
`/etc/attendance/install.env`; `--reconfigure` asks again. PCs that were off
are listed with the command to retry them. Run `./install.sh --help` for all
options.

To publish a release: commit, then `make release` and
`gh release create vYYYY.MM.DD dist/attendance-linux-* dist/attendance-deploy.tar.gz dist/install.sh dist/SHA256SUMS`
(a second release on the same day gets `.1`, `.2`, …).

### By hand with Ansible

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

---

# Отметка посещаемости по QR-коду

*[English version above](#qr-presence-check-in)*

Учёт посещаемости для школы программирования с общими компьютерами на Linux.
Ученик входит в систему на ПК, браузер показывает QR-код, который меняется
каждые 30 секунд. Ученик сканирует его камерой телефона, открывается бот школы
в Telegram, и бот отвечает «Вы отметились на PC-17». Когда ученик уходит,
выход по простою (обычный выход из системы или пропавший сигнал от ПК)
закрывает посещение моментом последней активности. Сотрудники видят часы за
день, неделю и месяц в веб-интерфейсе.

Техническое задание — `QR_Presence_Check_in_System_Development,_Testing_and_Rollout_Plan.pdf`.
Решения там, где ТЗ оставляло выбор, описаны в [docs/decisions.md](docs/decisions.md) (на английском).

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

| Часть | Где |
|---|---|
| Сервер, бот, админка | `cmd/attendance`, `internal/*` (Go, стандартная библиотека + `modernc.org/sqlite` + `x/crypto/bcrypt`) |
| Агент на ПК (доработанный `idle_logout`) | `agent/idle_logout` |
| Развёртывание | `deploy/ansible` (роли `attendance_server`, `attendance_agent`), `deploy/systemd`, `deploy/alert` |
| Локальная разработка, тестовые данные | `scripts/dev.sh`, `cmd/devseed` |
| Инструкции для сотрудников и учеников (на английском и русском), решения | `docs/` |

## Быстрый старт

Нужны Go 1.26+ и `curl`. Из корня репозитория:

```bash
make dev
```

При первом запуске собирается программа и в `.dev/` создаётся локальная
установка (настройки, ключ хоста, пользователь `admin`, примерные ученики).
Добавляются и четыре недели выдуманной посещаемости на PC-01…PC-12, чтобы в
админке были данные. Данные создаёт `cmd/devseed` через обычные события, один
раз на каждую папку `.dev/`; `SEED=0 make dev` их не добавляет. Затем
запускается сервер и имитируется один учебный ПК (этот компьютер играет роль
PC-01). Откройте:

* http://127.0.0.1:8080/admin — вход: `admin`, пароль `dev-password-123`
* http://127.0.0.1:8080/qr — меняющийся QR-код для отметки

**WSL 2:** браузер в Windows подключается через адрес WSL, и запрос приходит
с адреса Windows, поэтому `make dev` распознаёт WSL и подстраивается. Сервер
слушает все интерфейсы, в админку пускает только Windows, а роль PC-01 играет
Windows. Имитация агента отправляет события через `curl.exe` из Windows.
Используйте адреса, которые печатает скрипт (`http://<адрес WSL>:8080/...`), а не `localhost`.

Чтобы сканировать и привязывать настоящим телефоном, создайте тестового бота у
@BotFather и передайте его:

```bash
BOT_TOKEN=123456:your-test-token BOT_USERNAME=YourTestBot make dev
```

`Ctrl+C` всё останавливает. `rm -rf .dev` — начать с нуля. Так же можно
задать `PORT` и `ADMIN_PASSWORD`.

## Как это работает

* **Личность** — аккаунт Telegram. Каждый ученик один раз привязывает его по
  личной одноразовой ссылке (`https://t.me/<bot>?start=<code>`), которую
  сотрудники печатают или передают лично.
* **Место** — экран ПК. В QR-коде `https://t.me/<bot>?start=<token>`. Токен
  подписан HMAC (компьютер, время выдачи, случайное число), длиной 35 символов,
  действует 60 секунд и только один раз. Страница `/qr` и `/event` отвечают
  только адресам из `hosts.csv`.
* **Сначала сырые события.** Каждое сообщение агента, скан, `/checkout` и
  ночная очистка записываются в таблицу `events`, в которую только добавляют.
  Сеансы и посещения выводятся из неё одной детерминированной функцией, а
  `attendance replay` пересобирает их с нуля.
* **Один ученик на ПК.** Если на ПК, где уже отмечен ученик, сканирует другой,
  посещение первого закрывается моментом последней активности ПК, и бот
  сообщает ему об этом.
* **Уход** — последняя активность: время получения сигнала минус переданное
  время простоя. Выход по простою не засчитывает 30 лишних минут. ПК, молчащий
  больше 3 минут, закрывается по последней активности. В 04:00 все ещё
  открытые посещения закрываются с пометкой «закрыто автоматически».
* **Часы** считаются по посещениям с учётом исправлений сотрудников.
  Исправления не меняют исходные записи. Пересечения объединяются, посещения
  делятся в полночь. Округление, минимальная длина посещения и дневной предел
  настраиваются и по умолчанию выключены. Экран «Часы», страница ученика, бот
  и выгрузка CSV используют один и тот же запрос.
* **Админка** состоит из двух интерфейсов, переключатель справа вверху.
  *Дашборд* показывает графики, матрицу посещаемости, таблицу учеников с
  сортировкой и вкладки «Ученики», «Аномалии» и «Журнал». Старая *Админка* —
  это страницы «Сегодня», «Часы», «Ученики», «Аномалии» и «Журнал».
  Преподаватели только смотрят, администраторы могут и изменять данные.
  Инструкция для сотрудников — [docs/staff-runbook.ru.md](docs/staff-runbook.ru.md).

## Сборка и тесты

Go 1.26+ (модуль закрепляет `toolchain go1.27.3`; старые версии `go` скачают его сами).

```bash
make test          # модульные, сценарные тесты, тесты безопасности и replay
make race          # то же с детектором гонок
make load          # 50 ПК × 20 сигналов + 50 сканов разом
make fuzz          # фаззинг разбора токена 60 секунд
make check         # vet, staticcheck, govulncheck, gosec, shellcheck, race
make pi            # dist/attendance-linux-arm64 (pi32 / amd64 для других платформ)
```

## Попробовать вручную

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
attendance serve -no-bot -config att.conf      # или добавьте bot_token_file = … для тестового бота
```

Откройте http://127.0.0.1:8080/admin. Чтобы на странице QR появился код,
изобразите агента ПК:

```bash
curl -H "X-Host-Key: $(cat /tmp/att/host_key)" -d kind=session_start -d sid=test-1 http://127.0.0.1:8080/event
```

затем откройте http://127.0.0.1:8080/qr.

## Развёртывание

### С компьютера администратора (один скрипт)

На компьютере администратора с Ubuntu, под тем пользователем, который обычно
запускает Ansible для класса:

1. Создайте бота у @BotFather.
2. Скачайте `install.sh` из последнего
   [выпуска](https://github.com/ts-pvl/attendance-checker/releases) (в браузере
   вы вошли в GitHub) или командой `gh release download -R ts-pvl/attendance-checker -p install.sh`.
3. Запустите:

```bash
chmod +x install.sh && ./install.sh
```

Скрипт один раз спрашивает пароль sudo и делает следующее:

* Скачивает выпуск через вход в GitHub CLI или вставленный токен только для
  чтения. Токен не сохраняется. Проверяет суммы SHA256.
* При необходимости ставит Ansible и устанавливает сервер на этот компьютер
  под systemd, чтобы он запускался при загрузке.
* Создаёт `hosts.csv` из существующего инвентаря (`hosts.yml`, группа
  `students`). Спрашивает токен бота, проверяет его в Telegram и создаёт
  первую учётную запись администратора.
* После подтверждения ставит агент на учебные ПК. `--limit TS-A01` — сначала
  попробовать на одном ПК, `--pcs-only` — только агент, `--server-only` — без
  ПК. ПК загружаются с overlayroot, поэтому файлы записываются и на нижний
  диск (только для чтения), который на время копирования перемонтируется для
  записи. Старый `idle_logout` сохраняется как `idle_logout.orig`.

Для обновления запустите скрипт ещё раз. Ответы он помнит в
`/etc/attendance/install.env`; `--reconfigure` спросит заново. Выключенные ПК
будут перечислены вместе с командой для повтора. `./install.sh --help` —
все параметры.

Выпустить версию: закоммитить, затем `make release` и
`gh release create vYYYY.MM.DD dist/attendance-linux-* dist/attendance-deploy.tar.gz dist/install.sh dist/SHA256SUMS`
(второй выпуск за день получает `.1`, `.2`, …).

### Вручную через Ansible

1. Создайте бота у @BotFather (и отдельного тестового бота для разработки).
2. Соберите: `make pi` (или `make amd64`).
3. Скопируйте `deploy/ansible/inventory.example.ini` в `inventory.ini` и
   перечислите сервер и все учебные ПК с их постоянными адресами. Скопируйте
   `group_vars/all/vault.yml.example` в `vault.yml`, впишите ключ хоста
   (`attendance gen-key`) и токен бота, затем зашифруйте `ansible-vault encrypt`.
4. `cd deploy/ansible && ansible-playbook -i inventory.ini site.yml --ask-vault-pass`
5. На сервере: `sudo -u attendance attendance user add <имя> -role admin -config /etc/attendance/attendance.conf`
6. Загрузите учеников (админка → Ученики → CSV), создайте ссылки, распечатайте листки.
7. В BIOS сервера включите запуск после пропадания питания.

Сервер работает под systemd с `Restart=always` без ограничения числа
перезапусков, поэтому сам поднимается после сбоев и отключений питания. Ночная
очистка и резервное копирование выполняются в заданное время и наверстывают
пропущенное после простоя. Необязательный таймер оповещений пишет сотрудникам
в Telegram, если сервер недоступен, бот не может связаться с Telegram или ПК
перестали присылать сигналы посреди сеанса.

## Настройки

См. `deploy/attendance.conf.example`. Секреты (токен бота, ключ хоста) — файлы,
доступные только root; systemd передаёт их службе через `LoadCredential=`.
Ключ HMAC для QR-токенов создаётся при первом запуске в `/var/lib/attendance/token_secret`.

## Командная строка

```
attendance serve [-no-bot]
attendance check-config
attendance user add|passwd|disable|enable|list
attendance students import <file.csv> | students list
attendance code <student-id>            # одноразовая ссылка для привязки
attendance unlink <student-id> -reason "…"
attendance replay                       # пересобрать посещения из событий (сначала остановите службу)
attendance backup <file>
attendance gen-key
attendance version
```

Все команды принимают `-config` (по умолчанию `$ATTENDANCE_CONFIG`, иначе `/etc/attendance/attendance.conf`).
