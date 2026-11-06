// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// Parsing and evaluation for a declaration's optional effectiveDate
// (calendar-day window) and effectiveTime (day-of-week + time-of-day
// windows). Both are parsed once, at config load time (see config.go's
// compile step), into the typed forms here; Active(now) is the only thing
// evaluated per call. Dates/times are interpreted in a per-declaration
// timezone (config.go's RawDeclaration.Timezone), defaulting to UTC
// when unset. Time ranges don't cross midnight (endTime must be after
// startTime) - not needed by the spec's examples.
package guardrailcfg

import (
	"fmt"
	"strings"
	"time"
)

const (
	dateLayout = "2006-01-02"
	timeLayout = "15:04"
)

var weekdayNames = map[string]time.Weekday{
	"sunday":    time.Sunday,
	"monday":    time.Monday,
	"tuesday":   time.Tuesday,
	"wednesday": time.Wednesday,
	"thursday":  time.Thursday,
	"friday":    time.Friday,
	"saturday":  time.Saturday,
}

// effectiveDate gates a declaration to a calendar-day window. At least one
// of start/end is set (enforced by parseEffectiveDate); a nil field means
// that side of the window is open-ended, covering the spec's three cases
// ("starts", "ends", "range") with one struct. start/end are themselves
// absolute instants (midnight in the declaration's configured timezone),
// so Active's comparisons against now need no further timezone handling -
// time.Time comparisons are timezone-agnostic once both sides are
// well-formed instants.
type effectiveDate struct {
	start *time.Time // midnight in the declaration's timezone, inclusive
	end   *time.Time // midnight of the last active day, inclusive
}

// parseEffectiveDate parses start/end (each "YYYY-MM-DD", or nil for
// open-ended) into an effectiveDate, interpreting them in loc. At least
// one of start/end must be non-nil.
func parseEffectiveDate(start, end *string, loc *time.Location) (*effectiveDate, error) {
	if start == nil && end == nil {
		return nil, fmt.Errorf("effectiveDate must set at least one of start or end")
	}
	ed := &effectiveDate{}
	if start != nil {
		t, err := time.ParseInLocation(dateLayout, *start, loc)
		if err != nil {
			return nil, fmt.Errorf("invalid effectiveDate.start %q: %w", *start, err)
		}
		ed.start = &t
	}
	if end != nil {
		t, err := time.ParseInLocation(dateLayout, *end, loc)
		if err != nil {
			return nil, fmt.Errorf("invalid effectiveDate.end %q: %w", *end, err)
		}
		ed.end = &t
	}
	if ed.start != nil && ed.end != nil && ed.end.Before(*ed.start) {
		return nil, fmt.Errorf("effectiveDate.end %q is before start %q", *end, *start)
	}
	return ed, nil
}

// Active reports whether now falls within the date window.
func (d *effectiveDate) Active(now time.Time) bool {
	if d.start != nil && now.Before(*d.start) {
		return false
	}
	if d.end != nil {
		// AddDate operates on d.end's own Location's calendar, so this
		// correctly lands on "the day after" in that timezone even across
		// a DST transition (see time.Time.AddDate's documented behavior).
		endExclusive := d.end.AddDate(0, 0, 1)
		if !now.Before(endExclusive) {
			return false
		}
	}
	return true
}

// effectiveTimeWindow is a single day(s)+time-range entry. An empty days
// set means "every day" per the spec ("Only time range means every day").
// Unlike effectiveDate, evaluating a time-of-day/weekday window genuinely
// requires converting now into loc first (weekday and clock time are
// representation-dependent, not absolute-instant comparisons).
type effectiveTimeWindow struct {
	days             map[time.Weekday]bool // empty/nil = every day
	startMin, endMin int                   // minutes since midnight, in loc
	loc              *time.Location
}

// parseEffectiveTimeWindow parses one window's days (weekday names, may be
// empty) and "HH:MM" start/end times, to be interpreted in loc at
// evaluation time.
func parseEffectiveTimeWindow(days []string, startTime, endTime string, loc *time.Location) (*effectiveTimeWindow, error) {
	w := &effectiveTimeWindow{loc: loc}
	if len(days) > 0 {
		w.days = make(map[time.Weekday]bool, len(days))
		for _, d := range days {
			wd, ok := weekdayNames[strings.ToLower(strings.TrimSpace(d))]
			if !ok {
				return nil, fmt.Errorf("invalid weekday %q", d)
			}
			w.days[wd] = true
		}
	}
	startMin, err := parseTimeOfDay(startTime)
	if err != nil {
		return nil, fmt.Errorf("invalid effectiveTime.startTime %q: %w", startTime, err)
	}
	endMin, err := parseTimeOfDay(endTime)
	if err != nil {
		return nil, fmt.Errorf("invalid effectiveTime.endTime %q: %w", endTime, err)
	}
	if endMin <= startMin {
		return nil, fmt.Errorf("effectiveTime.endTime %q must be after startTime %q", endTime, startTime)
	}
	w.startMin, w.endMin = startMin, endMin
	return w, nil
}

// parseTimeOfDay just extracts the "HH:MM" wall-clock minutes-since-
// midnight value - the string itself carries no timezone, so no location
// is needed to parse it (only to later interpret a concrete instant
// against it, see active below).
func parseTimeOfDay(s string) (int, error) {
	t, err := time.ParseInLocation(timeLayout, s, time.UTC)
	if err != nil {
		return 0, err
	}
	return t.Hour()*60 + t.Minute(), nil
}

// active reports whether now falls within this single window, once
// converted into the window's timezone.
func (w *effectiveTimeWindow) active(now time.Time) bool {
	local := now.In(w.loc)
	if len(w.days) > 0 && !w.days[local.Weekday()] {
		return false
	}
	minutes := local.Hour()*60 + local.Minute()
	return minutes >= w.startMin && minutes < w.endMin
}

// effectiveTimeWindows is a declaration's full effectiveTime list; windows
// are OR'd together.
type effectiveTimeWindows []*effectiveTimeWindow

// Active reports whether now falls within any window (an empty list means
// no effectiveTime was configured, so callers should treat that as
// "always active" rather than calling Active at all - see evaluate.go).
func (ws effectiveTimeWindows) Active(now time.Time) bool {
	for _, w := range ws {
		if w.active(now) {
			return true
		}
	}
	return false
}
