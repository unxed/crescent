package supervisor

import (
	"context"
	"testing"
	"time"

	"github.com/unxed/crescent/internal/pins"
)

// "Waiting until <resetsAt>" is only what the window shows. It must not be a
// memory the loop consults: once the limit reads as open, the state is
// working and no stale wait remains.
func TestLimitedWaitDoesNotOutliveTheLimit(t *testing.T) {
	s := &Supervisor{pins: &pins.Pins{}, status: Status{State: StateIdle}}

	s.set(StateLimited, "", time.Now().Add(3*time.Hour))
	if s.status.State != StateLimited || s.status.Until.IsZero() {
		t.Fatal("ожидание лимита не выставлено")
	}
	s.set(StateWorking, "", time.Time{})
	if s.status.State != StateWorking || !s.status.Until.IsZero() {
		t.Errorf("после сброса осталось «ждём до»: %v %v", s.status.State, s.status.Until)
	}
}

// The pause between passes is the poll interval and nothing else: a limit that
// announced a reset hours away must not stretch it, or an early reset would go
// unnoticed until the announced time.
func TestSleepIgnoresAnnouncedResetTime(t *testing.T) {
	s := &Supervisor{
		pins:   &pins.Pins{},
		poll:   10 * time.Millisecond,
		wake:   make(chan struct{}, 1),
		status: Status{State: StateLimited, Until: time.Now().Add(3 * time.Hour)},
	}
	start := time.Now()
	if !s.sleep(context.Background()) {
		t.Fatal("сон прерван без причины")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("сон длился %v при интервале опроса 10 мс", d)
	}
}
