package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trevin-lee/prusactl/internal/connect"
)

// Firmware goes through Prusa Connect only: Connect hosts the .bbf files,
// copies one to the printer's USB drive (the download queue), and tells the
// printer to install it (the FLASH command).

type updateInput struct {
	printerRef
	Version string `json:"version,omitempty" jsonschema:"firmware version to install, e.g. 6.8.1; default is the latest one Connect offers for this printer"`
}

// stripBuild drops the +build suffix, so 6.8.1+16182 equals 6.8.1.
func stripBuild(v string) string {
	v, _, _ = strings.Cut(strings.TrimSpace(v), "+")
	return v
}

// firmwareFacts is what Prusa Connect's printer record says about firmware.
type firmwareFacts struct {
	Current, Latest, State string
	CanUpdate              bool
}

func firmwareOf(detail map[string]any) firmwareFacts {
	f := firmwareFacts{
		Current: str(detail, "support", "current"),
		Latest:  str(detail, "support", "latest"),
		State:   str(detail, "support", "state"),
	}
	if f.Current == "" {
		f.Current = str(detail, "firmware")
	}
	funcs, _ := detail["allowed_functionalities"].([]any)
	f.CanUpdate = slices.Contains(funcs, any("fw_update"))
	return f
}

func (f firmwareFacts) upToDate() bool {
	return f.Latest != "" && stripBuild(f.Current) == stripBuild(f.Latest)
}

// firmwareRecord looks up a firmware file Connect hosts, by version or, when
// the version is empty, as the latest for the printer. The same lookup by hash
// says where the file is on the printer (path) or how its copy is going.
func (s *Server) firmwareRecord(ctx context.Context, uuid string, q url.Values) (map[string]any, error) {
	var rec map[string]any
	if err := s.connect.Get(ctx, printerPath(uuid, "firmware"), q, &rec); err != nil {
		return nil, err
	}
	if str(rec, "hash") == "" {
		return nil, connect.Missing("GET", printerPath(uuid, "firmware"), "hash")
	}
	return rec, nil
}

// progressFunc tells a client that asked (it passed a progress token) how a
// long step is going.
func progressFunc(ctx context.Context, req *mcp.CallToolRequest) func(string) {
	step, last := 0, ""
	return func(msg string) {
		if msg == last { // a poll that shows nothing new
			return
		}
		last = msg
		step++
		if tok := req.Params.GetProgressToken(); tok != nil {
			_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: tok, Progress: float64(step), Message: msg})
		}
	}
}

// copyFirmware has Connect copy the file to the printer and waits until the
// printer reports where it is. It gives up when nothing changes for fwStall.
func (s *Server) copyFirmware(ctx context.Context, progress func(string), p printerSummary, rec map[string]any) (string, error) {
	name := str(rec, "name")
	if name == "" {
		name = str(rec, "display_name")
	}
	dir, err := s.defaultStorage(ctx, p.UUID)
	if err != nil {
		return "", err
	}
	var team int64
	if n := num(rec, "team_id"); n != nil {
		team = int64(*n)
	}
	hash := str(rec, "hash")
	err = s.connect.JSON(ctx, connect.Request{
		Method: http.MethodPost,
		Path:   printerPath(p.UUID, "download-queue"),
		JSON:   map[string]any{"team_id": team, "hash": hash, "path": path.Join(dir, name)},
	}, nil)
	if err != nil {
		return "", err
	}

	q := url.Values{"hash": {hash}, "team_id": {strconv.FormatInt(team, 10)}}
	last, changed := "", time.Now()
	copying := false // Connect reported bytes moving at some point
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(s.fwPoll):
		}
		cur, err := s.firmwareRecord(ctx, p.UUID, q)
		if err != nil {
			return "", err
		}
		if fp := str(cur, "path"); fp != "" {
			return fp, nil
		}
		if planned, _ := cur["planned"].(map[string]any); planned != nil {
			if why := str(planned, "abort_reason"); why != "" || planned["rejected"] == true {
				return "", fmt.Errorf("Prusa Connect could not copy the firmware to the printer (%s)", orDefault(why, "rejected"))
			}
		}
		tr, _ := cur["transferring"].(map[string]any)
		state := fmt.Sprintf("%v/%v", cur["planned"] != nil, tr["transferred"])
		msg := "waiting for the printer to start the download"
		switch {
		case tr != nil:
			copying = true
			msg = fmt.Sprintf("copying to the printer: %.0f%%", derefZero(num(tr, "progress")))
		case copying: // the transfer is over, the printer hasn't reported the file yet
			msg = "finishing the copy"
		}
		progress(msg)
		if state != last {
			last, changed = state, time.Now()
		} else if time.Since(changed) > s.fwStall {
			return "", fmt.Errorf("the printer made no progress on the download for %s; Connect may still have it queued (see get_transfers), and it is not installed", s.fwStall)
		}
	}
}

func derefZero(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (s *Server) addFirmwareTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "get_firmware_status",
		Description: "The firmware the printer runs, the latest one Prusa Connect offers for it, and whether they differ " +
			"(state: supported, outdated or unsupported). Read-only; Prusa Connect only.",
		Annotations: readOnly("Firmware status"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in printerRef) (*mcp.CallToolResult, any, error) {
		p, err := s.connectPrinter(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		detail, err := s.printerDetail(ctx, p.UUID)
		if err != nil {
			return nil, nil, err
		}
		f := firmwareOf(detail)
		out := map[string]any{"printer": p.Name, "current": f.Current, "latest": f.Latest, "state": f.State,
			"update_available": f.Latest != "" && !f.upToDate(), "update_supported": f.CanUpdate}
		return jsonResult(out)
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "update_firmware",
		Description: "Install firmware through Prusa Connect: Connect copies the .bbf it hosts (the latest, or version) to the " +
			"printer's USB drive, then sends the FLASH command, and the printer installs it and restarts. Waits for the copy " +
			"(a few minutes at most). Does nothing when the printer is already on the latest and no version is given. FLASH " +
			"runs only while the printer is idle, ready, finished or stopped: otherwise the file is left on the USB drive, " +
			"not installed, and the result says so; run this again when the print has ended. A printer that is installing " +
			"firmware may ask for a confirmation on its screen.",
		Annotations: mutating("Update firmware", true),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in updateInput) (*mcp.CallToolResult, any, error) {
		p, err := s.connectPrinter(ctx, in.printerRef)
		if err != nil {
			return nil, nil, err
		}
		detail, err := s.printerDetail(ctx, p.UUID)
		if err != nil {
			return nil, nil, err
		}
		f := firmwareOf(detail)
		if !f.CanUpdate {
			return nil, nil, fmt.Errorf("Prusa Connect doesn't offer firmware updates for %s", p.Name)
		}
		version := strings.TrimSpace(in.Version)
		if version == "" {
			if f.Latest == "" {
				return nil, nil, errors.New("Prusa Connect doesn't say which firmware is the latest for this printer; pass version")
			}
			if f.upToDate() {
				return jsonResult(map[string]any{"printer": p.Name, "current": f.Current, "up_to_date": true, "note": "Already on the latest firmware; nothing done."})
			}
			version = stripBuild(f.Latest)
		}
		rec, err := s.firmwareRecord(ctx, p.UUID, url.Values{"version": {version}})
		if connect.IsStatus(err, http.StatusNotFound) {
			return nil, nil, fmt.Errorf("Prusa Connect has no firmware %s for %s", version, p.Name)
		}
		if err != nil {
			return nil, nil, err
		}

		progress := progressFunc(ctx, req)
		out := map[string]any{"printer": p.Name, "current": f.Current, "version": version}
		file := str(rec, "path")
		out["copied"] = file == "" // false: it was on the printer already
		if file == "" {
			if file, err = s.copyFirmware(ctx, progress, p, rec); err != nil {
				return nil, nil, err
			}
		}
		out["file"] = file

		res, err := s.runCommand(ctx, p, "FLASH", map[string]any{"path": file}, false, 0,
			func() { progress("installing (the printer restarts)") })
		var blocked *stateError
		switch {
		case errors.As(err, &blocked):
			out["installed"] = false
			out["note"] = fmt.Sprintf("Firmware %s is on the printer at %s but not installed: %s. Run this again when the printer is idle.", version, file, blocked)
		case err != nil:
			return nil, nil, fmt.Errorf("the firmware is on the printer at %s, but installing it failed: %w", file, err)
		default:
			out["installed"] = true
			out["command_state"] = res.State
			out["note"] = fmt.Sprintf("Firmware %s is on the printer at %s. FLASH sent: the printer installs it and restarts. Check its screen.", version, file)
		}
		return jsonResult(out)
	})
}
