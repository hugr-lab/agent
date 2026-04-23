package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a compiled cron expression. 5 fields: minute, hour,
// day-of-month, month, day-of-week. Each field is either "*" (wildcard)
// or a single integer — enough for the only cron this agent needs:
// daily consolidation at a fixed hour (e.g. "0 3 * * *").
//
// Richer syntax (ranges, step values, comma lists) is out of scope per
// constitution §V ("Cron expression parser (~100 LOC, no robfig/cron
// runtime needed)"). If the feature grows that much, revisit via ADR.
type Schedule struct {
	minute     int // -1 = any
	hour       int
	dayOfMonth int
	month      int
	dayOfWeek  int
}

// Parse compiles a cron expression into a Schedule. Accepts 5
// whitespace-separated fields, each either "*" or a decimal integer.
// Returns an error for any other shape.
func Parse(expr string) (Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return Schedule{}, fmt.Errorf("scheduler: cron needs 5 fields, got %d: %q", len(fields), expr)
	}
	vals := [5]int{-1, -1, -1, -1, -1}
	for i, f := range fields {
		if f == "*" {
			continue
		}
		n, err := strconv.Atoi(f)
		if err != nil {
			return Schedule{}, fmt.Errorf("scheduler: cron field %d: %q: %w", i, f, err)
		}
		vals[i] = n
	}
	s := Schedule{vals[0], vals[1], vals[2], vals[3], vals[4]}
	if err := s.validate(); err != nil {
		return Schedule{}, err
	}
	return s, nil
}

func (s Schedule) validate() error {
	check := func(name string, v, lo, hi int) error {
		if v == -1 {
			return nil
		}
		if v < lo || v > hi {
			return fmt.Errorf("scheduler: cron %s out of range [%d,%d]: %d", name, lo, hi, v)
		}
		return nil
	}
	if err := check("minute", s.minute, 0, 59); err != nil {
		return err
	}
	if err := check("hour", s.hour, 0, 23); err != nil {
		return err
	}
	if err := check("day-of-month", s.dayOfMonth, 1, 31); err != nil {
		return err
	}
	if err := check("month", s.month, 1, 12); err != nil {
		return err
	}
	if err := check("day-of-week", s.dayOfWeek, 0, 6); err != nil {
		return err
	}
	return nil
}

// Next returns the next time at or after `from` when the schedule
// fires. Matches each field independently; step size is 1 minute.
//
// The implementation walks forward one minute at a time until every
// non-wildcard field matches. For the only caller today (daily
// consolidation) the loop terminates within at most one day's worth
// of minutes; that is fine for a background scheduler.
func (s Schedule) Next(from time.Time) time.Time {
	t := from.Add(time.Minute).Truncate(time.Minute)
	// Walk up to 8 days ahead — an absolute guard against pathological
	// schedules that never match.
	limit := t.Add(8 * 24 * time.Hour)
	for ; t.Before(limit); t = t.Add(time.Minute) {
		if s.matches(t) {
			return t
		}
	}
	return time.Time{}
}

func (s Schedule) matches(t time.Time) bool {
	if s.minute != -1 && t.Minute() != s.minute {
		return false
	}
	if s.hour != -1 && t.Hour() != s.hour {
		return false
	}
	if s.dayOfMonth != -1 && t.Day() != s.dayOfMonth {
		return false
	}
	if s.month != -1 && int(t.Month()) != s.month {
		return false
	}
	if s.dayOfWeek != -1 && int(t.Weekday()) != s.dayOfWeek {
		return false
	}
	return true
}
