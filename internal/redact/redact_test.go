package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

var secrets = []string{"abc123", "def456", "ghi789", "cam-token"}

const doc = `{"name":"Core One","api_key":"abc123","prusalink_api_key":"def456","prusaconnect_api_key":"ghi789",
	"id":9007199254740993,"cameras":[{"token":"cam-token","name":"Buddy3D"}],"password":""}`

func leaks(t *testing.T, out string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("%q leaked: %s", s, out)
		}
	}
}

func TestJSON(t *testing.T) {
	out, changed := JSON([]byte(doc))
	if !changed {
		t.Fatal("reported nothing masked")
	}
	leaks(t, string(out))
	var got map[string]any
	dec := json.NewDecoder(strings.NewReader(string(out)))
	dec.UseNumber()
	if err := dec.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["name"] != "Core One" || got["password"] != "" {
		t.Errorf("other fields changed: %v", got)
	}
	if got["id"].(json.Number).String() != "9007199254740993" {
		t.Errorf("large number lost precision: %v", got["id"])
	}
}

func TestJSONLeavesCleanAndNonJSONAlone(t *testing.T) {
	for _, in := range []string{`{"state":"IDLE","temp":24.5}`, `not json`, `{"a":1} {"b":2}`, ``} {
		out, changed := JSON([]byte(in))
		if changed || string(out) != in {
			t.Errorf("JSON(%q) = %q, %v; want it unchanged", in, out, changed)
		}
	}
}

func TestTextCoversTruncatedAndEmbeddedJSON(t *testing.T) {
	truncated := doc[:strings.Index(doc, `"name":"Buddy3D"`)] // cut mid-document
	leaks(t, Text(truncated))
	leaks(t, Text("Prusa Connect: GET /app/printers/x -> 500: "+doc))
	leaks(t, Text(`{"api_key": "abc123`)) // cut inside the value

	clean := `printer: GET /api/v1/job -> 409 Conflict: {"title":"busy"}`
	if Text(clean) != clean {
		t.Errorf("clean text changed: %s", Text(clean))
	}
}

// A camera address can carry a password; the host and path stay readable. The
// exact output is compared, since a pattern that stops at the first @ leaves
// the tail of the password behind and still hides "hunter2" as a whole word.
func TestTextMasksCameraPassword(t *testing.T) {
	for in, want := range map[string]string{
		"couldn't open rtsp://cam:hunter2@192.168.1.5/live: refused":       "couldn't open rtsp://[redacted]@192.168.1.5/live: refused",
		"couldn't open rtsp://cam:hun@ter2@192.168.1.5/live: refused":      "couldn't open rtsp://[redacted]@192.168.1.5/live: refused",
		"bad rtsp://cam:pa\nss@192.168.1.5/live":                           "bad rtsp://[redacted]@192.168.1.5/live",
		"bad rtsp://cam:pa\r\nss@192.168.1.5/live":                         "bad rtsp://[redacted]@192.168.1.5/live",
		"bad rtsp://cam:my secret@192.168.1.5/live":                        "bad rtsp://[redacted]@192.168.1.5/live",
		`{"url":"rtsp://cam:hunter2@192.168.1.5/live","token":"x"}`:        `{"token":"[redacted]","url":"rtsp://[redacted]@192.168.1.5/live"}`,
		`{"url":"rtsp://cam:hun@ter2@192.168.1.5/live","e":"a@b.example"}`: `{"url":"rtsp://[redacted]@192.168.1.5/live","e":"a@b.example"}`,
		`{"url":"rtsp://cam:pa\"ss@192.168.1.5/live"}`:                     `{"url":"rtsp://[redacted]@192.168.1.5/live"}`,
	} {
		if got := Text(in); got != want {
			t.Errorf("Text(%q)\n got %q\nwant %q", in, got, want)
		}
	}
	if got := Text("rtsp://192.168.1.5/live"); got != "rtsp://192.168.1.5/live" {
		t.Errorf("an address without a password changed: %q", got)
	}
}
