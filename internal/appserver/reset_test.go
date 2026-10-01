package appserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func win(used float64, mins int64, at time.Time) *RateLimitWindow {
	return &RateLimitWindow{UsedPercent: used, WindowDurationMins: mins,
		ResetsAt: Timestamp{Time: at, Valid: true}}
}

func yes() *bool { v := true; return &v }
func no() *bool  { v := false; return &v }

// The limit is sometimes reset ahead of the announced time, or with no notice
// at all. Exhausted keeps no memory, so the same resetsAt that was a wait a
// moment ago must read as open once the usage has dropped.
func TestEarlyResetIsSeenOnTheNextSnapshot(t *testing.T) {
	at := time.Now().Add(3 * time.Hour)

	spent := RateLimits{Primary: win(100, 300, at), Secondary: win(40, 10080, at.Add(100*time.Hour))}
	limited, until := spent.Exhausted()
	if !limited || !until.Equal(at) {
		t.Fatalf("исчерпанное окно: limited=%v until=%v", limited, until)
	}

	// Same resetsAt, still three hours away, but the window is empty again.
	reset := RateLimits{Primary: win(0, 300, at), Secondary: win(40, 10080, at.Add(100*time.Hour))}
	if limited, _ := reset.Exhausted(); limited {
		t.Error("ранний сброс не замечен: окно пустое, а цели всё ещё ждут resetsAt")
	}
}

func TestBackendAllowanceBeatsPercentages(t *testing.T) {
	at := time.Now().Add(time.Hour)

	// "Allowed" wins over a window that looks full.
	full := RateLimits{Primary: win(100, 300, at), Secondary: win(100, 10080, at), OrdinaryUsageAllowed: yes()}
	if limited, _ := full.Exhausted(); limited {
		t.Error("явное ordinaryUsageAllowed=true проигнорировано из-за 100%")
	}

	// "Not allowed" wins over a window that looks empty, and keeps the time.
	empty := RateLimits{Primary: win(0, 300, at), OrdinaryUsageAllowed: no()}
	limited, until := empty.Exhausted()
	if !limited || !until.Equal(at) {
		t.Errorf("явный запрет при 0%%: limited=%v until=%v", limited, until)
	}

	// No verdict: arithmetic decides, at the threshold.
	if limited, _ := (RateLimits{Primary: win(99.5, 300, at)}).Exhausted(); !limited {
		t.Error("99.5% должно считаться исчерпанным")
	}
	if limited, _ := (RateLimits{Primary: win(99.4, 300, at)}).Exhausted(); limited {
		t.Error("99.4% не должно считаться исчерпанным")
	}
}

func TestEitherWindowExhaustsAndNearestResetWins(t *testing.T) {
	soon := time.Now().Add(time.Hour)
	late := time.Now().Add(100 * time.Hour)

	weekly := RateLimits{Primary: win(10, 300, soon), Secondary: win(100, 10080, late)}
	limited, until := weekly.Exhausted()
	if !limited || !until.Equal(late) {
		t.Errorf("исчерпано недельное окно: limited=%v until=%v", limited, until)
	}

	both := RateLimits{Primary: win(100, 300, soon), Secondary: win(100, 10080, late)}
	if _, until := both.Exhausted(); !until.Equal(soon) {
		t.Errorf("ожидалось ближайшее время сброса, got %v", until)
	}

	// Spent but the server named no time: still limited, time unknown.
	blind := RateLimits{Primary: &RateLimitWindow{UsedPercent: 100}}
	if limited, until := blind.Exhausted(); !limited || !until.IsZero() {
		t.Errorf("без resetsAt: limited=%v until=%v", limited, until)
	}

	if limited, _ := (RateLimits{}).Exhausted(); limited {
		t.Error("ответ без окон принят за исчерпанный лимит")
	}
}

// A model-specific bucket running dry does not stop the ordinary allowance,
// which is what pinned goals spend; only the account-level windows count.
func TestModelBucketDoesNotLimitTheAccount(t *testing.T) {
	at := time.Now().Add(time.Hour)
	limits := RateLimits{
		Primary: win(5, 300, at),
		Buckets: []RateLimits{{LimitID: "codex-fable", Primary: win(100, 10080, at)}},
	}
	if limited, _ := limits.Exhausted(); limited {
		t.Error("исчерпанная корзина модели остановила обычное использование")
	}
}

// A fake codex app-server: this test binary run again with app-server as its
// first argument. It answers account/rateLimits/read from a file the test
// rewrites between calls, so the real Client is exercised end to end.
func TestMain(m *testing.M) {
	if os.Getenv("CRESCENT_FAKE_CODEX") == "1" {
		fakeCodex()
		return
	}
	os.Exit(m.Run())
}

func fakeCodex() {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var req struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil || req.ID == nil {
			continue // a notification: no answer
		}
		result := "{}"
		if req.Method == "account/rateLimits/read" {
			b, err := os.ReadFile(os.Getenv("CRESCENT_FAKE_LIMITS"))
			if err != nil {
				fmt.Printf(`{"id":%d,"error":{"code":-1,"message":%q}}`+"\n", *req.ID, err.Error())
				continue
			}
			result = string(b)
		}
		fmt.Printf(`{"id":%d,"result":%s}`+"\n", *req.ID, result)
	}
}

// End to end through the real client: limit spent, then reset early with the
// very same resetsAt, then spent again. Every read must reflect the server's
// current answer; nothing may be cached until the announced time.
func TestClientSeesSpontaneousResetBeforeResetsAt(t *testing.T) {
	file := filepath.Join(t.TempDir(), "limits.json")
	t.Setenv("CRESCENT_FAKE_CODEX", "1")
	t.Setenv("CRESCENT_FAKE_LIMITS", file)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := Dial(ctx, Options{CodexPath: os.Args[0]})
	if err != nil {
		t.Fatal(err)
	}
	// Not c.Close(): it calls cmd.Wait concurrently with readLoop's own Wait,
	// which -race reports. Cancelling ctx kills the fake server instead.
	_ = c

	at := time.Now().Add(3 * time.Hour).Unix()
	body := func(used int, allowed string) string {
		return fmt.Sprintf(`{"rateLimits":{"primary":{"usedPercent":%d,"windowDurationMins":300,"resetsAt":%d}}%s}`,
			used, at, allowed)
	}
	steps := []struct {
		name    string
		body    string
		limited bool
	}{
		{"исчерпан", body(100, `,"ordinaryUsageAllowed":false`), true},
		{"сброшен раньше времени, вердикт сервера", body(0, `,"ordinaryUsageAllowed":true`), false},
		{"исчерпан снова", body(100, ""), true},
		{"сброшен раньше времени, без вердикта", body(0, ""), false},
		{"сервер разрешает при полном окне", body(100, `,"ordinaryUsageAllowed":true`), false},
	}
	for _, st := range steps {
		if err := os.WriteFile(file, []byte(st.body), 0o600); err != nil {
			t.Fatal(err)
		}
		limits, err := c.RateLimitsWithRetry(ctx, 1)
		if err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		if limited, _ := limits.Exhausted(); limited != st.limited {
			t.Errorf("%s: limited=%v, ожидалось %v", st.name, limited, st.limited)
		}
	}
}
