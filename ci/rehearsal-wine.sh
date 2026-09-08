#!/bin/sh
# Run the Windows build through the full rehearsal, on Linux, using Wine.
#
# Why bother when CI already has a Windows runner: because a round trip to CI —
# or to a spare laptop — is a slow way to find out that a path separator is
# wrong. Wine catches that class of bug in seconds, locally. It found exactly
# one on its first run: the Go cache paths were being joined with forward
# slashes and following the Unix layout, so `workspace_write.writable_roots`
# contained "C:\users\me/.cache/go-build".
#
# Wine is not Windows. It is a fast first filter, not a replacement for the
# Windows job in CI, and definitely not for the machine the thing will run on.
set -eu

command -v wine64 >/dev/null 2>&1 && WINE=wine64
[ -z "${WINE:-}" ] && [ -x /usr/lib/wine/wine64 ] && WINE=/usr/lib/wine/wine64
[ -z "${WINE:-}" ] && command -v wine >/dev/null 2>&1 && WINE=wine
if [ -z "${WINE:-}" ]; then
    echo "Wine не найден — пропускаю (apt-get install wine64)"
    exit 0
fi

root="$(cd "$(dirname "$0")/.." && pwd)"
tmp="$(mktemp -d)"

export WINEPREFIX="$tmp/prefix"
export WINEDEBUG=-all

# wineserver outlives the last process that used the prefix and keeps files
# open in it, so removing the directory races it and fails. Shutting it down
# first is the documented way; and cleanup must never decide the exit status,
# which is what turned a passed rehearsal into a failed job.
cleanup() {
    status=$?
    "${WINESERVER:-wineserver}" -k 2>/dev/null || /usr/lib/wine/wineserver -k 2>/dev/null || true
    sleep 1
    rm -rf "$tmp" 2>/dev/null || true
    exit "$status"
}
trap cleanup EXIT

cd "$root"
mkdir -p "$tmp/bin"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o "$tmp/bin/codex.exe" ./ci/mockcodex
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o "$tmp/bin/crescent.exe" ./cmd/crescent

# Everything Wine sees lives under Z:, which is the root of the host filesystem.
winpath() { printf 'Z:%s' "$(echo "$1" | tr '/' '\\')"; }

sessions="$tmp/codex/sessions/2026/08/29"
mkdir -p "$sessions" "$tmp/workspace" "$tmp/cache"
sid="01a04f59-3ac3-7790-9f25-a0bd6c10ca2b"
printf '{"id":"%s","thread_name":"Konsole"}\n' "$sid" > "$tmp/codex/session_index.jsonl"

rollout="$sessions/rollout-a-$sid.jsonl"
ws_json="$(winpath "$tmp/workspace" | sed 's|\\|\\\\|g')"
cat > "$rollout" <<EOF
{"type":"session_meta","cwd":"$ws_json"}
{"type":"goal","payload":{"goal":{"objective":"довести Konsole до запуска","status":"active"}}}
EOF
touch -d '2026-09-06 10:00:00' "$rollout" 2>/dev/null || touch -t 202609061000 "$rollout"

export CODEX_HOME="$(winpath "$tmp/codex")"
export LOCALAPPDATA="$(winpath "$tmp/cache")"
export USERPROFILE="$(winpath "$tmp")"
export CRESCENT_CODEX="$(winpath "$tmp/bin/codex.exe")"
export MOCK_COUNTER="$tmp/counter"

run() { "$WINE" "$tmp/bin/crescent.exe" "$@" 2>&1 | grep -v '^wine:' || true; }

# The unit tests themselves, compiled for Windows and run here. This is the
# cheapest place to catch a platform assumption: the same failures the Windows
# job reports minutes later show up in seconds.
echo "--- тесты как Windows-бинарники ---"
for pkg in ./internal/codex ./internal/runner; do
    out="$tmp/bin/$(basename "$pkg")_test.exe"
    CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go test -c -o "$out" "$pkg"
    "$WINE" "$out" 2>&1 | grep -v '^wine:' | tail -5
done

echo "--- doctor (Windows-бинарник) ---"
run -doctor | tee "$tmp/doctor.txt"
# The bug Wine found: a path that mixes separators is not a path.
if grep -qE 'C:\\[^"]*/' "$tmp/doctor.txt"; then
    echo "ОШИБКА: путь со смешанными разделителями"; exit 1
fi

echo "--- список целей ---"
run -list | tee "$tmp/list.txt"
grep -q "Konsole" "$tmp/list.txt" || { echo "ОШИБКА: имя чата не подхвачено"; exit 1; }

echo "--- один ход ---"
echo 0 > "$MOCK_COUNTER"
run -run-once -read-only -yes | tee "$tmp/once.txt"
grep -q "turn.completed" "$tmp/once.txt" || { echo "ОШИБКА: поток событий не разобран"; exit 1; }

echo "--- демон до упирания в лимит ---"
echo 0 > "$MOCK_COUNTER"
"$WINE" "$tmp/bin/crescent.exe" -run > "$tmp/run.log" 2>&1 &
daemon=$!
ok=0
for _ in $(seq 1 40); do
    sleep 2
    if run -status | grep -q "лимит"; then ok=1; break; fi
done
kill "$daemon" 2>/dev/null || true
wait "$daemon" 2>/dev/null || true

grep -v '^wine:' "$tmp/run.log" || true
[ "$ok" = 1 ] || { echo "ОШИБКА: демон не дошёл до ожидания лимита"; exit 1; }

echo "РЕПЕТИЦИЯ НА WINE ПРОЙДЕНА"
