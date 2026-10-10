package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fwConnect is a stand-in Prusa Connect for a CORE One L, shaped like the
// answers the real one gave. The firmware file shows up on the printer
// (path) after pollsToCopy polls of its record, or never when that is 0
// and onPrinter is false.
type fwConnect struct {
	support        string // support.state Connect reports; "outdated" when empty
	noSupportState bool   // Connect gives no verdict at all
	flashEvent     string // what the sync FLASH answers with; FINISHED when empty
	printAfterCopy bool   // a print starts the moment the file lands, for the blocked path
	noState        bool   // the printer record carries no connect_state (an API change)
	mu             sync.Mutex
	state          string
	current        string
	latest         string
	onPrinter      bool
	pollsToCopy    int
	abort          string

	polls    int
	queued   []map[string]any
	commands []map[string]any
}

const fwName = "COREONE_L_COEONE_L+_firmware_6.8.1.bbf"

func (f *fwConnect) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/app/printers":
		fmt.Fprint(w, `{"printers":[{"uuid":"u1","name":"Core One","team_id":1}]}`)
	case "/app/printers/u1":
		rec := map[string]any{
			"uuid": "u1", "name": "Core One", "connect_state": f.state, "firmware": f.current,
			"allowed_functionalities": []string{"files", "fw_update", "fw_update_path"},
			"support":                 map[string]any{"current": f.current, "latest": f.latest, "stable": f.latest, "state": f.supportState()},
		}
		if f.noState {
			delete(rec, "connect_state")
		}
		_ = json.NewEncoder(w).Encode(rec)
	case "/app/printers/u1/storages":
		fmt.Fprint(w, `{"storages":[{"mountpoint":"/sd","type":"SD_CARD","read_only":true},{"mountpoint":"/usb","name":"usb","type":"USB","read_only":false}]}`)
	case "/app/printers/u1/supported-commands":
		_ = json.NewEncoder(w).Encode(map[string]any{"commands": []map[string]any{
			{"command": "FLASH", "executable_from_state": []string{"IDLE", "READY", "STOPPED", "FINISHED"}},
		}})
	case "/app/printers/u1/firmware":
		q := r.URL.Query()
		if v := q.Get("version"); v != "" && v != "6.8.1" {
			http.Error(w, `{"code":"NOT_FOUND_FILE"}`, http.StatusNotFound)
			return
		}
		rec := map[string]any{"type": "FIRMWARE", "name": fwName, "hash": "H4SH.", "team_id": 0, "meta": map[string]any{"version": "6.8.1"}}
		if q.Get("hash") != "" {
			f.polls++
		}
		switch {
		case f.onPrinter || (f.pollsToCopy > 0 && f.polls >= f.pollsToCopy):
			rec["path"] = "/usb/COREON~3.BBF"
			if f.printAfterCopy {
				f.state = "PRINTING"
			}
		case f.pollsToCopy >= 4 && f.polls == f.pollsToCopy-1: // transfer over, file not reported yet
		case f.abort != "":
			rec["planned"] = map[string]any{"id": 7, "abort_reason": f.abort}
		case f.polls > 0 && f.pollsToCopy > 0:
			rec["transferring"] = map[string]any{"progress": 40.0, "transferred": f.polls}
		case f.polls > 0:
			rec["planned"] = map[string]any{"id": 7}
		}
		_ = json.NewEncoder(w).Encode(rec)
	case "/app/printers/u1/download-queue":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.queued = append(f.queued, body)
		w.WriteHeader(http.StatusCreated)
	case "/app/printers/u1/commands/sync":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.commands = append(f.commands, body)
		fmt.Fprintf(w, `{"event":{"event":%q}}`, orDefault(f.flashEvent, "FINISHED"))
	default:
		http.NotFound(w, r)
	}
}

func (f *fwConnect) supportState() string {
	if f.noSupportState {
		return ""
	}
	return orDefault(f.support, "outdated")
}

func fwTools(t *testing.T, fc *fwConnect, stall time.Duration) *mcp.ClientSession {
	t.Helper()
	return connectConnectTools(t, fc, func(s *Server) { s.fwPoll, s.fwStall = time.Millisecond, stall })
}

func TestFirmwareStatus(t *testing.T) {
	fc := &fwConnect{state: "IDLE", current: "6.5.7+10473", latest: "6.8.1"}
	text, isErr := call(t, fwTools(t, fc, time.Second), "get_firmware_status", nil)
	if isErr {
		t.Fatal(text)
	}
	var got struct {
		Current, Latest, State string
		Update                 bool `json:"update_available"`
		Supported              bool `json:"update_supported"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatal(err)
	}
	if got.Current != "6.5.7+10473" || got.Latest != "6.8.1" || got.State != "outdated" || !got.Update || !got.Supported {
		t.Errorf("%s", text)
	}
	fc.current, fc.support = "6.8.1+16182", "supported" // Connect's verdict says it's current
	if text, _ := call(t, fwTools(t, fc, time.Second), "get_firmware_status", nil); !strings.Contains(text, `"update_available":false`) {
		t.Errorf("%s", text)
	}
}

func TestUpdateFirmwareWhenUpToDateDoesNothing(t *testing.T) {
	fc := &fwConnect{state: "IDLE", current: "6.8.1+16182", latest: "6.8.1", support: "supported"}
	text, isErr := call(t, fwTools(t, fc, time.Second), "update_firmware", nil)
	if isErr || !strings.Contains(text, `"up_to_date":true`) {
		t.Fatalf("%q (error=%v)", text, isErr)
	}
	if len(fc.queued)+len(fc.commands) != 0 {
		t.Errorf("sent something: %v %v", fc.queued, fc.commands)
	}
}

func TestUpdateFirmwareCopiesThenInstalls(t *testing.T) {
	fc := &fwConnect{state: "IDLE", current: "6.5.7+10473", latest: "6.8.1", pollsToCopy: 3}
	text, isErr := call(t, fwTools(t, fc, time.Second), "update_firmware", nil)
	if isErr {
		t.Fatal(text)
	}
	if len(fc.queued) != 1 || fc.queued[0]["hash"] != "H4SH." || fc.queued[0]["path"] != "/usb/"+fwName || fc.queued[0]["team_id"] != float64(0) {
		t.Errorf("download-queue: %v", fc.queued)
	}
	if len(fc.commands) != 1 || fc.commands[0]["command"] != "FLASH" ||
		fc.commands[0]["kwargs"].(map[string]any)["path"] != "/usb/COREON~3.BBF" {
		t.Errorf("commands: %v", fc.commands)
	}
	if !strings.Contains(text, `"installed":true`) || !strings.Contains(text, `"copied":true`) {
		t.Errorf("%s", text)
	}
}

// A file already on the printer is not copied again, and --version installs
// even when the printer already runs it.
func TestUpdateFirmwareUsesTheFileAlreadyOnThePrinter(t *testing.T) {
	fc := &fwConnect{state: "FINISHED", current: "6.8.1", latest: "6.8.1", onPrinter: true}
	text, isErr := call(t, fwTools(t, fc, time.Second), "update_firmware", map[string]any{"version": "6.8.1"})
	if isErr || !strings.Contains(text, `"copied":false`) {
		t.Fatalf("%q (error=%v)", text, isErr)
	}
	if len(fc.queued) != 0 || len(fc.commands) != 1 {
		t.Errorf("queued %v, commands %v", fc.queued, fc.commands)
	}
	if text, isErr := call(t, fwTools(t, fc, time.Second), "update_firmware", map[string]any{"version": "6.4.0"}); !isErr || !strings.Contains(text, "no firmware 6.4.0") {
		t.Errorf("an unknown version: %q", text)
	}
}

// FLASH is refused while printing: the file is copied and left there.
// A printer that is printing is refused before anything is copied, the way
// upload_file then=print refuses before a long upload: the alternative wrote a
// file to the drive the print was reading from and then reported success.
func TestUpdateFirmwareRefusesWhilePrinting(t *testing.T) {
	fc := &fwConnect{state: "PRINTING", current: "6.5.7", latest: "6.8.1", pollsToCopy: 1}
	text, isErr := call(t, fwTools(t, fc, time.Second), "update_firmware", nil)
	if !isErr || !strings.Contains(text, "PRINTING") || !strings.Contains(text, "nothing was copied") {
		t.Fatalf("%q (error=%v)", text, isErr)
	}
	if len(fc.queued)+len(fc.commands) != 0 {
		t.Errorf("queued %v, commands %v", fc.queued, fc.commands)
	}
}

// A print that starts while the file is on its way can't be helped; then the
// file stays on the drive and the result must not claim more than that.
func TestUpdateFirmwareLeavesTheFileStagedWhenAPrintStartsDuringTheCopy(t *testing.T) {
	fc := &fwConnect{state: "IDLE", current: "6.5.7", latest: "6.8.1", pollsToCopy: 1, printAfterCopy: true}
	text, isErr := call(t, fwTools(t, fc, time.Second), "update_firmware", nil)
	if isErr {
		t.Fatal(text)
	}
	if !strings.Contains(text, `"installed":false`) || !strings.Contains(text, "/usb/COREON~3.BBF but not installed") || !strings.Contains(text, "PRINTING") {
		t.Errorf("%s", text)
	}
	if len(fc.queued) != 1 || len(fc.commands) != 0 {
		t.Errorf("queued %v, commands %v", fc.queued, fc.commands)
	}
}

func TestUpdateFirmwareGivesUpWhenTheCopyStalls(t *testing.T) {
	fc := &fwConnect{state: "IDLE", current: "6.5.7", latest: "6.8.1"}
	text, isErr := call(t, fwTools(t, fc, 30*time.Millisecond), "update_firmware", nil)
	if !isErr || !strings.Contains(text, "no progress") || !strings.Contains(text, "not installed") {
		t.Fatalf("%q (error=%v)", text, isErr)
	}
	if len(fc.commands) != 0 {
		t.Errorf("installed anyway: %v", fc.commands)
	}

	fc = &fwConnect{state: "IDLE", current: "6.5.7", latest: "6.8.1", abort: "READ_ONLY"}
	if text, isErr := call(t, fwTools(t, fc, time.Second), "update_firmware", nil); !isErr || !strings.Contains(text, "READ_ONLY") {
		t.Errorf("an aborted copy: %q", text)
	}
}

// progressOf runs update_firmware and returns the progress messages it sent.
func progressOf(t *testing.T, fc *fwConnect) []string {
	t.Helper()
	var mu sync.Mutex
	var msgs []string
	opts := &mcp.ClientOptions{ProgressNotificationHandler: func(_ context.Context, r *mcp.ProgressNotificationClientRequest) {
		mu.Lock()
		msgs = append(msgs, r.Params.Message)
		mu.Unlock()
	}}
	cs := connectConnectToolsWith(t, opts, fc, func(s *Server) { s.fwPoll, s.fwStall = time.Millisecond, time.Second })
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "update_firmware",
		Meta: mcp.Meta{"progressToken": "p"}})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	time.Sleep(50 * time.Millisecond) // notifications arrive asynchronously
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), msgs...)
}

// Repeated polls say nothing new, and the gap after the transfer is the copy
// finishing, not the download starting.
func TestUpdateFirmwareProgressIsDeduplicatedAndAccurate(t *testing.T) {
	fc := &fwConnect{state: "IDLE", current: "6.5.7", latest: "6.8.1", pollsToCopy: 6}
	got := strings.Join(progressOf(t, fc), "|")
	want := "copying to the printer: 40%|finishing the copy|installing (the printer restarts)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Nothing says "installing" when FLASH is refused.
func TestUpdateFirmwareDoesNotClaimToInstallWhenBlocked(t *testing.T) {
	fc := &fwConnect{state: "IDLE", current: "6.5.7", latest: "6.8.1", pollsToCopy: 1, printAfterCopy: true}
	for _, m := range progressOf(t, fc) {
		if strings.Contains(m, "installing") {
			t.Errorf("progress %q while FLASH was refused", m)
		}
	}
}

// The record Connect returns for a command it waited for says CREATED; the
// event that ended the wait says what the printer did. A FLASH the printer
// turned down was reported as installed.
func TestUpdateFirmwareReportsARejectedFlash(t *testing.T) {
	fc := &fwConnect{state: "IDLE", current: "6.5.7", latest: "6.8.1", pollsToCopy: 1, flashEvent: "REJECTED"}
	text, isErr := call(t, fwTools(t, fc, time.Second), "update_firmware", nil)
	if isErr {
		t.Fatal(text)
	}
	if !strings.Contains(text, `"installed":false`) || !strings.Contains(text, "REJECTED") || strings.Contains(text, "restarts") {
		t.Errorf("a rejected FLASH was reported as: %s", text)
	}
}

// "Up to date" is Connect's verdict, not a string comparison: a printer running
// something newer than Connect's latest used to be flashed back to it.
func TestUpdateFirmwareNeverDowngradesByAccident(t *testing.T) {
	fc := &fwConnect{state: "IDLE", current: "7.0.0-RC1+17000", latest: "6.8.1", support: "supported"}
	cs := fwTools(t, fc, time.Second)
	text, isErr := call(t, cs, "update_firmware", nil)
	if isErr || !strings.Contains(text, `"up_to_date":true`) {
		t.Fatalf("%q (error=%v)", text, isErr)
	}
	if len(fc.queued)+len(fc.commands) != 0 {
		t.Errorf("flashed an older build over a newer one: %v %v", fc.queued, fc.commands)
	}
	if text, _ := call(t, cs, "get_firmware_status", nil); !strings.Contains(text, `"update_available":false`) {
		t.Errorf("status offered a downgrade: %s", text)
	}

	// Without a verdict there is nothing safe to decide on.
	fc = &fwConnect{state: "IDLE", current: "6.5.7", latest: "6.8.1", noSupportState: true}
	if text, isErr := call(t, fwTools(t, fc, time.Second), "update_firmware", nil); !isErr || !strings.Contains(text, "pass version") {
		t.Errorf("no verdict, yet: %q (error=%v)", text, isErr)
	}
}

// A verdict Connect might use that prusactl doesn't know ("unsupported", or a
// new one) is not "up to date": nobody said so. Status leaves update_available
// out and says why; update refuses to decide.
func TestFirmwareUnknownVerdictDecidesNothing(t *testing.T) {
	fc := &fwConnect{state: "IDLE", current: "6.5.7", latest: "6.8.1", support: "unsupported", pollsToCopy: 1}
	cs := fwTools(t, fc, time.Second)
	text, isErr := call(t, cs, "update_firmware", nil)
	if !isErr || !strings.Contains(text, "pass version") || !strings.Contains(text, "unsupported") {
		t.Errorf("update: %q (error=%v)", text, isErr)
	}
	if len(fc.queued)+len(fc.commands) != 0 {
		t.Errorf("acted on an unknown verdict: %v %v", fc.queued, fc.commands)
	}
	text, _ = call(t, cs, "get_firmware_status", nil)
	if strings.Contains(text, "update_available") || !strings.Contains(text, "no verdict") {
		t.Errorf("status decided for Connect: %s", text)
	}
	// An explicit version still installs.
	if text, isErr := call(t, cs, "update_firmware", map[string]any{"version": "6.8.1"}); isErr || !strings.Contains(text, `"installed":true`) {
		t.Errorf("explicit version: %q (error=%v)", text, isErr)
	}
}

// A printer record with no state is Prusa changing its API, which is reported
// as such; it is not a printer in some state that refuses to install.
func TestUpdateFirmwareReportsAMissingStateAsAnAPIChange(t *testing.T) {
	fc := &fwConnect{state: "IDLE", current: "6.5.7", latest: "6.8.1", noState: true}
	text, isErr := call(t, fwTools(t, fc, time.Second), "update_firmware", nil)
	if !isErr || !strings.Contains(text, "connect_state") || strings.Contains(text, "nothing was copied") {
		t.Errorf("%q (error=%v)", text, isErr)
	}
}
