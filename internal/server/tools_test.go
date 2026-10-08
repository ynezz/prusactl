package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trevin-lee/prusactl/internal/auth"
	"github.com/trevin-lee/prusactl/internal/connect"
	"github.com/trevin-lee/prusactl/internal/link"
)

// fakePrinter is a stand-in PrusaLink printer that records what it's asked to do.
type fakePrinter struct {
	mu       sync.Mutex
	state    string // printer state in /api/v1/status
	jobState string // "" means no job
	files    int    // entries in /usb
	calls    []string
	// handler answers a request before the defaults, for a test that needs the
	// printer to behave a particular way. It reports whether it replied.
	handler func(http.ResponseWriter, *http.Request) bool
	upload  struct {
		body    string
		headers http.Header
	}
}

func (f *fakePrinter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	if f.handler != nil && f.handler(w, r) {
		return
	}
	switch {
	case r.URL.Path == "/api/v1/info":
		fmt.Fprint(w, `{"hostname":"fake-core-one","serial":"SN1"}`)
	case r.URL.Path == "/api/v1/status":
		fmt.Fprintf(w, `{"printer":{"state":%q}}`, f.state)
	case r.URL.Path == "/api/v1/job" && r.Method == http.MethodGet:
		if f.jobState == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		fmt.Fprintf(w, `{"id":7,"state":%q}`, f.jobState)
	case strings.HasPrefix(r.URL.Path, "/api/v1/job/7"):
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/api/v1/files/usb/" || r.URL.Path == "/api/v1/files/usb":
		children := make([]map[string]any, f.files)
		for i := range children {
			children[i] = map[string]any{"name": fmt.Sprintf("F%03d.BGC", i), "display_name": fmt.Sprintf("part-%03d.bgcode", i), "type": "PRINT_FILE"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "usb", "children": children})
	case r.URL.Path == "/api/v1/files/usb/prusactl-macro.gcode" && r.Method == http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		f.upload.body, f.upload.headers = string(b), r.Header.Clone()
		w.WriteHeader(http.StatusCreated)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakePrinter) called(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// connectTools runs the real MCP server, direct route only, against fp and
// returns a client session to call its tools with.
func connectTools(t *testing.T, fp *fakePrinter) *mcp.ClientSession {
	t.Helper()
	return connectToolsWith(t, fp, nil)
}

// connectToolsWith is connectTools with a chance to adjust the server before
// it starts, e.g. to give it a camera.
func connectToolsWith(t *testing.T, fp *fakePrinter, tweak func(*Server)) *mcp.ClientSession {
	t.Helper()
	srv := httptest.NewServer(fp)
	t.Cleanup(srv.Close)
	lc := link.New(link.Config{Host: srv.URL, Auth: link.AuthAPIKey}, "k")
	session := &auth.Session{Store: noSession{}}
	s := New(session, connect.New(session, "test"), lc, nil, "test")
	// Never read the real saved setup, which would point at a real printer.
	s.openLink = func() (*link.Client, error) { return lc, nil }
	// Nor the saved camera.
	s.cameraURL = func() (string, error) { return "", nil }
	if tweak != nil {
		tweak(s)
	}

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

// call runs a tool and returns its text and whether it reported an error.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return err.Error(), true
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func TestEveryToolIsListedAndDescribed(t *testing.T) {
	cs := connectTools(t, &fakePrinter{state: "IDLE"})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) < 20 {
		t.Fatalf("only %d tools", len(res.Tools))
	}
	for _, tool := range res.Tools {
		if len(tool.Description) < 20 {
			t.Errorf("%s has no real description", tool.Name)
		}
	}
}

func TestListPrinterFilesPages(t *testing.T) {
	cs := connectTools(t, &fakePrinter{state: "IDLE", files: 120})
	page := func(args map[string]any) (entries int, total, next float64) {
		t.Helper()
		args["path"] = "/usb"
		text, isErr := call(t, cs, "list_printer_files", args)
		if isErr {
			t.Fatal(text)
		}
		var out struct {
			Files struct {
				Entries    []any   `json:"entries"`
				Total      float64 `json:"total"`
				NextOffset float64 `json:"next_offset"`
			} `json:"files"`
		}
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatalf("%v: %s", err, text)
		}
		return len(out.Files.Entries), out.Files.Total, out.Files.NextOffset
	}
	if n, total, next := page(map[string]any{}); n != 50 || total != 120 || next != 50 {
		t.Errorf("default page: %d entries, total %v, next %v", n, total, next)
	}
	if n, _, next := page(map[string]any{"offset": 100}); n != 20 || next != 0 {
		t.Errorf("last page: %d entries, next %v", n, next)
	}
	if n, _, _ := page(map[string]any{"limit": 5000}); n != 120 {
		t.Errorf("big limit: %d entries", n)
	}
}

func TestRunGcodeUploadsAMacroOnlyWhenSafe(t *testing.T) {
	fp := &fakePrinter{state: "PRINTING"}
	cs := connectTools(t, fp)
	if text, isErr := call(t, cs, "run_gcode", map[string]any{"gcode": "G28"}); !isErr || !strings.Contains(text, "PRINTING") {
		t.Fatalf("busy printer: %q", text)
	}
	fp.state = "FINISHED"
	if text, isErr := call(t, cs, "run_gcode", map[string]any{"gcode": "G28"}); !isErr || !strings.Contains(text, "plate_clear") {
		t.Fatalf("finished printer without plate_clear: %q", text)
	}
	if fp.called("PUT") {
		t.Fatal("uploaded although the printer wasn't ready")
	}

	fp.state = "IDLE"
	if text, isErr := call(t, cs, "run_gcode", map[string]any{"gcode": "G28\nM115"}); isErr {
		t.Fatal(text)
	}
	lines := strings.Split(strings.TrimSpace(fp.upload.body), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[1], `M862.6 P"Input shaper"`) || lines[2] != "G28" || lines[3] != "M115" {
		t.Errorf("macro body = %q", fp.upload.body)
	}
	if fp.upload.headers.Get("Print-After-Upload") != "?1" || fp.upload.headers.Get("Overwrite") != "?1" {
		t.Errorf("upload headers = %v", fp.upload.headers)
	}
}

func TestControlPrintResumesOnlyFromPaused(t *testing.T) {
	fp := &fakePrinter{state: "PAUSED", jobState: "PAUSED"}
	cs := connectTools(t, fp)
	if text, isErr := call(t, cs, "control_print", map[string]any{"action": "resume"}); isErr {
		t.Fatal(text)
	}
	if !fp.called("PUT /api/v1/job/7/resume") {
		t.Fatalf("resume didn't reach the printer: %v", fp.calls)
	}
	// "continue" was an undocumented second name for resume; it now points there.
	text, isErr := call(t, cs, "control_print", map[string]any{"action": "continue"})
	if !isErr || !strings.Contains(text, "use resume") {
		t.Errorf("continue: %q", text)
	}

	fp2 := &fakePrinter{state: "ATTENTION", jobState: "ATTENTION"}
	cs2 := connectTools(t, fp2)
	if text, isErr := call(t, cs2, "control_print", map[string]any{"action": "resume"}); !isErr || !strings.Contains(text, "answered there") {
		t.Fatalf("resume from ATTENTION: %q", text)
	}
	if fp2.called("PUT /api/v1/job") {
		t.Fatal("sent a resume the firmware would reject")
	}
}

func TestStartPrintRefusesABusyPrinter(t *testing.T) {
	fp := &fakePrinter{state: "PRINTING"}
	cs := connectTools(t, fp)
	if text, isErr := call(t, cs, "start_print", map[string]any{"path": "/usb/part.bgcode"}); !isErr || !strings.Contains(text, "PRINTING") {
		t.Fatalf("start_print on a busy printer: %q", text)
	}
	if fp.called("POST") {
		t.Fatal("started a print on a busy printer")
	}
}

// via forces a route, so a tool that can only use one must refuse the other
// instead of quietly ignoring it, and a bad value is an error everywhere.
func TestViaIsRefusedWhenImpossible(t *testing.T) {
	cs := connectTools(t, &fakePrinter{state: "IDLE", files: 1})
	for _, tc := range []struct {
		tool, via, want string
		args            map[string]any
	}{
		{tool: "run_gcode", via: "connect", want: "only works through the direct connection", args: map[string]any{"gcode": "M115"}},
		{tool: "download_printer_file", via: "connect", want: "only works through the direct connection",
			// Absolute means something different on Windows, and the path is
			// checked before the route is.
			args: map[string]any{"path": "/usb/x.bgcode", "local_path": filepath.Join(t.TempDir(), "x.bgcode")}},
		{tool: "get_camera_snapshot", via: "direct", want: "no local camera"},
		{tool: "get_queue", via: "direct", want: "only works through Prusa Connect"},
		{tool: "respond_to_dialog", via: "direct", want: "only works through Prusa Connect", args: map[string]any{"button": "Yes"}},
		{tool: "get_printer", via: "bogus", want: "via must be direct or connect"},
		{tool: "get_queue", via: "bogus", want: "via must be direct or connect"},
		{tool: "run_gcode", via: "bogus", want: "via must be direct or connect", args: map[string]any{"gcode": "M115"}},
	} {
		args := map[string]any{"via": tc.via}
		for k, v := range tc.args {
			args[k] = v
		}
		text, isErr := call(t, cs, tc.tool, args)
		if !isErr || !strings.Contains(text, tc.want) {
			t.Errorf("%s via=%s: %q, want %q", tc.tool, tc.via, text, tc.want)
		}
	}
}
