// Package jobtime reads a print's timing from a job object, which both routes
// name the same way (time_remaining, time_printing, in seconds).
package jobtime

import (
	"encoding/json"
	"strconv"
	"time"
)

// Timing is how long a print has run, how long is left and when it should end.
// A value that is missing, negative or not a number is nil; Ends is the zero
// time unless some time is left.
type Timing struct {
	Remaining, Elapsed *time.Duration
	Ends               time.Time
}

// Read takes the timing from job, working the finish out from now.
func Read(job map[string]any, now time.Time) Timing {
	t := Timing{Remaining: seconds(job["time_remaining"]), Elapsed: seconds(job["time_printing"])}
	if t.Remaining != nil && *t.Remaining > 0 {
		t.Ends = now.Add(*t.Remaining)
	}
	return t
}

func seconds(v any) *time.Duration {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case json.Number:
		var err error
		if f, err = n.Float64(); err != nil {
			return nil
		}
	case string:
		var err error
		if f, err = strconv.ParseFloat(n, 64); err != nil {
			return nil
		}
	default:
		return nil
	}
	// Firmware sends huge placeholders for "unknown"; a year is past believing.
	if f != f || f < 0 || f > 366*24*3600 {
		return nil
	}
	d := time.Duration(f) * time.Second
	return &d
}
