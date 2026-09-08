package appserver

import (
	"encoding/json"
	"strings"
	"time"
)

// Timestamp accepts what the server actually sends.
//
// The published schema types resetsAt as a string; the running server sends a
// number. Rather than pick a side, this accepts both — a client that refuses
// the answer it is given is no better than one that misreads it. Numbers are
// interpreted by magnitude, which is unambiguous: seconds since 1970 passed
// 10^9 in 2001 and will not reach 10^11 until the year 5138.
type Timestamp struct {
	Time  time.Time
	Valid bool
}

// UnmarshalJSON accepts a string, a number, or null.
func (t *Timestamp) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `""` {
		return nil
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"} {
			if parsed, err := time.Parse(layout, str); err == nil {
				t.Time, t.Valid = parsed, true
				return nil
			}
		}
		return nil
	}
	var n float64
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	if n <= 0 {
		return nil
	}
	t.Time, t.Valid = epochToTime(n), true
	return nil
}

func epochToTime(n float64) time.Time {
	switch {
	case n > 1e17:
		return time.Unix(0, int64(n))
	case n > 1e14:
		return time.UnixMicro(int64(n))
	case n > 1e11:
		return time.UnixMilli(int64(n))
	default:
		return time.Unix(int64(n), 0)
	}
}
