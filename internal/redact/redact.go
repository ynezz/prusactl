// Package redact masks credentials (PrusaLink and Connect API keys, camera
// tokens, passwords) in data headed for a terminal or a model. Every tool
// result, API error, and `prusactl api` response goes through it.
package redact

import (
	"bytes"
	"encoding/json"
	"regexp"
)

// Mask replaces a credential's value.
const Mask = "[redacted]"

// key matches the names of fields that hold credentials.
var key = regexp.MustCompile(`(?i)api_?key|token|password|secret`)

// pair finds `"name": value` in text that isn't valid JSON, such as a
// truncated response: group 1 is the name and colon, group 2 the value.
var pair = regexp.MustCompile(`(?i)("[^"]*(?:api_?key|token|password|secret)[^"]*"\s*:\s*)("(?:[^"\\]|\\.)*"?|[^,}\]\s]+)`)

// rtspUser finds the user:password in a camera address such as
// rtsp://user:password@192.168.1.5/live. The password may hold an @, so the
// userinfo runs to the last @ before the path, and it may hold spaces, a line break or a
// JSON-escaped quote, which is how a malformed address that url.Parse refuses
// shows up in an error.
var rtspUser = regexp.MustCompile(`(?is)(rtsps?://)(?:[^/"\\]|\\.)+@`)

// Value masks credential fields in a decoded JSON value, in place, and
// returns it.
func Value(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if key.MatchString(k) {
				if val != nil && val != "" {
					t[k] = Mask
				}
			} else {
				t[k] = Value(val)
			}
		}
	case []any:
		for i := range t {
			t[i] = Value(t[i])
		}
	}
	return v
}

// JSON masks credential fields in a JSON document and reports whether it
// masked anything. Numbers keep their exact digits. Input that isn't a single
// JSON document comes back unchanged; use Text for that.
func JSON(raw []byte) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil || dec.More() {
		return raw, false
	}
	before, err := json.Marshal(v)
	if err != nil {
		return raw, false
	}
	after, err := json.Marshal(Value(v))
	if err != nil || bytes.Equal(before, after) {
		return raw, false
	}
	return after, true
}

// Text masks credentials in any text: exactly, when it is a JSON document, and
// otherwise by pattern, which also covers truncated JSON and JSON inside an
// error message.
func Text(s string) string {
	if out, changed := JSON([]byte(s)); changed {
		return rtspUser.ReplaceAllString(string(out), "${1}"+Mask+"@")
	}
	s = rtspUser.ReplaceAllString(s, "${1}"+Mask+"@")
	if json.Valid([]byte(s)) {
		return s
	}
	return pair.ReplaceAllString(s, `${1}"`+Mask+`"`)
}
