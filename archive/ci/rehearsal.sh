#!/bin/sh
# Full daemon rehearsal without a Codex account.
#
# The event schema this mock speaks is not invented: it follows
# codex-rs/exec/src/exec_events.rs, and the usage-limit wording follows
# codex-rs/protocol/src/error.rs. That is what makes the rehearsal worth
# running — it fails when crescent stops understanding what Codex actually
# emits, not when it stops understanding a fiction.
set -eu

root="$(cd "$(dirname "$0")/../.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

export CODEX_HOME="$tmp/codex"
export XDG_CACHE_HOME="$tmp/cache"
workspace="$tmp/workspace"
mkdir -p "$CODEX_HOME/sessions/2026/08/29" "$workspace" "$tmp/bin"

# A session with a goal, its working directory alive, last touched long enough
# ago that the daemon will not mistake it for a human at the keyboard.
sid="01a04f59-3ac3-7790-9f25-a0bd6c10ca2b"
printf '{"id":"%s","thread_name":"Konsole"}\n' "$sid" > "$CODEX_HOME/session_index.jsonl"
rollout="$CODEX_HOME/sessions/2026/08/29/rollout-a-$sid.jsonl"
cat > "$rollout" <<EOF
{"type":"session_meta","cwd":"$workspace"}
{"type":"goal","payload":{"goal":{"objective":"довести Konsole до запуска","status":"active"}}}
EOF
touch -d '2026-09-06 10:00:00' "$rollout" 2>/dev/null || touch -t 202609061000 "$rollout"

export MOCK_COUNTER="$tmp/counter"
export CRESCENT_CODEX="$tmp/bin/codex"

cd "$root"
# The mock is a Go program shared with the Windows rehearsal: Windows cannot
# spawn a .cmd directly, and that is the platform this exercise exists to cover.
go build -o "$tmp/bin/codex" ./archive/ci/mockcodex
go build -o "$tmp/crescent" ./archive/cmd/crescent

echo "--- doctor ---"
"$tmp/crescent" -doctor

echo "--- список целей ---"
"$tmp/crescent" -list | tee "$tmp/list.txt"
grep -q "Konsole" "$tmp/list.txt" || { echo "ОШИБКА: имя чата из индекса не подхвачено"; exit 1; }

echo "--- один ход под присмотром ---"
"$tmp/crescent" -run-once -read-only -yes | tee "$tmp/once.txt"
grep -q "turn.completed" "$tmp/once.txt" || { echo "ОШИБКА: поток событий не разобран"; exit 1; }

echo "--- демон: самопроверка, ходы, лимит, ожидание ---"
echo 0 > "$MOCK_COUNTER"
"$tmp/crescent" -run > "$tmp/run.log" 2>&1 &
daemon=$!
ok=0
for _ in $(seq 1 60); do
  sleep 1
  if "$tmp/crescent" -status 2>/dev/null | grep -q "ждёт сброса лимита"; then ok=1; break; fi
done
kill "$daemon" 2>/dev/null || true
wait "$daemon" 2>/dev/null || true

echo "--- журнал демона ---"
cat "$tmp/run.log"
[ "$ok" = 1 ] || { echo "ОШИБКА: демон не дошёл до ожидания лимита"; exit 1; }
grep -q "самопроверка пройдена" "$tmp/run.log" || { echo "ОШИБКА: самопроверка не прошла"; exit 1; }

echo "РЕПЕТИЦИЯ ПРОЙДЕНА"
