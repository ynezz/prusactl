package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trevin-lee/prusactl/internal/connect"
	"github.com/trevin-lee/prusactl/internal/hint"
	"github.com/trevin-lee/prusactl/internal/redact"
)

// summaryKeys are the Connect printer fields worth showing in a list;
// get_printer returns everything.
var summaryKeys = []string{
	"uuid", "name", "sn", "location", "team_id", "team_name", "printer_type_name", "printer_model",
	"connect_state", "printer_state", "temp", "chamber", "filament", "nozzle_diameter",
	"speed", "flow", "job_info", "dialog_info", "is_online", "last_online", "firmware",
}

// redactRaw decodes JSON with its credentials masked; input that isn't JSON
// passes through.
func redactRaw(raw json.RawMessage) any {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	return redact.Value(v)
}

type snapshotInput struct {
	printerRef
	CameraID string `json:"camera_id,omitempty" jsonschema:"camera to use when the printer has several; default is the first"`
}

type eventsInput struct {
	pagedRef // newest first; default 20
}

type telemetryInput struct {
	printerRef
	Minutes     int `json:"minutes,omitempty" jsonschema:"how far back to go; default 30"`
	Granularity int `json:"granularity,omitempty" jsonschema:"seconds between samples; default 15"`
}

// directStatus gathers PrusaLink's view of the printer.
func (s *Server) directStatus(ctx context.Context) (map[string]any, error) {
	var status, info, job json.RawMessage
	if _, err := s.direct().Get(ctx, "/api/v1/status", &status); err != nil {
		return nil, err
	}
	if _, err := s.direct().Get(ctx, "/api/v1/info", &info); err != nil {
		return nil, err
	}
	hasJob, err := s.direct().Get(ctx, "/api/v1/job", &job)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"status": redactRaw(status), "info": redactRaw(info), "job": nil}
	if hasJob {
		out["job"] = redactRaw(job)
	}
	// The same few facts in the same place as the Connect route reports them.
	var jobMap map[string]any
	if hasJob {
		jobMap = decode(job)
	}
	out["summary"] = summarizeDirect(decode(status), jobMap, time.Now())
	return out, nil
}

func (s *Server) addPrinterTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "list_printers",
		Description: "The printers prusactl can reach: the one set up for direct access (with whether it answers " +
			"right now) and those on the Prusa Connect account, with state, temperatures, filament, job progress, " +
			"and any dialog on the printer's screen.",
		Annotations: readOnly("List printers"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		out := map[string]any{}
		if s.direct() != nil {
			d := map[string]any{"host": s.direct().Config.Host}
			if info, err := s.probeLink(ctx); err != nil {
				d["reachable"], d["error"] = false, err.Error()
			} else {
				d["reachable"], d["name"], d["serial"] = true, displayName(info, s.direct().Config.Host), info.Serial
			}
			out["direct"] = d
		}
		if s.session.SignedIn() {
			raws, err := s.listPrinters(ctx)
			if err != nil {
				out["connect_error"] = err.Error()
			}
			list := make([]map[string]any, 0, len(raws))
			for _, r := range raws {
				var full map[string]any
				if err := json.Unmarshal(r, &full); err != nil {
					continue
				}
				sum := map[string]any{}
				for _, k := range summaryKeys {
					if v, ok := full[k]; ok && v != nil {
						sum[k] = v
					}
				}
				list = append(list, sum)
			}
			out["connect"] = list
		}
		if len(out) == 0 {
			return nil, nil, fmt.Errorf("nothing is set up yet: %s (direct), or %s", hint.Printer(), hint.Connect())
		}
		return jsonResult(out)
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "get_printer",
		Description: "Live status of the printer. \"summary\" holds the common facts in the same place whichever " +
			"route was used: state, nozzle/bed/chamber temperatures with their targets, and the current job's name, " +
			"progress and time remaining. Everything the route itself reported is kept alongside it, so through Prusa " +
			"Connect that also includes axis positions, fans, speed/flow, filament, nozzle, settings and dialog_info " +
			"(the dialog on the printer's screen, which respond_to_dialog answers), and directly it includes the " +
			"printer's own status, info and job records.",
		Annotations: readOnly("Get printer status"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in printerRef) (*mcp.CallToolResult, any, error) {
		t, err := s.route(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		if t.direct {
			st, err := s.directStatus(ctx)
			if err != nil {
				return nil, nil, err
			}
			return jsonResult(withVia(t, st))
		}
		var out json.RawMessage
		if err := s.connect.Get(ctx, printerPath(t.connect.UUID), nil, &out); err != nil {
			return nil, nil, err
		}
		return jsonResult(withVia(t, map[string]any{
			"status":  redactRaw(out),
			"summary": summarizeConnect(decode(out), time.Now()),
		}))
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "get_camera_snapshot",
		Description: "The latest image from the printer's camera (via Prusa Connect), so you can see the print, " +
			"the bed, and the nozzle. Use it before starting a print or moving anything (is the plate clear?) and " +
			"to watch a print for failures. Reports how old the image is.",
		Annotations: readOnly("Camera snapshot"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in snapshotInput) (*mcp.CallToolResult, any, error) {
		p, err := s.connectPrinter(ctx, in.printerRef)
		if err != nil {
			return nil, nil, err
		}
		var cams struct {
			Cameras *[]map[string]any `json:"cameras"`
		}
		if err := s.connect.Get(ctx, printerPath(p.UUID, "cameras"), nil, &cams); err != nil {
			return nil, nil, err
		}
		if cams.Cameras == nil {
			return nil, nil, connect.Missing("GET", printerPath(p.UUID, "cameras"), "cameras")
		}
		if len(*cams.Cameras) == 0 {
			return nil, nil, fmt.Errorf("%s has no camera in Prusa Connect", p.Name)
		}
		cam := (*cams.Cameras)[0]
		if in.CameraID != "" {
			cam = nil
			for _, c := range *cams.Cameras {
				if fmt.Sprint(c["id"]) == in.CameraID || fmt.Sprint(c["name"]) == in.CameraID {
					cam = c
				}
			}
			if cam == nil {
				return nil, nil, fmt.Errorf("no camera %q on %s", in.CameraID, p.Name)
			}
		}
		// Since Prusa moved cameras to its camera service (2026-09), snapshots
		// come from the URL Connect's GraphQL API gives for the camera, found
		// by its token. The older endpoints stay as a fallback.
		var resp *http.Response
		if token, _ := cam["token"].(string); token != "" {
			if urls, err := s.connect.SnapshotURLs(ctx, p.UUID); err == nil && urls[token] != "" {
				if r, err := s.connect.Do(ctx, connect.Request{Method: http.MethodGet, Path: urls[token]}); err == nil {
					resp = r
				}
			}
		}
		if resp == nil {
			id := connect.PathEscape(jsonID(cam["id"]))
			query := url.Values{"printer_uuid": {p.UUID}}
			resp, err = s.connect.Do(ctx, connect.Request{Method: http.MethodGet, Path: "/app/cameras/" + id + "/snapshots/last", Query: query})
			if connect.IsStatus(err, http.StatusNotFound) {
				// WebRTC cameras (Buddy3D) may never have pushed a full snapshot;
				// the web app falls back to the thumbnail endpoint.
				resp, err = s.connect.Do(ctx, connect.Request{Method: http.MethodGet, Path: "/thumbnail/camera/" + id, Query: query})
			}
			if err != nil {
				return nil, nil, err
			}
		}
		defer resp.Body.Close()
		img, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if err != nil {
			return nil, nil, err
		}
		mime := resp.Header.Get("Content-Type")
		if mime == "" || mime == "application/octet-stream" {
			mime = http.DetectContentType(img)
		}
		note := fmt.Sprintf("Camera %q on %s.", fmt.Sprint(cam["name"]), p.Name)
		if lm, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
			note += fmt.Sprintf(" Taken %s (%s ago).", lm.Local().Format(time.DateTime), time.Since(lm).Round(time.Second))
		}
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: note},
			&mcp.ImageContent{Data: img, MIMEType: mime},
		}}, nil, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "get_telemetry",
		Description: "Recent telemetry history from Prusa Connect: temperatures, fans, speed, and axis positions over time.",
		Annotations: readOnly("Printer telemetry"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in telemetryInput) (*mcp.CallToolResult, any, error) {
		p, err := s.connectPrinter(ctx, in.printerRef)
		if err != nil {
			return nil, nil, err
		}
		minutes, gran := in.Minutes, in.Granularity
		if minutes <= 0 {
			minutes = 30
		}
		if gran <= 0 {
			gran = 15
		}
		q := url.Values{
			"from":        {strconv.FormatInt(time.Now().Add(-time.Duration(minutes)*time.Minute).Unix(), 10)},
			"granularity": {strconv.Itoa(gran)},
		}
		var out json.RawMessage
		if err := s.connect.Get(ctx, printerPath(p.UUID, "telemetry"), q, &out); err != nil {
			return nil, nil, err
		}
		return jsonResult(out)
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "list_events",
		Description: "The printer's event log from Prusa Connect, newest first: state changes, job starts and " +
			"finishes, errors, attention requests, commands, and file transfers. Use it to find out what happened " +
			"while nobody was watching.",
		Annotations: readOnly("Printer events"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in eventsInput) (*mcp.CallToolResult, any, error) {
		p, err := s.connectPrinter(ctx, in.printerRef)
		if err != nil {
			return nil, nil, err
		}
		var out json.RawMessage
		if err := s.connect.Get(ctx, printerPath(p.UUID, "events"), pageQuery(in.Limit, in.Offset, 20), &out); err != nil {
			return nil, nil, err
		}
		return jsonResult(withNextOffset(out))
	})
}

// withNextOffset adds next_offset to a Connect list response when more rows
// remain, so every list says "there is more" the same way the printer's own
// file listings do. Connect reports it inside a "pager" object, which is kept.
func withNextOffset(raw json.RawMessage) json.RawMessage {
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil {
		return raw
	}
	var pager struct {
		Limit  int `json:"limit"`
		Offset int `json:"offset"`
		Total  int `json:"total"`
	}
	if json.Unmarshal(body["pager"], &pager) != nil || pager.Limit <= 0 {
		return raw
	}
	next := pager.Offset + pager.Limit
	if next >= pager.Total {
		return raw
	}
	b, err := json.Marshal(next)
	if err != nil {
		return raw
	}
	body["next_offset"] = b
	out, err := json.Marshal(body)
	if err != nil {
		return raw
	}
	return out
}

// maxPage caps every list, so a huge limit is a page rather than a result too
// big to return.
const maxPage = 500

func pageQuery(limit, offset, def int) url.Values {
	switch {
	case limit <= 0:
		limit = def
	case limit > maxPage:
		limit = maxPage
	}
	if offset < 0 {
		offset = 0
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	return q
}

// jsonID renders a decoded JSON id (float64 or string) for use in a path.
func jsonID(v any) string {
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}
