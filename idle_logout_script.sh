#!/usr/bin/env bash
# Автоматический logout при бездействии 30 минут.
# Запускается через /etc/xdg/autostart/idle-logout.desktop при каждом логине.
# Поллит idle time через GNOME Mutter IdleMonitor (работает и на Wayland и на Xorg).
#
# При 80% таймаута показывает предупреждение через notify-send.
# При 100% — принудительный logout.

set -u

IDLE_LIMIT_MS=1800000   # 30 минут
WARNING_MS=1500000      # предупреждение за 5 минут (25 минут idle)
CHECK_INTERVAL=30       # проверять каждые 30 секунд
LOG=/tmp/idle_logout.log

exec >> "$LOG" 2>&1
echo ""
echo "[$(date)] idle_logout started"
echo "  USER=${USER:-?}  LIMIT=${IDLE_LIMIT_MS}ms  WARNING=${WARNING_MS}ms"

warned=0

while true; do
  sleep "$CHECK_INTERVAL"

  idle_ms=$(gdbus call --session \
    --dest org.gnome.Mutter.IdleMonitor \
    --object-path /org/gnome/Mutter/IdleMonitor/Core \
    --method org.gnome.Mutter.IdleMonitor.GetIdletime 2>/dev/null \
    | awk '{print $2}' | tr -cd '0-9') || true

  if [[ -z "$idle_ms" ]]; then
    continue
  fi

  if [[ "$idle_ms" -lt "$WARNING_MS" ]]; then
    warned=0
    continue
  fi

  if [[ "$idle_ms" -ge "$WARNING_MS" ]] && [[ "$idle_ms" -lt "$IDLE_LIMIT_MS" ]] && [[ "$warned" -eq 0 ]]; then
    remaining=$(( (IDLE_LIMIT_MS - idle_ms) / 1000 ))
    echo "[$(date)] idle ${idle_ms}ms — warning (${remaining}s left)"
    notify-send -u critical -i dialog-warning -t 30000 \
      "Внимание!" \
      "Компьютер выключится через ${remaining} сек из-за неактивности. Подвигайте мышь или нажмите клавишу." 2>/dev/null || true
    warned=1
  fi

  if [[ "$idle_ms" -ge "$IDLE_LIMIT_MS" ]]; then
    echo "[$(date)] idle ${idle_ms}ms >= ${IDLE_LIMIT_MS}ms — forcing logout"
    gnome-session-quit --logout --no-prompt 2>/dev/null \
      || loginctl terminate-session "${XDG_SESSION_ID:-}" 2>/dev/null \
      || pkill -KILL -u "$USER"
    exit 0
  fi
done
