package server

import (
	"encoding/json"
	"testing"
	"time"
)

// The point of summary is that one reading of the answer works whichever route
// replied, so the two routes are checked against the same expectations.

func TestSummaryReadsBothRoutesTheSameWay(t *testing.T) {
	status := decode(json.RawMessage(`{"printer":{"state":"PRINTING","temp_nozzle":215.4,"target_nozzle":215,"temp_bed":60.1,"target_bed":60}}`))
	job := decode(json.RawMessage(`{"progress":42.5,"time_remaining":1800,"file":{"name":"PART~1.BGC","display_name":"part.bgcode"}}`))
	direct := summarizeDirect(status, job, time.Now())

	connect := summarizeConnect(decode(json.RawMessage(`{
		"connect_state":"PRINTING",
		"temp":{"temp_nozzle":215.4,"target_nozzle":215,"temp_bed":60.1,"target_bed":60},
		"job_info":{"progress":42.5,"time_remaining":1800,"display_name":"part.bgcode","path":"/usb/PART~1.BGC"}
	}`)), time.Now())

	for name, s := range map[string]*summary{"direct": direct, "connect": connect} {
		if s.State != "PRINTING" {
			t.Errorf("%s state = %q", name, s.State)
		}
		if s.Nozzle == nil || s.Nozzle.Actual != 215.4 || s.Nozzle.Target == nil || *s.Nozzle.Target != 215 {
			t.Errorf("%s nozzle = %+v", name, s.Nozzle)
		}
		if s.Bed == nil || s.Bed.Actual != 60.1 {
			t.Errorf("%s bed = %+v", name, s.Bed)
		}
		if s.Job == nil || s.Job.Name != "part.bgcode" {
			t.Errorf("%s job = %+v", name, s.Job)
		}
		if s.Job == nil || s.Job.Progress == nil || *s.Job.Progress != 42.5 {
			t.Errorf("%s progress = %+v", name, s.Job)
		}
	}
}

// PrusaLink has no chamber field at all, so its absence is the printer's limit
// and not something to invent a zero for.
func TestSummaryLeavesOutWhatTheRouteDoesNotReport(t *testing.T) {
	direct := summarizeDirect(decode(json.RawMessage(`{"printer":{"state":"IDLE","temp_nozzle":26}}`)), nil, time.Now())
	if direct.Chamber != nil {
		t.Errorf("direct invented a chamber reading: %+v", direct.Chamber)
	}
	if direct.Bed != nil {
		t.Errorf("direct invented a bed reading: %+v", direct.Bed)
	}
	if direct.Job != nil {
		t.Errorf("direct invented a job: %+v", direct.Job)
	}
	if direct.Nozzle == nil || direct.Nozzle.Target != nil {
		t.Errorf("nozzle target should stay absent, not become 0: %+v", direct.Nozzle)
	}

	conn := summarizeConnect(decode(json.RawMessage(`{"connect_state":"IDLE","chamber":{"temp":24.7}}`)), time.Now())
	if conn.Chamber == nil || conn.Chamber.Actual != 24.7 {
		t.Errorf("connect chamber = %+v", conn.Chamber)
	}
}

// A job Connect names only by path still gets a name a person can read.
func TestSummaryFallsBackToTheFileName(t *testing.T) {
	conn := summarizeConnect(decode(json.RawMessage(`{"connect_state":"PRINTING","job_info":{"path":"/usb/PART~1.BGC","progress":1}}`)), time.Now())
	if conn.Job == nil || conn.Job.Name != "usb/PART~1.BGC" {
		t.Errorf("job = %+v", conn.Job)
	}
	direct := summarizeDirect(
		decode(json.RawMessage(`{"printer":{"state":"PRINTING"}}`)),
		decode(json.RawMessage(`{"progress":1,"file":{"name":"PART~1.BGC"}}`)), time.Now())
	if direct.Job == nil || direct.Job.Name != "PART~1.BGC" {
		t.Errorf("job = %+v", direct.Job)
	}
}

// Ids larger than a float64 can hold keep their digits.
func TestDecodeKeepsLongNumbersExact(t *testing.T) {
	m := decode(json.RawMessage(`{"id":9007199254740993}`))
	if got := m["id"].(json.Number).String(); got != "9007199254740993" {
		t.Errorf("id = %s", got)
	}
}

// Both routes name the timing fields the same way; the finish is worked out
// from the remaining time, and nonsense values are left out.
func TestSummaryJobTiming(t *testing.T) {
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

	direct := summarizeDirect(decode(json.RawMessage(`{"printer":{"state":"PRINTING"}}`)),
		decode(json.RawMessage(`{"progress":89,"time_remaining":1380,"time_printing":7800,"file":{"name":"a.gcode"}}`)), at)
	conn := summarizeConnect(decode(json.RawMessage(
		`{"connect_state":"PRINTING","job_info":{"progress":89,"time_remaining":1380,"time_printing":7800,"path":"/a.gcode"}}`)), at)
	for name, s := range map[string]*summary{"direct": direct, "connect": conn} {
		j := s.Job
		if j == nil || j.TimeRemaining == nil || *j.TimeRemaining != 1380 || j.TimeElapsed == nil || *j.TimeElapsed != 7800 {
			t.Fatalf("%s job = %+v", name, j)
		}
		if j.EndsAt != "2026-10-04T12:23:00Z" {
			t.Errorf("%s ends_at = %q", name, j.EndsAt)
		}
	}

	bad := summarizeConnect(decode(json.RawMessage(
		`{"connect_state":"PRINTING","job_info":{"progress":3,"time_remaining":-1,"time_printing":-1,"path":"/a.gcode"}}`)), at)
	if j := bad.Job; j.TimeRemaining != nil || j.TimeElapsed != nil || j.EndsAt != "" {
		t.Errorf("negative times were kept: %+v", j)
	}
}
