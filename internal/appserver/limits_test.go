package appserver

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Limits were asked for from the first day and went unshown for weeks, so the
// shape that carries them is worth pinning: the historical single bucket plus
// rateLimitsByLimitId, where a model's own allowance lives.
func TestEveryLimitBucketIsRead(t *testing.T) {
	const body = `{
	  "rateLimits": {
	    "primary":   {"usedPercent": 42, "windowDurationMins": 300,   "resetsAt": 1788906780},
	    "secondary": {"usedPercent": 18, "windowDurationMins": 10080, "resetsAt": 1789488360}
	  },
	  "rateLimitsByLimitId": {
	    "codex-fable": {"limitName": "Fable",
	      "primary": {"usedPercent": 73, "windowDurationMins": 10080, "resetsAt": 1789488360}}
	  },
	  "ordinaryUsageAllowed": true
	}`

	var resp struct {
		RateLimits           RateLimits            `json:"rateLimits"`
		ByLimitID            map[string]RateLimits `json:"rateLimitsByLimitId"`
		OrdinaryUsageAllowed *bool                 `json:"ordinaryUsageAllowed"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	limits := resp.RateLimits
	for id, b := range resp.ByLimitID {
		if b.LimitID == "" {
			b.LimitID = id
		}
		limits.Buckets = append(limits.Buckets, b)
	}

	lines := strings.Join(limits.Summary(), " | ")
	for _, want := range []string{"5-часовое", "42%", "недельное", "18%", "Fable", "73%"} {
		if !strings.Contains(lines, want) {
			t.Errorf("в сводке нет %q:\n%s", want, lines)
		}
	}
	// A model bucket must be named, not shown as another anonymous account line.
	if strings.Count(lines, "аккаунт") != 2 {
		t.Errorf("корзина модели не названа своим именем:\n%s", lines)
	}
}

// A bucket the server did not name still has to be identifiable.
func TestBucketFallsBackToItsIdentifier(t *testing.T) {
	if got := (RateLimits{LimitID: "codex"}).Title(); got != "codex" {
		t.Errorf("Title = %q", got)
	}
	if got := (RateLimits{NormalModelSlug: "gpt-5.6-luna"}).Title(); got != "gpt-5.6-luna" {
		t.Errorf("Title = %q", got)
	}
}

// A deleted or recreated chat leaves its id pinned, and every request for it
// fails the same way for ever. Recognising that is what lets crescent stop
// asking instead of writing one identical line every few seconds — a live log
// carried hundreds of them.
func TestVanishedThreadIsRecognised(t *testing.T) {
	gone := []error{
		errors.New("thread/goal/get: thread not found: 01a04f59-3ac3-7790-9f25-a0bd6c10ca2b (code -32600)"),
		errors.New("Thread Not Found"),
		errors.New("no such thread"),
	}
	for _, err := range gone {
		if !IsThreadGone(err) {
			t.Errorf("не опознано как исчезнувший тред: %v", err)
		}
	}

	// Everything else must keep its pin: a network blip is not a deletion.
	stays := []error{
		nil,
		errors.New("failed to fetch codex rate limits"),
		errors.New("thread 01a0 already has an active writer (code -32600)"),
	}
	for _, err := range stays {
		if IsThreadGone(err) {
			t.Errorf("цель была бы снята из-за временной ошибки: %v", err)
		}
	}
}

// The response repeats the same allowance under several names: the historical
// "account" view and a "codex" bucket carry identical percentages and reset
// times. Printing both stretched the window far past everything else in it.
func TestIdenticalBucketsAreNotRepeated(t *testing.T) {
	same := &RateLimitWindow{UsedPercent: 10, WindowDurationMins: 300,
		ResetsAt: Timestamp{Time: mustTime("2026-09-09T17:36:00Z"), Valid: true}}

	limits := RateLimits{Primary: same}
	limits.Buckets = []RateLimits{
		{LimitName: "codex", Primary: same}, // те же цифры
		{LimitName: "gpt-reserve", Primary: &RateLimitWindow{UsedPercent: 0,
			WindowDurationMins: 10080,
			ResetsAt:           Timestamp{Time: mustTime("2026-09-09T12:56:00Z"), Valid: true}}},
	}

	got := limits.Summary()
	if len(got) != 2 {
		t.Fatalf("строк %d, ожидалось 2 (дубликат codex должен исчезнуть): %q", len(got), got)
	}
	joined := strings.Join(got, " | ")
	if strings.Contains(joined, "codex") {
		t.Errorf("повтор той же корзины остался: %s", joined)
	}
	if !strings.Contains(joined, "gpt-reserve") {
		t.Errorf("корзина с другими цифрами потеряна: %s", joined)
	}
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// Lines taken verbatim from a live journal. Both goals failed the same way,
// hundreds of times, always off by one: the SQLite projection believes it has
// applied a line the rollout file is offering again, so the write is refused
// and the turn is shut down from inside Codex. Restarting cannot fix it.
func TestHistoryDesyncIsRecognised(t *testing.T) {
	real := "WARN codex_thread_store::local::live_writer: failed to project durable " +
		"rollout for 01a08207-cf5e-7631-90c3-3f8ac9c49a75: thread-store internal error: " +
		"thread history projection for 01a08207-cf5e-7631-90c3-3f8ac9c49a75 " +
		"expected ordinal 4330, got 4329"

	if !IsHistoryDesynced(real) {
		t.Error("расхождение истории треда не опознано")
	}
	if got := ThreadIDIn(real); got != "01a08207-cf5e-7631-90c3-3f8ac9c49a75" {
		t.Errorf("идентификатор треда не извлечён: %q", got)
	}
	if !IsHistoryDesynced("thread history projection for X is behind durable rollout") {
		t.Error("вторая форма расхождения не опознана")
	}

	// Ordinary noise must not be mistaken for it: a goal that is merely busy or
	// briefly offline has to keep being restarted.
	for _, line := range []string{
		"WARN app_server.request{otel.name=\"thread/resume\"}",
		"ERROR failed to fetch codex rate limits",
		"thread 01a0 already has an active writer",
	} {
		if IsHistoryDesynced(line) {
			t.Errorf("обычная ошибка принята за расхождение истории: %s", line)
		}
	}
}

// A log line carries an installation_id in the same UUID shape as a thread id.
// Taking the first match created a journal file named after the installation —
// a goal log for a goal that does not exist.
func TestInstallationIDIsNotAThread(t *testing.T) {
	line := "INFO remote_control::websocket: loop exiting " +
		"remote_control_url=https://chatgpt.com/backend-api/ " +
		"installation_id=7e8fd38a-70f9-4b99-be27-4aeecc9c4d2e server_name=rcnb"
	if got := ThreadIDIn(line); got != "" {
		t.Errorf("установочный идентификатор принят за тред: %q", got)
	}

	real := "WARN session_loop{thread_id=01a08207-cf5e-7631-90c3-3f8ac9c49a75}: что-то"
	if got := ThreadIDIn(real); got != "01a08207-cf5e-7631-90c3-3f8ac9c49a75" {
		t.Errorf("настоящий тред не найден: %q", got)
	}

	proj := "thread history projection for 01a08208-2013-7922-95c2-34c9cd13f424 expected ordinal 5, got 4"
	if got := ThreadIDIn(proj); got != "01a08208-2013-7922-95c2-34c9cd13f424" {
		t.Errorf("тред из строки о проекции не найден: %q", got)
	}
}

// The model's thinking arrives as fragments — 293 in one five-minute probe run
// — and dropping them was why a working goal looked like silence. They are
// joined per turn and reported once, as a sentence.
func TestReasoningFragmentsBecomeOneThought(t *testing.T) {
	frag := func(turn, delta string) json.RawMessage {
		return json.RawMessage(`{"threadId":"t1","turnId":"` + turn + `","delta":"` + delta + `"}`)
	}

	if _, ok := Interpret("item/reasoning/summaryTextDelta", frag("turn-1", "Проверяю ")); ok {
		t.Error("отдельный фрагмент попал в журнал — их сотни на ход")
	}
	Interpret("item/reasoning/summaryTextDelta", frag("turn-1", "лог CI"))

	a, ok := Interpret("item/reasoning/summaryPartAdded",
		json.RawMessage(`{"threadId":"t1","turnId":"turn-1"}`))
	if !ok {
		t.Fatal("собранная мысль не дошла до журнала")
	}
	if a.Text != "Проверяю лог CI" {
		t.Errorf("мысль собрана неверно: %q", a.Text)
	}
	if a.Kind != "думает" {
		t.Errorf("вид записи %q", a.Kind)
	}

	// A second turn must not inherit the first one's words.
	Interpret("item/reasoning/summaryTextDelta", frag("turn-2", "Другое"))
	b, _ := Interpret("item/reasoning/summaryPartAdded",
		json.RawMessage(`{"threadId":"t1","turnId":"turn-2"}`))
	if b.Text != "Другое" {
		t.Errorf("мысли разных ходов смешались: %q", b.Text)
	}
}

// A goal that cannot be restarted is still a goal. Hiding it meant Лунобот-1
// sat blocked, waiting for a word from the user, and never appeared in the
// window — the one goal that needed a person was the one a person could not see.
func TestBlockedGoalIsStillListed(t *testing.T) {
	if Restartable("blocked") {
		t.Fatal("blocked внезапно стал перезапускаемым — тест проверяет не то")
	}
	// Restartable decides restarting; listing must not consult it. The guard in
	// Candidates now checks only that a goal exists, which is what this asserts
	// at the level a unit test can reach.
	for _, st := range []string{"blocked", "complete", "cancelled"} {
		if Restartable(st) {
			t.Errorf("%q не должен перезапускаться", st)
		}
	}
	for _, st := range []string{"paused", "usageLimited", "idle", "active"} {
		if !Restartable(st) {
			t.Errorf("%q должен перезапускаться", st)
		}
	}
}

// Each kind of approval answers in its own shape, and a wrong shape is a
// rejected response — that is, a turn that waits for ever.
func TestApprovalAnswersMatchTheirSchemas(t *testing.T) {
	cases := []struct{ method, wantKey string }{
		{"item/commandExecution/requestApproval", "decision"},
		{"item/fileChange/requestApproval", "decision"},
		{"item/permissions/requestApproval", "scope"},
		{"item/tool/requestUserInput", "response"},
	}
	for _, c := range cases {
		if !IsApprovalRequest(c.method) {
			t.Errorf("%s не опознан как запрос разрешения", c.method)
			continue
		}
		got, ok := ApprovalAnswer(c.method, nil).(map[string]any)
		if !ok || got[c.wantKey] == nil {
			t.Errorf("%s: ответ без поля %q: %#v", c.method, c.wantKey, got)
		}
	}

	// Anything else is not ours to answer.
	if IsApprovalRequest("account/chatgptAuthTokens/refresh") {
		t.Error("обновление токенов принято за запрос разрешения")
	}
}

// The journal has to say what was allowed, or automatic approval becomes an
// invisible hand.
func TestApprovalSummaryNamesTheCommand(t *testing.T) {
	got := ApprovalSummary("item/commandExecution/requestApproval",
		[]byte(`{"command":["git","push","origin","main"],"reason":"push в репозиторий"}`))
	for _, want := range []string{"git push origin main", "push в репозиторий"} {
		if !strings.Contains(got, want) {
			t.Errorf("в описании нет %q: %s", want, got)
		}
	}
}
