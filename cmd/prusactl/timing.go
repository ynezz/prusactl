package main

import (
	"fmt"
	"time"

	"github.com/trevin-lee/prusactl/internal/jobtime"
)

// jobTiming says how long a print has run, how long is left and when it should
// end on the local clock, e.g. ", 23m left (ends ~12:41), 2h10m elapsed". Both
// routes name the fields the same way (time_remaining, time_printing, in
// seconds). A value that is missing, negative or not a number is left out, and
// it is "" when the job reports neither.
func jobTiming(job map[string]any, now time.Time) string {
	t := jobtime.Read(job, now)
	out := ""
	if t.Remaining != nil {
		out += ", " + shortDuration(*t.Remaining) + " left"
		if !t.Ends.IsZero() {
			out += " (ends ~" + t.Ends.Local().Format("15:04") + ")"
		}
	}
	if t.Elapsed != nil {
		out += ", " + shortDuration(*t.Elapsed) + " elapsed"
	}
	return out
}

// shortDuration writes 45s, 23m, 2h10m or 1d3h.
func shortDuration(d time.Duration) string {
	s := int64(d / time.Second)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm", s/60)
	case s < 86400:
		if m := s % 3600 / 60; m > 0 {
			return fmt.Sprintf("%dh%02dm", s/3600, m)
		}
		return fmt.Sprintf("%dh", s/3600)
	}
	return fmt.Sprintf("%dd%dh", s/86400, s%86400/3600)
}
