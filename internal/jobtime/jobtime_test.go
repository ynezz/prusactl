package jobtime

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRead(t *testing.T) {
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		job              string
		left, done       time.Duration
		hasLeft, hasDone bool
		ends             time.Time
	}{
		"both":      {`{"time_remaining":1380,"time_printing":7800}`, 23 * time.Minute, 130 * time.Minute, true, true, at.Add(23 * time.Minute)},
		"strings":   {`{"time_remaining":"1380","time_printing":"7800"}`, 23 * time.Minute, 130 * time.Minute, true, true, at.Add(23 * time.Minute)},
		"fraction":  {`{"time_remaining":59.9}`, 59 * time.Second, 0, true, false, at.Add(59 * time.Second)},
		"missing":   {`{"progress":5}`, 0, 0, false, false, time.Time{}},
		"negative":  {`{"time_remaining":-1,"time_printing":-5}`, 0, 0, false, false, time.Time{}},
		"junk":      {`{"time_remaining":"soon","time_printing":null}`, 0, 0, false, false, time.Time{}},
		"huge":      {`{"time_remaining":4294967295}`, 0, 0, false, false, time.Time{}},
		"zero left": {`{"time_remaining":0,"time_printing":59}`, 0, 59 * time.Second, true, true, time.Time{}},
	}
	for name, c := range cases {
		var job map[string]any
		dec := json.NewDecoder(strings.NewReader(c.job))
		dec.UseNumber()
		if err := dec.Decode(&job); err != nil {
			t.Fatal(err)
		}
		got := Read(job, at)
		if (got.Remaining != nil) != c.hasLeft || (got.Remaining != nil && *got.Remaining != c.left) ||
			(got.Elapsed != nil) != c.hasDone || (got.Elapsed != nil && *got.Elapsed != c.done) || !got.Ends.Equal(c.ends) {
			t.Errorf("%s: got %+v", name, got)
		}
	}
}
