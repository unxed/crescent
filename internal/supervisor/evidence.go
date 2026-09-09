package supervisor

import "time"

// EvidenceMaxAge is how long a proof stays good. Past this the lamp goes red on
// its own, with nobody updating anything.
const EvidenceMaxAge = 10 * time.Second

// Evidence is what crescent has actually established about the work, each item
// with the moment it was established.
//
// The lamp is derived from this and never stored, so it fails safe: the way a
// train's air brake stops the train when the hose leaks, a supervisor that has
// stopped gathering evidence — hung, crashed, deadlocked — turns the lamp red
// without anyone noticing the failure first. Green must be earned every few
// seconds; silence is a stop signal, not an assumption of health.
type Evidence struct {
	// At is when this evidence was gathered. The zero value is stale, so a
	// Status nobody has filled in is red by construction.
	At time.Time

	// ServerAlive: a JSON-RPC round trip to app-server completed. This proves
	// both that the process lives and that we can still talk to it — a dead
	// collector is indistinguishable from dead work, so both must be excluded.
	ServerAlive bool
	// LimitKnown: the account's usage was read successfully. Not knowing is
	// not permission.
	LimitKnown bool
	// LimitOK: the account may work right now.
	LimitOK bool
	// Online: the last limits read reached the backend rather than failing on
	// the network. Only meaningful once a read was attempted.
	Online bool
	// LimitChecked: a limits read has been attempted at all. Distinguishes
	// "not asked yet" from "asked and failed" — reporting the first as the
	// second was itself a small lie.
	LimitChecked bool
	// GoalsMoving: at least one pinned goal is actually advancing.
	GoalsMoving bool
	// Pinned: whether any goal is selected. With none, there is nothing to
	// prove about movement and saying "no goal is moving" would mislead.
	Pinned bool
}

// Fresh reports whether the evidence is recent enough to be trusted.
func (e Evidence) Fresh(now time.Time) bool {
	return !e.At.IsZero() && now.Sub(e.At) <= EvidenceMaxAge
}

// Lamp derives the traffic light from the evidence at a given moment.
//
// Red by default, in the strict sense: every path that has not proved
// something returns red or yellow, and green is reachable only when every
// condition below has been established within EvidenceMaxAge.
func (e Evidence) Lamp(now time.Time) (Lamp, string) {
	switch {
	case !e.Fresh(now):
		if e.At.IsZero() {
			return LampRed, "нет данных о работе — ещё ничего не проверено"
		}
		return LampRed, "данные о работе устарели (" +
			now.Sub(e.At).Round(time.Second).String() + ") — сбор информации остановился"
	case !e.ServerAlive:
		return LampRed, "нет связи с app-server"
	case !e.Pinned:
		return LampYellow, "ни одна цель не отмечена"
	case !e.LimitChecked:
		return LampYellow, "лимит аккаунта ещё не проверялся"
	case !e.Online:
		return LampYellow, "нет связи с сервером OpenAI"
	case !e.LimitKnown:
		return LampYellow, "лимит аккаунта не удалось прочитать"
	case !e.LimitOK:
		return LampYellow, "лимит аккаунта исчерпан"
	case !e.GoalsMoving:
		return LampYellow, "ни одна отмеченная цель не движется"
	}
	return LampGreen, "работа идёт"
}
