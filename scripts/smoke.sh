#!/usr/bin/env bash
# End-to-end smoke test: build the real binary, set it up the way an admin
# would (key, hosts, staff user, students), start the server and use it like
# a lab PC and a staff browser. Fails on the first thing that does not work.
#
#   make smoke            # or: scripts/smoke.sh [path/to/attendance]
#
# Everything lives in a temporary directory that is removed at the end.
# Invented data only; no Telegram (the bot is off).
set -euo pipefail

cd "$(dirname "$0")/.."
BIN="${1:-}"
DIR=$(mktemp -d)
SERVER=""
cleanup() {
  [[ -n "$SERVER" ]] && kill "$SERVER" 2>/dev/null && wait "$SERVER" 2>/dev/null
  rm -rf "$DIR"
}
trap cleanup EXIT

if [[ -z "$BIN" ]]; then
  BIN="$DIR/attendance"
  CGO_ENABLED=0 go build -o "$BIN" ./cmd/attendance
fi

PORT="${PORT:-18080}"
BASE="http://127.0.0.1:$PORT"
CONF="$DIR/attendance.conf"
JAR="$DIR/cookies"
PASSWORD="smoke-test-password-1"
step() { printf '  %-58s' "$1"; }
ok() { echo "ok"; }
fail() { echo "FAIL"; echo "    $*" >&2; [[ -f "$DIR/server.log" ]] && tail -20 "$DIR/server.log" >&2; exit 1; }

echo "smoke test in $DIR"

step "set up: key, hosts, config, staff user, students"
"$BIN" gen-key > "$DIR/host_key"
printf 'name,ip\nPC-01,127.0.0.1\nPC-02,192.0.2.2\n' > "$DIR/hosts.csv"
cat > "$CONF" <<EOF
listen = 127.0.0.1:$PORT
db = $DIR/attendance.db
bot_username = SmokeTestBot
host_key_file = $DIR/host_key
hosts_file = $DIR/hosts.csv
token_secret_file = $DIR/token_secret
backup_dir = $DIR/backups
admin_networks = 127.0.0.0/8
admin_from_lab_pcs = true
EOF
"$BIN" check-config -config "$CONF" > /dev/null || fail "check-config"
printf '%s\n' "$PASSWORD" | "$BIN" user add admin -role admin -config "$CONF" > /dev/null 2>&1 || fail "user add"
"$BIN" students import deploy/students.csv.example -config "$CONF" > /dev/null || fail "students import"
"$BIN" students list -config "$CONF" | grep -q "Petrova Anna" || fail "students list"
ok

step "start the server"
"$BIN" serve -no-bot -config "$CONF" > "$DIR/server.log" 2>&1 &
SERVER=$!
for _ in $(seq 50); do
  curl -fsS -o /dev/null "$BASE/healthz" 2>/dev/null && break
  kill -0 "$SERVER" 2>/dev/null || fail "the server exited"
  sleep 0.2
done
curl -fsS "$BASE/healthz" | grep -q '"ok":true' || fail "/healthz is not ok"
ok

# A lab PC: the key goes to curl on stdin, never on the command line.
pc() { printf 'header = "X-Host-Key: %s"\n' "$(cat "$DIR/host_key")" | curl -fsS -o /dev/null --config - -d sid=smoke-1 "$@" "$BASE/event"; }

step "a lab PC logs in and sends a heartbeat"
pc -d kind=session_start || fail "session_start refused"
pc -d kind=heartbeat -d idle_ms=0 || fail "heartbeat refused"
code=$(curl -s -o /dev/null -w '%{http_code}' -H 'X-Host-Key: wrong' -d sid=x -d kind=heartbeat "$BASE/event")
[[ "$code" == 401 ]] || fail "a wrong host key got $code, want 401"
ok

step "the PC's QR page shows a check-in code"
curl -fsS "$BASE/qr" | grep -q "PC-01" || fail "/qr"
curl -fsS "$BASE/qr/state" | grep -q '"link":"https://t.me/SmokeTestBot?start=' || fail "/qr/state has no code"
ok

step "staff log in"
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/admin/today")
[[ "$code" == 303 ]] || fail "admin page without login: $code, want 303"
loc=$(curl -s -c "$JAR" -o /dev/null -w '%{redirect_url}' --data-urlencode username=admin --data-urlencode "password=$PASSWORD" "$BASE/admin/login")
[[ "$loc" == "$BASE/admin/dashboard" ]] || fail "login went to '$loc'"
ok

get() { curl -fsS -b "$JAR" "$BASE$1"; }

step "every tab opens"
for p in /admin/dashboard /admin/today /admin/hours /admin/students /admin/students/1 /admin/students/slips \
         /admin/anomalies /admin/help /admin/platform /admin/journal; do
  get "$p" > "$DIR/page.html" || fail "GET $p"
  grep -q 'class="tabs"' "$DIR/page.html" || fail "$p has no tabs"
done
get /admin/today | grep -q 'data-host="PC-01"' || fail "Today has no PC-01 tile"
ok

step "staff add a student, find them and export the list"
csrf=$(get /admin/students | sed -n 's/.*name="csrf" value="\([^"]*\)".*/\1/p' | head -1)
[[ -n "$csrf" ]] || fail "no CSRF token"
code=$(curl -s -b "$JAR" -o /dev/null -w '%{http_code}' --data-urlencode "csrf=$csrf" --data-urlencode "name=Smoke Testova" \
  --data-urlencode group=QA --data-urlencode login=stestova --data-urlencode return=/admin/students "$BASE/admin/students/add")
[[ "$code" == 303 ]] || fail "add student: $code"
get "/admin/students?q=testova" | grep -q '>Smoke Testova</a>' || fail "search does not find the new student"
get "/admin/students?q=testova&tg=linked" | grep -q '>Smoke Testova</a>' && fail "Telegram filter: not linked yet"
get "/admin/students.csv?q=testova" | grep -q '^Smoke Testova,QA,stestova,$' || fail "students CSV"
get "/admin/students.csv" | head -1 | grep -q 'name,group,login,telegram' || fail "students CSV header"
get "/admin/hours.csv?period=week" | head -1 | grep -q 'student_id,name,group' || fail "hours CSV"
get "/admin/journal" | grep -q 'stestova' || fail "the journal does not show the change"
ok

step "old addresses lead to the new pages"
loc=$(curl -s -b "$JAR" -o /dev/null -w '%{redirect_url}' "$BASE/admin/new/students?group=QA")
[[ "$loc" == "$BASE/admin/students?group=QA" ]] || fail "/admin/new/students went to '$loc'"
ok

step "stop the server, rebuild visits from events, back up"
kill "$SERVER"; wait "$SERVER" 2>/dev/null || true; SERVER=""
"$BIN" replay -config "$CONF" | grep -q '^replayed [1-9]' || fail "replay"
"$BIN" backup "$DIR/backup.db" -config "$CONF" > /dev/null || fail "backup"
[[ -s "$DIR/backup.db" ]] || fail "empty backup"
ok

echo "smoke test passed"
