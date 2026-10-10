package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/trevin-lee/prusactl/internal/appdir/appdirtest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trevin-lee/prusactl/internal/auth"
	"github.com/trevin-lee/prusactl/internal/connect"
	"github.com/trevin-lee/prusactl/internal/link"
)

// signedIn is a session store holding a valid token, so no sign-in or
// refresh ever happens.
type signedIn struct{}

func (signedIn) Load() (*auth.Token, error) {
	return &auth.Token{AccessToken: "access", RefreshToken: "refresh", Expiry: time.Now().Add(time.Hour)}, nil
}
func (signedIn) Save(*auth.Token) error { return nil }
func (signedIn) Clear() error           { return nil }

// fakeConnect is a stand-in Prusa Connect with one printer.
type fakeConnect struct {
	mu    sync.Mutex
	state string
	sent  []string // commands the printer was sent

	// Simulated API changes.
	renamedList bool // /app/printers lists them under another key
	noState     bool // printer records carry no connect_state

	events []map[string]any // newest first, as Connect returns them
	queue  []map[string]any
	gone   []string // file hashes the delete asked Connect to drop

	// Cameras: the camera service (GraphQL + snapshots) and the old endpoints.
	base          string // the fake's own URL
	serviceDown   bool   // GraphQL answers CameraServiceError
	snapshotURL   string // overrides the snapshot URL GraphQL hands out
	snapshotAuths []string
}

func (f *fakeConnect) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/app/printers" && f.renamedList:
		fmt.Fprint(w, `{"items":[{"uuid":"u1","name":"Core One","team_id":1}]}`)
	case r.URL.Path == "/app/printers":
		fmt.Fprintf(w, `{"printers":[{"uuid":"u1","name":"Core One","team_id":1,"connect_state":%q}]}`, f.state)
	case r.URL.Path == "/app/printers/u1" && f.noState:
		fmt.Fprint(w, `{"uuid":"u1","name":"Core One","status":{"state":"FINISHED"}}`)
	case r.URL.Path == "/app/printers/u1":
		fmt.Fprintf(w, `{"uuid":"u1","name":"Core One","connect_state":%q}`, f.state)
	case r.URL.Path == "/app/printers/u1/supported-commands":
		states := []string{"IDLE", "READY", "FINISHED", "STOPPED"}
		var cmds []map[string]any
		for _, c := range []string{"HOME", "MOVE_Z", "MOVE_E", "SET_PRINTER_READY", "SET_NOZZLE_TEMPERATURE"} {
			cmds = append(cmds, map[string]any{"command": c, "executable_from_state": states})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"commands": cmds})
	case r.URL.Path == "/app/printers/u1/commands/sync" && r.Method == http.MethodPost:
		var body struct {
			Command string `json:"command"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.sent = append(f.sent, body.Command)
		fmt.Fprint(w, `{"event":{"event":"FINISHED"}}`)
	case r.URL.Path == "/app/printers/u1/events":
		from, to := page(r, len(f.events))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"events": f.events[from:to],
			"pager":  map[string]any{"limit": to - from, "offset": from, "total": len(f.events)},
		})
	case r.URL.Path == "/app/printers/u1/queue":
		_ = json.NewEncoder(w).Encode(map[string]any{"queue": f.queue})
	case r.URL.Path == "/app/printers/u1/cameras":
		fmt.Fprint(w, `{"cameras":[{"id":601734,"name":"Buddy3D Camera","token":"cam-token"}]}`)
	case r.URL.Path == "/graphql" && r.Method == http.MethodPost:
		conn := map[string]any{"__typename": "CameraServiceError", "errorCode": "UNAVAILABLE"}
		if !f.serviceDown {
			u := f.snapshotURL
			if u == "" {
				u = f.base + "/v1/snapshot/cam-uuid"
			}
			node := map[string]any{"token": "cam-token", "snapshots": map[string]any{"lastSnapshotUrl": u}}
			conn = map[string]any{"__typename": "CameraConnection", "edges": []any{map[string]any{"node": node}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"camera": map[string]any{"camerasConnection": conn}}})
	case strings.HasPrefix(r.URL.Path, "/v1/snapshot/"):
		f.snapshotAuths = append(f.snapshotAuths, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "image/jpeg")
		fmt.Fprint(w, "\xff\xd8service")
	case r.URL.Path == "/app/cameras/601734/snapshots/last":
		w.Header().Set("Content-Type", "image/jpeg")
		fmt.Fprint(w, "\xff\xd8legacy")
	case r.URL.Path == "/app/teams/1/files/raw" && r.Method == http.MethodDelete:
		var body struct {
			Hashes []string `json:"hashes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.gone = append(f.gone, body.Hashes...)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// page applies the limit and offset Connect's list endpoints take.
func page(r *http.Request, total int) (int, int) {
	from, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit <= 0 {
		limit = total
	}
	from = min(from, total)
	return from, min(from+limit, total)
}

func (f *fakeConnect) sentCommands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func connectConnectTools(t *testing.T, fc *fakeConnect) *mcp.ClientSession {
	t.Helper()
	appdirtest.Use(t) // the refresh lock file lives under the config dir
	srv := httptest.NewServer(fc)
	t.Cleanup(srv.Close)
	session := &auth.Session{Store: signedIn{}}
	cc := connect.New(session, "test")
	cc.BaseURL = srv.URL
	cc.GraphQLURL = srv.URL + "/graphql"
	fc.base = srv.URL
	s := New(session, cc, nil, link.ErrNotConfigured, "test")
	s.openLink = func() (*link.Client, error) { return nil, link.ErrNotConfigured }

	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := s.mcp.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// send_command applies the same plate rule as start_print: after a finished
// or stopped print, commands that move toward the plate need plate_clear.
func TestSendCommandPlateRule(t *testing.T) {
	fc := &fakeConnect{state: "FINISHED"}
	cs := connectConnectTools(t, fc)

	for _, cmd := range []string{"HOME", "MOVE_Z"} {
		text, isErr := call(t, cs, "send_command", map[string]any{"command": cmd})
		if !isErr || !strings.Contains(text, "plate_clear") {
			t.Errorf("%s after a finished print without plate_clear: %q", cmd, text)
		}
	}
	if sent := fc.sentCommands(); len(sent) != 0 {
		t.Fatalf("sent %v without a plate check", sent)
	}

	// Commands that don't reach toward the plate aren't held up, and marking
	// the printer ready is itself the confirmation.
	for _, cmd := range []string{"MOVE_E", "SET_NOZZLE_TEMPERATURE", "SET_PRINTER_READY"} {
		if text, isErr := call(t, cs, "send_command", map[string]any{"command": cmd}); isErr {
			t.Errorf("%s: %s", cmd, text)
		}
	}
	if text, isErr := call(t, cs, "send_command", map[string]any{"command": "home", "plate_clear": true}); isErr {
		t.Fatalf("HOME with plate_clear: %s", text)
	}
	want := []string{"MOVE_E", "SET_NOZZLE_TEMPERATURE", "SET_PRINTER_READY", "HOME"}
	if got := fc.sentCommands(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("sent %v, want %v", got, want)
	}

	// Once the printer is idle there's nothing left from a print to hit.
	fc.mu.Lock()
	fc.state = "IDLE"
	fc.mu.Unlock()
	if text, isErr := call(t, cs, "send_command", map[string]any{"command": "HOME"}); isErr {
		t.Fatalf("HOME while idle: %s", text)
	}
}

// A renamed or missing field must surface as "Prusa changed its API", not as
// a wrong answer ("no printers") or a check silently skipped.
func TestChangedConnectFormatIsReported(t *testing.T) {
	cs := connectConnectTools(t, &fakeConnect{state: "IDLE", renamedList: true})
	text, isErr := call(t, cs, "list_supported_commands", map[string]any{})
	if !isErr || !strings.Contains(text, "changed its API") || strings.Contains(text, "no printers") {
		t.Errorf("renamed printer list: %q", text)
	}

	fc := &fakeConnect{state: "FINISHED", noState: true}
	cs = connectConnectTools(t, fc)
	text, isErr = call(t, cs, "send_command", map[string]any{"command": "HOME"})
	if !isErr || !strings.Contains(text, "connect_state") || !strings.Contains(text, "changed its API") {
		t.Errorf("record without a state: %q", text)
	}
	if sent := fc.sentCommands(); len(sent) != 0 {
		t.Fatalf("sent %v although the state couldn't be read", sent)
	}
}

// snapshotImage calls get_camera_snapshot and returns the image bytes.
func snapshotImage(t *testing.T, cs *mcp.ClientSession) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_camera_snapshot", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Content {
		if img, ok := c.(*mcp.ImageContent); ok {
			return string(img.Data)
		}
	}
	t.Fatalf("no image in %+v", res.Content)
	return ""
}

// Since Prusa's camera service move, snapshots come from the URL GraphQL gives
// for the camera's token, fetched with the session.
func TestCameraSnapshotFromCameraService(t *testing.T) {
	fc := &fakeConnect{state: "IDLE"}
	cs := connectConnectTools(t, fc)
	if got := snapshotImage(t, cs); got != "\xff\xd8service" {
		t.Fatalf("image %q, want the camera service's", got)
	}
	if len(fc.snapshotAuths) != 1 || fc.snapshotAuths[0] != "Bearer access" {
		t.Fatalf("snapshot requests carried %v", fc.snapshotAuths)
	}

	fc.mu.Lock()
	fc.serviceDown = true
	fc.mu.Unlock()
	if got := snapshotImage(t, cs); got != "\xff\xd8legacy" {
		t.Fatalf("with the camera service down: image %q, want the old endpoint's", got)
	}
}

// A snapshot URL off Prusa's hosts never receives the session.
func TestCameraSnapshotSessionStaysWithPrusa(t *testing.T) {
	var hits []string
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.Header.Get("Authorization"))
		fmt.Fprint(w, "stolen")
	}))
	t.Cleanup(elsewhere.Close)
	fc := &fakeConnect{state: "IDLE", snapshotURL: elsewhere.URL + "/v1/snapshot/cam-uuid"}
	cs := connectConnectTools(t, fc)
	if got := snapshotImage(t, cs); got != "\xff\xd8legacy" {
		t.Fatalf("image %q, want the old endpoint's", got)
	}
	if len(hits) != 0 {
		t.Fatalf("the other site was contacted: %v", hits)
	}
}
