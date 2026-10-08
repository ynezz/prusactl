package server

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/trevin-lee/prusactl/internal/jobtime"
)

// Reading the printer means reading whatever JSON the route returned: the
// printer calls its state status.printer.state, Prusa Connect calls it
// status.connect_state and files the temperatures elsewhere again. summarize
// lifts the handful of facts every caller wants into the same place on both
// routes. It only adds fields: the route's own reply is still there in full,
// so nothing the printer reports is hidden.

// summary is the same few facts however they were fetched.
type summary struct {
	State   string    `json:"state,omitempty"`
	Nozzle  *reading  `json:"nozzle,omitempty"`
	Bed     *reading  `json:"bed,omitempty"`
	Chamber *reading  `json:"chamber,omitempty"`
	Job     *jobBrief `json:"job,omitempty"`
}

type reading struct {
	Actual float64  `json:"actual"`
	Target *float64 `json:"target,omitempty"`
}

type jobBrief struct {
	Name          string   `json:"name,omitempty"`
	Progress      *float64 `json:"progress,omitempty"`       // percent
	TimeRemaining *float64 `json:"time_remaining,omitempty"` // seconds
	TimeElapsed   *float64 `json:"time_elapsed,omitempty"`   // seconds, the printer's time_printing
	EndsAt        string   `json:"ends_at,omitempty"`        // estimated finish, RFC 3339, local clock
}

// timing fills the time fields from a job object, working the finish out from
// now. See jobtime for what is left out.
func (b *jobBrief) timing(job map[string]any, now time.Time) {
	t := jobtime.Read(job, now)
	if t.Remaining != nil {
		b.TimeRemaining = secs(*t.Remaining)
	}
	if t.Elapsed != nil {
		b.TimeElapsed = secs(*t.Elapsed)
	}
	if !t.Ends.IsZero() {
		b.EndsAt = t.Ends.Format(time.RFC3339)
	}
}

func secs(d time.Duration) *float64 {
	f := d.Seconds()
	return &f
}

// num reads a JSON number from a nested path, e.g. num(m, "temp", "temp_bed").
func num(m map[string]any, path ...string) *float64 {
	cur := any(m)
	for _, k := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = obj[k]
	}
	switch v := cur.(type) {
	case float64:
		return &v
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return &f
		}
	}
	return nil
}

func str(m map[string]any, path ...string) string {
	cur := any(m)
	for _, k := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = obj[k]
	}
	s, _ := cur.(string)
	return s
}

func temp(actual, target *float64) *reading {
	if actual == nil {
		return nil
	}
	return &reading{Actual: *actual, Target: target}
}

// summarizeDirect reads PrusaLink's /api/v1/status and /api/v1/job.
func summarizeDirect(status, job map[string]any, now time.Time) *summary {
	s := &summary{State: str(status, "printer", "state")}
	s.Nozzle = temp(num(status, "printer", "temp_nozzle"), num(status, "printer", "target_nozzle"))
	s.Bed = temp(num(status, "printer", "temp_bed"), num(status, "printer", "target_bed"))
	if len(job) > 0 {
		b := &jobBrief{Progress: num(job, "progress")}
		b.timing(job, now)
		if b.Name = str(job, "file", "display_name"); b.Name == "" {
			b.Name = str(job, "file", "name")
		}
		if b.Name != "" || b.Progress != nil || b.TimeRemaining != nil {
			s.Job = b
		}
	}
	return s
}

// summarizeConnect reads a Prusa Connect printer record.
func summarizeConnect(p map[string]any, now time.Time) *summary {
	s := &summary{State: stateOf(p)}
	s.Nozzle = temp(num(p, "temp", "temp_nozzle"), num(p, "temp", "target_nozzle"))
	s.Bed = temp(num(p, "temp", "temp_bed"), num(p, "temp", "target_bed"))
	s.Chamber = temp(num(p, "chamber", "temp"), num(p, "chamber", "target_temp"))
	if job, ok := p["job_info"].(map[string]any); ok && len(job) > 0 {
		b := &jobBrief{Progress: num(job, "progress")}
		b.timing(job, now)
		if b.Name = str(job, "display_name"); b.Name == "" {
			b.Name = strings.TrimPrefix(str(job, "path"), "/")
		}
		if b.Name != "" || b.Progress != nil || b.TimeRemaining != nil {
			s.Job = b
		}
	}
	return s
}

// decode turns a raw JSON object into a map, using json.Number so large ids
// keep their digits.
func decode(raw json.RawMessage) map[string]any {
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if dec.Decode(&m) != nil {
		return nil
	}
	return m
}
