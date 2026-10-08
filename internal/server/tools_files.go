package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trevin-lee/prusactl/internal/connect"
	"github.com/trevin-lee/prusactl/internal/link"
)

type listFilesInput struct {
	printerRef
	Path   string `json:"path,omitempty" jsonschema:"folder on the printer, e.g. /usb or /usb/parts; omit to list the printer's storages"`
	Limit  int    `json:"limit,omitempty" jsonschema:"how many entries to return; at most 500"`
	Offset int    `json:"offset,omitempty" jsonschema:"entries to skip, for the next page"`
}

type uploadInput struct {
	printerRef
	plateConfirmation
	LocalPath   string `json:"local_path" jsonschema:"absolute path of a sliced file (.bgcode or .gcode) on this computer"`
	Destination string `json:"destination,omitempty" jsonschema:"printer folder to copy it into, e.g. /usb/; default is the printer's first storage"`
	Filename    string `json:"filename,omitempty" jsonschema:"name on the printer; default is the local file name"`
	Then        string `json:"then,omitempty" jsonschema:"after uploading: none (just store it), print (start it right away), or queue (add to the Prusa Connect print queue); default none"`
	Overwrite   bool   `json:"overwrite,omitempty" jsonschema:"replace a file with the same name on the printer (direct route)"`
}

type downloadInput struct {
	printerRef
	Path      string `json:"path" jsonschema:"file on the printer, e.g. /usb/part.bgcode"`
	LocalPath string `json:"local_path" jsonschema:"absolute path on this computer: a folder (the file keeps its name) or a file name"`
	Overwrite bool   `json:"overwrite,omitempty" jsonschema:"replace an existing local file"`
}

type queueInput struct {
	printerRef
	Path      string `json:"path,omitempty" jsonschema:"file already on the printer, e.g. /usb/part.bgcode"`
	Hash      string `json:"hash,omitempty" jsonschema:"file in Connect storage (from upload_file or list_connect_files); use instead of path"`
	TeamID    int64  `json:"team_id,omitempty" jsonschema:"team owning the Connect file; default is the printer's team"`
	Position  *int   `json:"position,omitempty" jsonschema:"0 = front of the queue, -1 = end (default)"`
	SetReady  bool   `json:"set_ready,omitempty" jsonschema:"also mark the printer ready so Connect starts the next job; only with hash. Ready means the plate is clear, the same confirmation plate_clear gives elsewhere: set it only after checking the camera or asking the user"`
	WaitUntil int64  `json:"wait_until,omitempty" jsonschema:"unix time before which the job must not start; only with hash"`
}

type startPrintInput struct {
	printerRef
	plateConfirmation
	Path string `json:"path" jsonschema:"file on the printer as list_printer_files reports it, e.g. /usb/3DBENC~2.BGC"`
}

type deleteFilesInput struct {
	printerRef
	Paths []string `json:"paths" jsonschema:"full paths on the printer, e.g. /usb/old.bgcode"`
}

type deleteConnectFilesInput struct {
	printerRef          // only to find the team the files belong to
	Hashes     []string `json:"hashes" jsonschema:"file hashes from list_connect_files or upload_file"`
	TeamID     int64    `json:"team_id,omitempty" jsonschema:"team owning the files; default is the team of the named printer"`
}

type queueJobInput struct {
	printerRef
	JobID int64 `json:"job_id" jsonschema:"queued job id from get_queue"`
}

type connectFilesInput struct {
	pagedRef       // default 50
	TeamID   int64 `json:"team_id,omitempty" jsonschema:"team whose storage to list; default is the team of the first printer"`
}

// teamOf works out whose cloud storage is meant. Connect's storage belongs to a
// team, not a printer, but a printer is how anyone names one: an explicit
// team_id wins, otherwise the team of the printer referred to.
func (s *Server) teamOf(ctx context.Context, ref printerRef, teamID int64) (int64, error) {
	if teamID != 0 {
		return teamID, nil
	}
	p, err := s.connectPrinter(ctx, ref)
	if err != nil {
		return 0, fmt.Errorf("%w, or name the team with team_id", err)
	}
	return p.TeamID, nil
}

// withStorages gives a storage listing one name for the list, whichever route
// answered: the printer calls it storage_list, Prusa Connect calls it storages.
// The route's own reply is kept.
func withStorages(raw json.RawMessage) json.RawMessage {
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil {
		return raw
	}
	if _, ok := body["storages"]; ok {
		return raw
	}
	list, ok := body["storage_list"]
	if !ok {
		return raw
	}
	body["storages"] = list
	out, err := json.Marshal(body)
	if err != nil {
		return raw
	}
	return out
}

// transferBrief is a transfer in progress, said the same way whichever route
// reported it. Connect's own record carries the whole sliced file's metadata,
// down to the outline of every object on the plate, which is not what "is
// anything being sent to the printer?" is asking.
type transferBrief struct {
	ID            *float64 `json:"id,omitempty"` // a handle on it, for api_request
	Name          string   `json:"name,omitempty"`
	Path          string   `json:"path,omitempty"`
	State         string   `json:"state,omitempty"`
	Type          string   `json:"type,omitempty"`
	Size          *float64 `json:"size,omitempty"`
	Transferred   *float64 `json:"transferred,omitempty"`
	Progress      *float64 `json:"progress,omitempty"`
	TimeRemaining *float64 `json:"time_remaining,omitempty"` // the printer reports these
	TimeElapsed   *float64 `json:"time_transferring,omitempty"`
	Started       *float64 `json:"start,omitempty"` // Prusa Connect reports this
}

func compactTransfer(raw json.RawMessage) any {
	m := decode(raw)
	if len(m) == 0 {
		return nil
	}
	b := &transferBrief{
		ID:            num(m, "id"),
		State:         str(m, "state"),
		Type:          str(m, "type"),
		Size:          num(m, "size"),
		Transferred:   num(m, "transferred"),
		Progress:      num(m, "progress"),
		TimeRemaining: num(m, "time_remaining"),
		TimeElapsed:   num(m, "time_transferring"),
		Started:       num(m, "start"),
	}
	for _, path := range [][]string{{"display_name"}, {"name"}, {"source_file", "display_name"}, {"source_file", "name"}} {
		if b.Name = str(m, path...); b.Name != "" {
			break
		}
	}
	if b.Path = str(m, "path"); b.Path == "" {
		b.Path = str(m, "destination")
	}
	if b.Name == "" && b.Path == "" && b.State == "" {
		return nil
	}
	// Connect reports how big it is but not how far along; the printer reports
	// both. Work out the one that's missing where it can be.
	if b.Progress == nil && b.Size != nil && b.Transferred != nil && *b.Size > 0 {
		pct := *b.Transferred / *b.Size * 100
		b.Progress = &pct
	}
	return b
}

// currentTransfer picks the transfer still running out of what Connect returns,
// so "is anything being sent to the printer?" has the same answer on both
// routes. Connect's list is the printer's transfer history, newest first, and
// every finished one carries an end; the printer itself only reports a
// transfer while it is happening.
func currentTransfer(raw json.RawMessage) (json.RawMessage, error) {
	var outer struct {
		Transfers *[]json.RawMessage `json:"transfers"`
	}
	// "Nothing is being transferred" is the wrong answer to give when the
	// reply couldn't be read: it is indistinguishable from the truth, and a
	// caller watching an upload would conclude it had arrived. Elsewhere a
	// changed field is reported as a changed API, and so here.
	if err := json.Unmarshal(raw, &outer); err != nil || outer.Transfers == nil {
		return nil, connect.Missing("GET", "/app/printers/{uuid}/transfers", "transfers")
	}
	for _, t := range *outer.Transfers {
		var one struct {
			End   *float64 `json:"end"`
			State string   `json:"state"`
		}
		if err := json.Unmarshal(t, &one); err != nil {
			return nil, connect.Missing("GET", "/app/printers/{uuid}/transfers", "transfers[]")
		}
		// Anything that has ended is history, however it ended; a transfer that
		// failed is still worth reporting, so the caller knows why nothing came.
		if one.End == nil && !strings.HasPrefix(one.State, "FIN") {
			return t, nil
		}
	}
	return json.RawMessage("null"), nil
}

// busyFileAdvice explains PrusaLink's 409 on a file it still holds open. The
// printer says only "File is busy", which is true of a file just uploaded or
// selected on its screen, and says nothing about what to do next.
func busyFileAdvice(err error) error {
	if !link.IsStatus(err, http.StatusConflict) {
		return err
	}
	return fmt.Errorf("%w (the printer still has the file open, usually because it was just uploaded or is selected on its screen; try again in a moment, or delete it through Prusa Connect with via=\"connect\")", err)
}

func (s *Server) addFileTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "list_printer_files",
		Description: "Browse files on the printer's own storage (USB drive). Without path, lists the storages.",
		Annotations: readOnly("List printer files"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listFilesInput) (*mcp.CallToolResult, any, error) {
		t, err := s.route(ctx, in.printerRef)
		if err != nil {
			return nil, nil, err
		}
		dir := strings.TrimSpace(in.Path)
		var out json.RawMessage
		switch {
		case t.direct && dir == "":
			if _, err = s.direct().Get(ctx, "/api/v1/storage", &out); err == nil {
				out = withStorages(out)
			}
		case t.direct:
			var path string
			if path, err = link.FilePath(dir); err == nil {
				_, err = s.direct().Get(ctx, path, &out)
			}
			if err == nil {
				out = compactFolder(dir, out, in.Limit, in.Offset)
			}
		case dir == "":
			if err = s.connect.Get(ctx, printerPath(t.connect.UUID, "storages"), nil, &out); err == nil {
				out = withStorages(out)
			}
		default:
			q := pageQuery(in.Limit, in.Offset, 50)
			q.Set("path", dir)
			if err = s.connect.Get(ctx, printerPath(t.connect.UUID, "files"), q, &out); err == nil {
				out = compactConnectFolder(out)
			}
		}
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(withVia(t, map[string]any{"files": out}))
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "delete_printer_files",
		Description: "Delete files from the printer's storage. Permanent: there is no trash and no undo, and a file " +
			"that is printing is refused. List the folder first and delete only what the user named or clearly " +
			"asked to clean up.",
		Annotations: mutating("Delete printer files", true),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in deleteFilesInput) (*mcp.CallToolResult, any, error) {
		if len(in.Paths) == 0 {
			return nil, nil, errors.New("paths is empty")
		}
		t, err := s.route(ctx, in.printerRef)
		if err != nil {
			return nil, nil, err
		}
		if t.direct {
			for i, p := range in.Paths {
				path, err := link.FilePath(p)
				if err == nil {
					_, err = s.direct().JSON(ctx, link.Request{Method: http.MethodDelete, Path: path}, nil)
				}
				if err != nil {
					err = busyFileAdvice(err)
					if i == 0 {
						return nil, nil, fmt.Errorf("could not delete %s: %w", p, err)
					}
					return nil, nil, fmt.Errorf("deleted %s; could not delete %s: %w", strings.Join(in.Paths[:i], ", "), p, err)
				}
			}
			return jsonResult(withVia(t, map[string]any{"deleted": in.Paths}))
		}
		var out json.RawMessage
		err = s.connect.JSON(ctx, connect.Request{Method: http.MethodDelete, Path: printerPath(t.connect.UUID, "files"), JSON: map[string]any{"files": in.Paths}}, &out)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(withVia(t, map[string]any{"deleted": in.Paths, "response": out}))
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "upload_file",
		Description: "Upload a sliced print file (.bgcode/.gcode) from this computer to the printer, optionally " +
			"starting it (then=print) or adding it to the Prusa Connect queue (then=queue). Directly on the local " +
			"network the file goes straight to the printer; through Connect it is stored in Connect and copied to the " +
			"printer in the background (get_transfers shows progress); there then=print puts it first in the queue and " +
			"marks the printer ready, and Connect starts it once the file arrives. then=print is refused unless the " +
			"printer is idle. Before then=print, confirm the plate is clear (a camera snapshot, or the user); plate_clear " +
			"is your own assertion. Uploading through Connect also leaves a " +
			"copy in the team's Connect storage, which delete_connect_files removes.",
		Annotations: mutating("Upload print file", false),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in uploadInput) (*mcp.CallToolResult, any, error) {
		then := strings.ToLower(strings.TrimSpace(in.Then))
		switch then {
		case "":
			then = "none"
		case "none", "print":
		case "queue":
			if err := checkVia(in.printerRef, "connect"); err != nil {
				return nil, nil, fmt.Errorf("then=queue: %w", err)
			}
			in.Via = "connect" // the queue lives in Connect
		default:
			return nil, nil, fmt.Errorf("then must be none, print, or queue (got %q)", in.Then)
		}
		t, err := s.route(ctx, in.printerRef)
		if err != nil {
			return nil, nil, err
		}
		if then == "print" {
			// Check before a possibly long upload, not after.
			if err := s.readyToStart(ctx, t, in.PlateClear); err != nil {
				return nil, nil, err
			}
		}
		if t.direct {
			path, err := s.uploadDirect(ctx, in.LocalPath, in.Destination, in.Filename, in.Overwrite, then == "print")
			if err != nil {
				return nil, nil, err
			}
			out := map[string]any{"uploaded": path, "printing": false}
			if then == "print" {
				for k, v := range startReport(s.startedState(ctx, t)) {
					out[k] = v
				}
			}
			return jsonResult(withVia(t, out))
		}
		p := t.connect
		res, err := s.uploadViaConnect(ctx, p, in.LocalPath, in.Destination, in.Filename)
		if err != nil {
			return nil, nil, err
		}
		out := withVia(t, map[string]any{
			"uploaded": res.Path,
			"hash":     res.Hash, // what delete_connect_files and add_to_queue take
			"printing": false,
			"upload":   res.Upload,
			"file":     res.File,
		})
		if then != "none" {
			body := map[string]any{"hash": res.Hash, "team_id": p.TeamID, "position": -1}
			if then == "print" {
				body["position"] = 0
				body["set_ready"] = true
			}
			var queued json.RawMessage
			if err := s.connect.JSON(ctx, connect.Request{Method: http.MethodPost, Path: printerPath(p.UUID, "queue"), JSON: body}, &queued); err != nil {
				return nil, nil, fmt.Errorf("uploaded (hash %s) but queueing failed: %w", res.Hash, err)
			}
			out["queued"] = queued
			if then == "print" {
				// Through Connect, "print" means first in the queue with the
				// printer marked ready; Connect starts it once the file has been
				// copied over and the printer checks in.
				out["printing"] = false
				out["note"] = "queued first and the printer marked ready; Connect starts it when the file reaches the printer (get_transfers, get_printer)"
			}
		}
		return jsonResult(out)
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "download_printer_file",
		Description: "Copy a file from the printer's storage to this computer, e.g. to inspect the G-code or the " +
			"slicer settings a finished print used. Direct route only: Prusa Connect can't read files off the printer.",
		Annotations: mutating("Download printer file", false),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in downloadInput) (*mcp.CallToolResult, any, error) {
		if !filepath.IsAbs(in.LocalPath) {
			return nil, nil, fmt.Errorf("local_path must be absolute (got %q)", in.LocalPath)
		}
		if err := checkVia(in.printerRef, "direct"); err != nil {
			return nil, nil, err
		}
		in.Via = "direct"
		t, err := s.route(ctx, in.printerRef)
		if err != nil {
			return nil, nil, fmt.Errorf("download_printer_file needs the direct connection: %w", err)
		}
		path, n, err := s.direct().Download(ctx, in.Path, in.LocalPath, in.Overwrite)
		if errors.Is(err, link.ErrExists) {
			return nil, nil, fmt.Errorf("%s already exists; set overwrite to replace it", path)
		}
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(withVia(t, map[string]any{"saved": path, "bytes": n}))
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "start_print",
		Description: "Start printing a file that is already on the printer's storage. The printer must be idle " +
			"and the plate clear: check get_printer and look at get_camera_snapshot first. After a finished or stopped " +
			"print it needs plate_clear=true, which is your own assertion that the plate is empty, so verify it with the " +
			"camera or the user before setting it.",
		Annotations: mutating("Start print", false),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in startPrintInput) (*mcp.CallToolResult, any, error) {
		t, err := s.route(ctx, in.printerRef)
		if err != nil {
			return nil, nil, err
		}
		if err := s.readyToStart(ctx, t, in.PlateClear); err != nil {
			return nil, nil, err
		}
		if t.direct {
			path, err := link.FilePath(in.Path)
			if err != nil {
				return nil, nil, err
			}
			if _, err := s.direct().JSON(ctx, link.Request{Method: http.MethodPost, Path: path}, nil); err != nil {
				if link.IsStatus(err, http.StatusNotFound) {
					// The printer's 404 here carries no message at all.
					return nil, nil, fmt.Errorf("%s has no file at %s; its file listing shows what is there, under the short names it stores them by, e.g. /usb/FIT-GA~2.BGC", t.name, in.Path)
				}
				return nil, nil, err
			}
			out := startReport(s.startedState(ctx, t))
			out["started"] = in.Path
			return jsonResult(withVia(t, out))
		}
		res, err := s.runCommand(ctx, t.connect, "START_PRINT", map[string]any{"path": in.Path}, false, 0)
		if err != nil {
			return nil, nil, err
		}
		// Wait for the printer the same way the direct route does. Connect's
		// record is telemetry-lagged, so reading it once reports the state from
		// before the print started and calls a good start "not printing yet".
		out := startReport(s.startedState(ctx, t))
		out["started"], out["result"] = in.Path, res
		return jsonResult(withVia(t, out))
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "get_queue",
		Description: "The printer's print queue in Prusa Connect: jobs waiting to print, in order, with their ids.",
		Annotations: readOnly("Get print queue"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in pagedRef) (*mcp.CallToolResult, any, error) {
		p, err := s.connectPrinter(ctx, in.printerRef)
		if err != nil {
			return nil, nil, err
		}
		var out json.RawMessage
		if err := s.connect.Get(ctx, printerPath(p.UUID, "queue"), pageQuery(in.Limit, in.Offset, 100), &out); err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]any{"printer": p.Name, "queue": withNextOffset(out)})
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "add_to_queue",
		Description: "Add a file to the printer's Prusa Connect print queue, either one on the printer (path) or " +
			"one in Connect storage (hash). Connect starts the next queued job when the printer is idle and marked " +
			"ready (send_command SET_PRINTER_READY, or set_ready here). Marking it ready says the plate is clear, the " +
			"same assertion as plate_clear, and nothing verifies it: do it only after looking at get_camera_snapshot or " +
			"asking the user.",
		Annotations: mutating("Queue print", false),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in queueInput) (*mcp.CallToolResult, any, error) {
		if (in.Path == "") == (in.Hash == "") {
			return nil, nil, errors.New("pass exactly one of path or hash")
		}
		p, err := s.connectPrinter(ctx, in.printerRef)
		if err != nil {
			return nil, nil, err
		}
		pos := -1
		if in.Position != nil {
			pos = *in.Position
		}
		body := map[string]any{"position": pos}
		if in.Path != "" {
			body["path"] = in.Path
		} else {
			team := in.TeamID
			if team == 0 {
				team = p.TeamID
			}
			body["hash"], body["team_id"] = in.Hash, team
			if in.SetReady {
				body["set_ready"] = true
			}
			if in.WaitUntil > 0 {
				body["wait_until"] = in.WaitUntil
			}
		}
		var out json.RawMessage
		if err := s.connect.JSON(ctx, connect.Request{Method: http.MethodPost, Path: printerPath(p.UUID, "queue"), JSON: body}, &out); err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]any{"printer": p.Name, "queued": out})
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "remove_from_queue",
		Description: "Remove a job from the printer's Prusa Connect print queue. The queue entry is gone for good; add_to_queue puts it back at the end.",
		Annotations: mutating("Remove queued job", true),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in queueJobInput) (*mcp.CallToolResult, any, error) {
		p, err := s.connectPrinter(ctx, in.printerRef)
		if err != nil {
			return nil, nil, err
		}
		path := printerPath(p.UUID, "queue", strconv.FormatInt(in.JobID, 10))
		if err := s.connect.JSON(ctx, connect.Request{Method: http.MethodDelete, Path: path}, nil); err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]any{"printer": p.Name, "removed": in.JobID})
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "get_transfers",
		Description: "File transfers to the printer that are in progress (with progress), plus, through Prusa Connect, files waiting in its download queue.",
		Annotations: readOnly("File transfers"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in printerRef) (*mcp.CallToolResult, any, error) {
		t, err := s.route(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		if t.direct {
			var tr json.RawMessage
			found, err := s.direct().Get(ctx, "/api/v1/transfer", &tr)
			if err != nil {
				return nil, nil, err
			}
			if !found {
				tr = json.RawMessage("null")
			}
			return jsonResult(withVia(t, map[string]any{"transfer": compactTransfer(tr)}))
		}
		var transfers, queue json.RawMessage
		if err := s.connect.Get(ctx, printerPath(t.connect.UUID, "transfers"), nil, &transfers); err != nil {
			return nil, nil, err
		}
		if err := s.connect.Get(ctx, printerPath(t.connect.UUID, "download-queue"), nil, &queue); err != nil {
			return nil, nil, err
		}
		current, err := currentTransfer(transfers)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(withVia(t, map[string]any{
			"transfer":       compactTransfer(current),
			"download_queue": queue,
		}))
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "delete_connect_files",
		Description: "Delete files from Prusa Connect's cloud storage, freeing the team's quota. Takes the hashes " +
			"list_connect_files and upload_file report. Permanent, with no undo, and it removes the file for the whole " +
			"team. delete_printer_files is the equivalent for the printer's own storage.",
		Annotations: mutating("Delete Connect files", true),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in deleteConnectFilesInput) (*mcp.CallToolResult, any, error) {
		if len(in.Hashes) == 0 {
			return nil, nil, errors.New("hashes is empty")
		}
		team, err := s.teamOf(ctx, in.printerRef, in.TeamID)
		if err != nil {
			return nil, nil, err
		}
		path := "/app/teams/" + strconv.FormatInt(team, 10) + "/files/raw"
		body := map[string]any{"hashes": in.Hashes}
		if err := s.connect.JSON(ctx, connect.Request{Method: http.MethodDelete, Path: path, JSON: body}, nil); err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]any{"team_id": team, "deleted": in.Hashes})
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "list_connect_files",
		Description: "Files stored in Prusa Connect's cloud storage for a team, with the hashes add_to_queue takes. " +
			"These count against the team's storage quota; delete_connect_files removes them, and " +
			"delete_printer_files removes files from the printer instead.",
		Annotations: readOnly("List Connect files"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in connectFilesInput) (*mcp.CallToolResult, any, error) {
		team, err := s.teamOf(ctx, in.printerRef, in.TeamID)
		if err != nil {
			return nil, nil, err
		}
		var out json.RawMessage
		path := "/app/teams/" + strconv.FormatInt(team, 10) + "/files"
		if err := s.connect.Get(ctx, path, pageQuery(in.Limit, in.Offset, 50), &out); err != nil {
			return nil, nil, err
		}
		return jsonResult(withNextOffset(out))
	})
}

type uploadResult struct {
	Hash   string
	Path   string // where it landed on the printer, as the direct route reports it
	Upload json.RawMessage
	File   json.RawMessage
}

// upload mirrors the web app: register the upload with its destination, then
// PUT the bytes to the team's raw file endpoint. Connect then copies the file
// to the printer on its own.
// compactFolder trims a PrusaLink folder listing to what an agent needs:
// each entry's full path (the printer's short 8.3 name, which is what
// start_print and delete take), display name, type, and times.
// compactFolder turns PrusaLink's folder listing into one page of entries,
// with the total and, when there is more, the offset of the next page.
func compactFolder(dir string, raw json.RawMessage, limit, offset int) json.RawMessage {
	var folder struct {
		Name     string `json:"name"`
		Children []struct {
			Name        string `json:"name"`
			DisplayName string `json:"display_name"`
			Type        string `json:"type"`
			Size        *int64 `json:"size,omitempty"`
			MTimestamp  int64  `json:"m_timestamp"`
			RO          bool   `json:"ro"`
		} `json:"children"`
	}
	if json.Unmarshal(raw, &folder) != nil || folder.Children == nil {
		return raw
	}
	base := "/" + strings.Trim(dir, "/") + "/"
	total := len(folder.Children)
	switch {
	case limit <= 0:
		limit = 50
	case limit > 500:
		limit = 500
	}
	offset = min(max(offset, 0), total)
	page := folder.Children[offset:min(offset+limit, total)]
	entries := make([]map[string]any, 0, len(page))
	for _, c := range page {
		e := map[string]any{"path": base + c.Name, "name": c.DisplayName, "type": c.Type}
		if c.DisplayName == "" {
			e["name"] = c.Name
		}
		if c.MTimestamp > 0 {
			e["modified"] = time.Unix(c.MTimestamp, 0).Format(time.DateTime)
		}
		if c.Size != nil {
			e["size"] = *c.Size
		}
		if c.RO {
			e["read_only"] = true
		}
		entries = append(entries, e)
	}
	res := map[string]any{"folder": base, "entries": entries, "total": total}
	if next := offset + len(page); next < total {
		res["next_offset"] = next
	}
	b, err := json.Marshal(res)
	if err != nil {
		return raw
	}
	return b
}

// compactConnectFolder turns Connect's folder listing into the same page shape
// compactFolder produces, so both routes answer alike. Connect does the paging
// itself and reports it under "pager"; hashes are kept for add_to_queue.
func compactConnectFolder(raw json.RawMessage) json.RawMessage {
	var folder struct {
		Path  string `json:"path"`
		Files *[]struct {
			Name        string `json:"name"`
			DisplayName string `json:"display_name"`
			Path        string `json:"path"`
			Type        string `json:"type"`
			Hash        string `json:"hash"`
			Size        *int64 `json:"size,omitempty"`
			MTimestamp  int64  `json:"m_timestamp"`
			RO          bool   `json:"read_only"`
		} `json:"files"`
		Pager struct {
			Limit  int `json:"limit"`
			Offset int `json:"offset"`
			Total  int `json:"total"`
		} `json:"pager"`
	}
	if json.Unmarshal(raw, &folder) != nil || folder.Files == nil {
		return raw
	}
	entries := make([]map[string]any, 0, len(*folder.Files))
	for _, f := range *folder.Files {
		e := map[string]any{"path": f.Path, "name": f.DisplayName, "type": f.Type}
		if f.DisplayName == "" {
			e["name"] = f.Name
		}
		if f.MTimestamp > 0 {
			e["modified"] = time.Unix(f.MTimestamp, 0).Format(time.DateTime)
		}
		if f.Size != nil {
			e["size"] = *f.Size
		}
		if f.RO {
			e["read_only"] = true
		}
		if f.Hash != "" {
			e["hash"] = f.Hash
		}
		entries = append(entries, e)
	}
	// Same trailing slash as the direct route, so the two agree exactly.
	res := map[string]any{"folder": strings.TrimSuffix(folder.Path, "/") + "/", "entries": entries, "total": folder.Pager.Total}
	if next := folder.Pager.Offset + len(entries); next < folder.Pager.Total {
		res["next_offset"] = next
	}
	b, err := json.Marshal(res)
	if err != nil {
		return raw
	}
	return b
}

// checkPrintFile validates a local sliced file and settles its printer name.
func checkPrintFile(localPath, filename string) (os.FileInfo, string, error) {
	if !filepath.IsAbs(localPath) {
		return nil, "", fmt.Errorf("local_path must be absolute (got %q)", localPath)
	}
	st, err := os.Stat(localPath)
	if err != nil {
		return nil, "", err
	}
	if !st.Mode().IsRegular() {
		return nil, "", fmt.Errorf("%s is not a regular file", localPath)
	}
	if filename == "" {
		filename = filepath.Base(localPath)
	}
	if strings.ContainsAny(filename, "/\\") {
		return nil, "", fmt.Errorf("filename must not contain slashes (got %q)", filename)
	}
	ext := strings.ToLower(filepath.Ext(filename))
	if ext != ".bgcode" && ext != ".gcode" {
		return nil, "", fmt.Errorf("%s is not a sliced print file (.bgcode or .gcode); slice it first", filename)
	}
	return st, filename, nil
}

// uploadDirect PUTs the file straight to the printer's storage.
func (s *Server) uploadDirect(ctx context.Context, localPath, destination, filename string, overwrite, printAfter bool) (string, error) {
	st, filename, err := checkPrintFile(localPath, filename)
	if err != nil {
		return "", err
	}
	if destination == "" {
		destination = "/usb/"
	}
	target := strings.TrimRight(destination, "/") + "/" + filename
	path, err := link.FilePath(target)
	if err != nil {
		return "", err
	}
	flag := func(b bool) string {
		if b {
			return "?1"
		}
		return "?0"
	}
	_, err = s.direct().JSON(ctx, link.Request{
		Method:        http.MethodPut,
		Path:          path,
		Body:          func() (io.ReadCloser, error) { return os.Open(localPath) },
		ContentLength: st.Size(),
		ContentType:   "application/octet-stream",
		Header:        http.Header{"Overwrite": {flag(overwrite)}, "Print-After-Upload": {flag(printAfter)}},
		Timeout:       30 * time.Minute,
	}, nil)
	if link.IsStatus(err, http.StatusConflict) && !overwrite {
		return "", fmt.Errorf("%s already exists on the printer (pass overwrite=true to replace it): %w", target, err)
	}
	return target, err
}

// uploadViaConnect mirrors the web app: register the upload with its
// destination, then PUT the bytes to the team's raw file endpoint. Connect
// then copies the file to the printer on its own.
func (s *Server) uploadViaConnect(ctx context.Context, p printerSummary, localPath, destination, filename string) (*uploadResult, error) {
	st, filename, err := checkPrintFile(localPath, filename)
	if err != nil {
		return nil, err
	}
	if destination == "" {
		destination, err = s.defaultStorage(ctx, p.UUID)
		if err != nil {
			return nil, err
		}
	}

	var created json.RawMessage
	err = s.connect.JSON(ctx, connect.Request{
		Method: http.MethodPost,
		Path:   "/app/users/teams/" + strconv.FormatInt(p.TeamID, 10) + "/uploads",
		JSON: map[string]any{
			"destination":  destination,
			"filename":     filename,
			"size":         st.Size(),
			"printer_uuid": p.UUID,
		},
	}, &created)
	if err != nil {
		return nil, err
	}
	var meta struct {
		ID   json.RawMessage `json:"id"`
		Hash string          `json:"hash"`
	}
	if err := json.Unmarshal(created, &meta); err != nil || len(meta.ID) == 0 {
		return nil, connect.Missing("POST", "/app/users/teams/{team}/uploads", "id")
	}
	uploadID := strings.Trim(string(meta.ID), `"`)

	resp, err := s.connect.Do(ctx, connect.Request{
		Method: http.MethodPut,
		Path:   "/app/teams/" + strconv.FormatInt(p.TeamID, 10) + "/files/raw",
		Query:  url.Values{"upload_id": {uploadID}},
		Body: func() (io.ReadCloser, error) {
			return os.Open(localPath)
		},
		ContentLength: st.Size(),
		ContentType:   "text/x.gcode", // what PrusaSlicer sends for both .gcode and .bgcode
		Header:        http.Header{"Upload-Size": {strconv.FormatInt(st.Size(), 10)}},
		Timeout:       30 * time.Minute,
	})
	if err != nil {
		// Don't leave a half-finished upload holding the team's quota.
		abort := connect.Request{Method: http.MethodPost, Path: "/app/uploads/" + connect.PathEscape(uploadID) + "/abort"}
		_ = s.connect.JSON(context.WithoutCancel(ctx), abort, nil)
		return nil, err
	}
	defer resp.Body.Close()
	fileJSON, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var file struct {
		Hash string `json:"hash"`
	}
	_ = json.Unmarshal(fileJSON, &file)
	hash := file.Hash
	if hash == "" {
		hash = meta.Hash
	}
	if hash == "" {
		// The file is in Connect, but without its hash it can't be queued or
		// found again.
		return nil, connect.Missing("PUT", "/app/teams/{team}/files/raw", "hash")
	}
	if !json.Valid(fileJSON) {
		fileJSON = nil
	}
	return &uploadResult{Hash: hash, Path: path.Join(destination, filename), Upload: created, File: fileJSON}, nil
}

// defaultStorage returns the path of the printer's first writable storage,
// preferring a USB drive.
func (s *Server) defaultStorage(ctx context.Context, uuid string) (string, error) {
	var resp struct {
		Storages []struct {
			Path       string `json:"path"`
			Mountpoint string `json:"mountpoint"`
			Type       string `json:"type"`
			ReadOnly   bool   `json:"read_only"`
			Corrupted  bool   `json:"corrupted"`
		} `json:"storages"`
	}
	if err := s.connect.Get(ctx, printerPath(uuid, "storages"), nil, &resp); err != nil {
		return "", err
	}
	best, bestUSB := "", false
	for _, st := range resp.Storages {
		path := st.Path
		if path == "" {
			path = st.Mountpoint
		}
		if path == "" || st.ReadOnly || st.Corrupted {
			continue
		}
		if usb := strings.EqualFold(st.Type, "USB"); best == "" || (usb && !bestUSB) {
			best, bestUSB = path, usb
		}
	}
	if best == "" {
		return "", errors.New("the printer reports no writable storage (is a USB drive inserted?)")
	}
	return strings.TrimRight(best, "/") + "/", nil
}
