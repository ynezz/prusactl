package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestJobTiming(t *testing.T) {
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.Local)
	want := ", 23m left (ends ~12:23), 2h10m elapsed"
	cases := map[string]struct {
		job  string
		want string
	}{
		"both":      {`{"time_remaining":1380,"time_printing":7800}`, want},
		"missing":   {`{"progress":5}`, ""},
		"zero left": {`{"time_remaining":0,"time_printing":59}`, ", 0s left, 59s elapsed"},
		"only left": {`{"time_remaining":90000}`, ", 1d1h left (ends ~13:00)"},
	}
	for name, c := range cases {
		var job map[string]any
		dec := json.NewDecoder(strings.NewReader(c.job))
		dec.UseNumber()
		if err := dec.Decode(&job); err != nil {
			t.Fatal(err)
		}
		if got := jobTiming(job, at); got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
}
