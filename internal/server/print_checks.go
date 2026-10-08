package server

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// plateConfirmation is embedded in every tool input that can start a job or
// move toward the plate, so the rule has one field and one meaning everywhere.
// Marking the printer ready (SET_PRINTER_READY, add_to_queue's set_ready) is
// Prusa's own form of the same confirmation.
type plateConfirmation struct {
	PlateClear bool `json:"plate_clear,omitempty" jsonschema:"your own assertion that the plate is empty; nothing checks it, so make it true: look at get_camera_snapshot and read the image, or take the user's word. Required when the printer is FINISHED or STOPPED, since the last print may still be there. Never set it to get past the refusal"`
}

// plateCommands are firmware commands that start a job or move the head or
// bed where a part left from the last print would be hit.
var plateCommands = map[string]bool{
	"START_PRINT":       true,
	"HOME":              true,
	"MOVE":              true,
	"MOVE_Z":            true,
	"MESH_BED_LEVELING": true,
}

func needsPlateCheck(command string) bool {
	return plateCommands[strings.ToUpper(strings.TrimSpace(command))]
}

// plateErr is the rule itself: after a finished or stopped print, someone
// has to have looked at the plate.
func plateErr(name, state string, plateClear bool) error {
	if (state == "FINISHED" || state == "STOPPED") && !plateClear {
		return fmt.Errorf("%s is %s, so the last print may still be on the plate. Look first (get_camera_snapshot, "+
			"or ask the user), then call again with plate_clear=true", name, state)
	}
	return nil
}

// plateCheck applies the plate rule alone, for commands whose other state
// requirements the printer's supported-command list already covers.
func (s *Server) plateCheck(ctx context.Context, t target, plateClear bool) error {
	state, err := s.printerState(ctx, t)
	if err != nil {
		return fmt.Errorf("checking %s's plate state: %w", t.name, err)
	}
	return plateErr(t.name, state, plateClear)
}

// canStart is what the firmware itself accepts a remote print from (see
// printer_state::remote_print_ready in Prusa-Firmware-Buddy). FINISHED and
// STOPPED may still have the last part on the plate, which only a look at the
// printer or camera can rule out; the tool descriptions ask for that.
func canStart(state string) bool {
	switch state {
	case "IDLE", "READY", "FINISHED", "STOPPED":
		return true
	}
	return false
}

// printerState reads the printer's state over the route in use.
func (s *Server) printerState(ctx context.Context, t target) (string, error) {
	if t.direct {
		var st struct {
			Printer struct {
				State string `json:"state"`
			} `json:"printer"`
		}
		if _, err := s.direct().Get(ctx, "/api/v1/status", &st); err != nil {
			return "", err
		}
		return st.Printer.State, nil
	}
	p, err := s.printerDetail(ctx, t.connect.UUID)
	if err != nil {
		return "", err
	}
	return connectState(p, t.connect.UUID)
}

// readyToStart refuses a print or G-code the printer can't take, before
// anything is uploaded or queued. After a finished or stopped print the part
// may still be on the plate, where a new print or a homing move would hit it,
// so those states also need plateClear: an explicit statement that someone
// looked.
func (s *Server) readyToStart(ctx context.Context, t target, plateClear bool) error {
	state, err := s.printerState(ctx, t)
	if err != nil {
		return fmt.Errorf("checking %s before printing: %w", t.name, err)
	}
	if !canStart(state) {
		hint := ""
		switch state {
		case "OFFLINE":
			hint = "; Prusa Connect can't reach it right now"
		case "ATTENTION":
			hint = "; a question is waiting on its screen, and must be answered before it will take a job"
		case "PRINTING", "PAUSED", "BUSY":
			hint = "; wait for it to finish, or add the file to the Prusa Connect queue"
		}
		return fmt.Errorf("%s is %s, so it can't take a new job%s", t.name, state, hint)
	}
	return plateErr(t.name, state, plateClear)
}

// startedState waits briefly for a print just started directly to leave the
// idle states, and returns the state the printer settled in. The firmware
// accepts Print-After-Upload but may still stop on a question on its screen.
func (s *Server) startedState(ctx context.Context, t target) string {
	state := ""
	for i := 0; i < 10; i++ {
		if st, err := s.printerState(ctx, t); err == nil {
			state = st
			if !canStart(st) {
				return st
			}
		}
		select {
		case <-ctx.Done():
			return state
		case <-time.After(time.Second):
		}
	}
	return state
}

// startReport describes the outcome of starting a print directly.
func startReport(state string) map[string]any {
	out := map[string]any{"printer_state": state, "printing": state == "PRINTING"}
	switch {
	case state == "ATTENTION":
		out["note"] = "the printer stopped on a question on its screen before printing; it must be answered for the job to go on"
	case canStart(state):
		out["note"] = "the printer hasn't started yet; check its state again in a moment"
	}
	return out
}
