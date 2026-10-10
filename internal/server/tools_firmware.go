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

// verdict is Prusa Connect's own reading of the running firmware, the one its
// update button goes by: "outdated" means an update is due, "current" that
// Connect calls it supported, and "unknown" anything else, including no state
// at all, which nothing is decided on. Comparing version strings instead would
// call a printer running something newer than Connect's "latest" (a release
// candidate, say) out of date and flash the older build over it.
func (f firmwareFacts) verdict() string {
	switch f.State {
	case "outdated":
		return "outdated"
	case "supported":
		return "current"
	}
	return "unknown"
}

// explicitVersionHint finishes a note that couldn't decide, with what is
// still possible: an update by explicit version is accepted (not "installs":
// the printer can still refuse the flash), and only where Connect offers
// updates for the printer at all.
func explicitVersionHint(canUpdate bool) string {
	if canUpdate {
		return "; an update with an explicit version is still accepted"
	}
	return "; and Prusa Connect doesn't offer firmware updates for this printer"
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
		tr, _ := cur["transferring"].(map[string]any)
		// A path with a transfer still running would be a file the printer is
		// still writing; FLASH on that is a truncated image for the bootloader
		// to refuse.
		if fp := str(cur, "path"); fp != "" && tr == nil {
			return fp, nil
		}
		if planned, _ := cur["planned"].(map[string]any); planned != nil {
			if why := str(planned, "abort_reason"); why != "" || planned["rejected"] == true {
				return "", fmt.Errorf("Prusa Connect could not copy the firmware to the printer (%s)", orDefault(why, "rejected"))
			}
		}
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
		out := map[string]any{"printer": p.Name, "current": f.Current, "latest": f.Latest, "state": f.State, "update_supported": f.CanUpdate}
		switch f.verdict() {
		case "outdated":
			if f.Latest == "" {
				// Outdated, but no version to update to: not "no update", so
				// update_available is left out, the same way as below.
				out["note"] = "Prusa Connect says the firmware is outdated but doesn't say which version is the latest" + explicitVersionHint(f.CanUpdate)
			} else {
				out["update_available"] = true
			}
		case "current":
			out["update_available"] = false
		default:
			// Not "up to date": nobody has said so. update_available is left
			// out rather than guessed, and update_firmware refuses to guess too.
			out["note"] = fmt.Sprintf("Prusa Connect gives no verdict on this firmware (state %q), so whether an update is due is unknown", orDefault(f.State, "none")) + explicitVersionHint(f.CanUpdate)
		}
		return jsonResult(out)
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "update_firmware",
		Description: "Install firmware through Prusa Connect: Connect copies the .bbf it hosts (the latest, or version) to the " +
			"printer's USB drive, then sends the FLASH command, and the printer installs it and restarts. Waits for the copy " +
			"(a few minutes at most). Without version it goes by Prusa Connect's own verdict: installs the latest when " +
			"Connect says the firmware is outdated, does nothing when Connect says it is current, and refuses when there " +
			"is no verdict, so a printer newer than Connect's latest is never downgraded by accident. Refuses before copying " +
			"anything unless the printer is idle, ready, finished or stopped; a print that starts during the copy leaves " +
			"the file on the USB drive, not installed, and the result says so. installed is true only when the printer " +
			"confirmed FLASH. Run it only when the user asked for a firmware update: it restarts the printer, which may " +
			"ask for a confirmation on its screen.",
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
		// Refuse before anything is copied, not only before FLASH: a .bbf
		// written to the USB drive a print is reading from, followed by two
		// minutes of polling and a success exit, is not a refusal.
		// Through connectState, not stateOf: a record with no state is Prusa
		// changing its API, to be reported as such, not a printer to refuse.
		st, err := connectState(detail, p.UUID)
		if err != nil {
			return nil, nil, err
		}
		if !canStart(st) {
			return nil, nil, &stateError{msg: fmt.Sprintf("%s is %s; firmware installs only while the printer is idle, ready, finished or stopped, so nothing was copied. Run this again when it is", p.Name, st)}
		}
		version := strings.TrimSpace(in.Version)
		if version == "" {
			switch f.verdict() {
			case "unknown":
				return nil, nil, fmt.Errorf("Prusa Connect gives no verdict on this printer's firmware (state %q), so whether an update is due is unknown; pass version", orDefault(f.State, "none"))
			case "current":
				return jsonResult(map[string]any{"printer": p.Name, "current": f.Current, "up_to_date": true, "note": "Prusa Connect says this firmware is current; nothing done."})
			}
			if f.Latest == "" {
				return nil, nil, errors.New("Prusa Connect doesn't say which firmware is the latest for this printer; pass version")
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
		case res.State != "FINISHED":
			// Connect answered the sync call, but the event that ended the wait
			// isn't the printer accepting the command. Saying "installed" here
			// would report a refusal as a success.
			out["installed"] = false
			out["command_state"] = res.State
			if res.State == "REJECTED" {
				out["note"] = fmt.Sprintf("Firmware %s is on the printer at %s, but the printer rejected FLASH and did not install it. Check its screen.", version, file)
			} else {
				// CREATED or EMIT is a command still on its way, and the
				// printer may well be flashing; what is true is only that
				// nothing confirmed it.
				out["note"] = fmt.Sprintf("Firmware %s is on the printer at %s. FLASH was sent (%s) but the printer hasn't confirmed installing it: check its screen, and the firmware version once it is back.", version, file, orDefault(res.State, "no outcome reported"))
			}
		default:
			out["installed"] = true
			out["command_state"] = res.State
			out["note"] = fmt.Sprintf("Firmware %s is on the printer at %s. FLASH accepted: the printer installs it and restarts. Check its screen.", version, file)
		}
		return jsonResult(out)
	})
}
