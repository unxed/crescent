#!/bin/bash
# Дымовой тест окна: запускает настоящее приложение под Xvfb с подставным
# app-server и проверяет то, что ломалось в жизни.
#
# Два факта об окружении, добытые дорого и записанные здесь, чтобы не добывать
# их снова:
#
#  * openbox без --sm-disable не поднимается (жалуется на session manager), а
#    без оконного менеджера, объявляющего _NET_ACTIVE_WINDOW, xdotool не может
#    ни найти окно, ни передать ему фокус;
#  * всё взаимодействие обязано происходить в одном запуске скрипта: фоновые
#    процессы не переживают его конец.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
tmp="$(mktemp -d)"
trap 'kill $(jobs -p) 2>/dev/null || true; rm -rf "$tmp"' EXIT

export DISPLAY=:97
export XDG_CACHE_HOME="$tmp/cache"
mkdir -p "$XDG_CACHE_HOME/crescent"

fail() { echo "ПРОВАЛ: $*" >&2; exit 1; }

# ---- подставной app-server, говорящий в формах схемы ----------------------
cat > "$tmp/app-server.sh" <<'MOCK'
#!/bin/sh
N=1000
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  R=$(( $(date +%s) + 3600 ))
  case "$line" in
    *'"method":"initialize"'*) echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{}}";;
    *'"method":"initialized"'*) ;;
    *rateLimits/read*)
      echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"rateLimits\":{\"primary\":{\"usedPercent\":42,\"windowDurationMins\":300,\"resetsAt\":$R}},\"rateLimitsByLimitId\":{\"codex-fable\":{\"limitName\":\"Fable\",\"primary\":{\"usedPercent\":73,\"windowDurationMins\":10080,\"resetsAt\":$R}}}}}";;
    *thread/list*)
      echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"data\":[{\"id\":\"t1\",\"name\":\"Konsole\",\"status\":{\"type\":\"notLoaded\"},\"updatedAt\":200},{\"id\":\"t2\",\"name\":\"Форк\",\"status\":{\"type\":\"notLoaded\"},\"updatedAt\":100},{\"id\":\"t3\",\"name\":\"Форк\",\"status\":{\"type\":\"notLoaded\"},\"updatedAt\":150}],\"nextCursor\":null}}";;
    *goal/get*)
      case "$line" in
        *t1*) N=$((N+250)); echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"goal\":{\"objective\":\"довести Konsole\",\"status\":\"active\",\"tokensUsed\":$N}}}";;
        *)    echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"goal\":{\"objective\":\"одна и та же цель\",\"status\":\"paused\"}}}";;
      esac;;
    *turn/interrupt*) echo "ПРЕРЫВАНИЕ ПОЛУЧЕНО" >&2; echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{}}";;
    *) echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{}}";;
  esac
done
MOCK
chmod +x "$tmp/app-server.sh"
printf '#!/bin/sh\ncase "$1" in app-server) exec %s/app-server.sh;; esac\n' "$tmp" > "$tmp/codex"
chmod +x "$tmp/codex"
export CRESCENT_CODEX="$tmp/codex"

echo '{"t1":"Konsole"}' > "$XDG_CACHE_HOME/crescent/pins.json"

# ---- экран ----------------------------------------------------------------
Xvfb "$DISPLAY" -screen 0 1024x768x24 >"$tmp/xvfb.log" 2>&1 &
sleep 3
openbox --sm-disable >"$tmp/openbox.log" 2>&1 &
sleep 2

go build -o "$tmp/crescent" ./cmd/crescent

"$tmp/crescent" -gui -foreground >"$tmp/app.log" 2>&1 &
sleep 12

wid=$(xdotool search --name crescent 2>/dev/null | tail -1 || true)
[ -n "$wid" ] || { echo "--- вывод приложения ---"; cat "$tmp/app.log"; fail "окно не появилось"; }
xdotool windowactivate --sync "$wid"
echo "окно $wid открыто и в фокусе"

log="$XDG_CACHE_HOME/crescent/journal/all.log"
[ -f "$log" ] || fail "журнал не создан"

# ---- что должно быть на экране и в журнале --------------------------------
# Дедупликация форков проверяется модульным тестом: в журнал попадают только
# закреплённые цели, поэтому здесь такая проверка не смогла бы упасть, а
# проверка, неспособная упасть, хуже отсутствующей.

# Каждая строка журнала обязана иметь дату: без неё вчерашняя запись читается
# как сегодняшняя.
if grep -avE '^[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}' "$log" | grep -q .; then
    fail "в журнале есть строки без даты"
fi

# Esc обязан прятать окно в трей, не закрывая приложение.
xdotool key --clearmodifiers Escape
sleep 2
if xwininfo -id "$wid" 2>/dev/null | grep -q IsViewable; then
    fail "Esc не свернул окно"
fi
kill -0 %3 2>/dev/null || pgrep -f "$tmp/crescent" >/dev/null || fail "приложение вышло по Esc вместо сворачивания"
grep -aq "свёрнуто в трей" "$log" || fail "сворачивание не отмечено в журнале"

echo "ДЫМОВОЙ ТЕСТ ОКНА ПРОЙДЕН"
