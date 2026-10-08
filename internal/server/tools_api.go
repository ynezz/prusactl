package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trevin-lee/prusactl/internal/connect"
	"github.com/trevin-lee/prusactl/internal/link"
	"github.com/trevin-lee/prusactl/internal/redact"
)

type apiInput struct {
	Method string            `json:"method,omitempty" jsonschema:"GET, POST, PUT, PATCH or DELETE; default GET"`
	Path   string            `json:"path" jsonschema:"/api/... for the printer (PrusaLink) or /app/... for Prusa Connect"`
	Query  map[string]string `json:"query,omitempty" jsonschema:"query-string parameters"`
	Body   any               `json:"body,omitempty" jsonschema:"JSON request body"`
}

func (s *Server) addAPITool() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "api_request",
		Description: "Call any printer API endpoint directly, for anything the other tools don't cover. Paths " +
			"starting with /api/ go to the printer itself over the local network (PrusaLink, e.g. /api/v1/status); " +
			"paths starting with /app/ go to Prusa Connect as the signed-in user (e.g. PATCH /app/printers/{uuid} for " +
			"settings, /app/stats/printers/{uuid}/..., /app/teams/{team_id}/files). Credentials in responses are " +
			"redacted. This is a raw escape hatch for what the other tools don't cover, not a shortcut around them: no " +
			"safety checks apply (a busy printer, plate_clear), a DELETE or PUT is permanent and really done, and a " +
			"mistake can start a print, move the head, or change settings. Use start_print, upload_file, run_gcode, " +
			"send_command, or control_print for anything that prints or moves the printer, and GET to look.",
		Annotations: mutating("Raw API call", true),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in apiInput) (*mcp.CallToolResult, any, error) {
		method := strings.ToUpper(strings.TrimSpace(in.Method))
		if method == "" {
			method = http.MethodGet
		}
		switch method {
		case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			return nil, nil, fmt.Errorf("unsupported method %q", in.Method)
		}
		u, err := url.Parse(strings.TrimSpace(in.Path))
		if err != nil {
			return nil, nil, err
		}
		// Only relative API paths: credentials must never be sent elsewhere.
		direct := strings.HasPrefix(u.Path, "/api/")
		if u.Scheme != "" || u.Host != "" || strings.Contains(u.Path, "..") || (!direct && !strings.HasPrefix(u.Path, "/app/")) {
			return nil, nil, fmt.Errorf("path must start with /api/ (printer) or /app/ (Prusa Connect), got %q", in.Path)
		}
		route := "connect"
		if direct {
			route = "direct"
		}
		q := u.Query()
		for k, v := range in.Query {
			q.Set(k, v)
		}
		var resp *http.Response
		if direct {
			if s.direct() == nil {
				return nil, nil, s.directErr()
			}
			req := link.Request{Method: method, Path: u.Path, Query: q}
			if in.Body != nil {
				b, err := json.Marshal(in.Body)
				if err != nil {
					return nil, nil, err
				}
				req.Body = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
				req.ContentLength, req.ContentType = int64(len(b)), "application/json"
			}
			resp, err = s.direct().Do(ctx, req)
		} else {
			req := connect.Request{Method: method, Path: u.Path, Query: q}
			if in.Body != nil {
				req.JSON = in.Body
			}
			resp, err = s.connect.Do(ctx, req)
		}
		if err != nil {
			return nil, nil, err
		}
		defer resp.Body.Close()
		const maxBody = 4 << 20
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		if err != nil {
			return nil, nil, err
		}
		if len(body) > maxBody {
			return nil, nil, fmt.Errorf("the response is over %d MB; narrow the request (e.g. limit/offset)", maxBody>>20)
		}
		ct := resp.Header.Get("Content-Type")
		switch {
		case strings.HasPrefix(ct, "image/"):
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{Data: body, MIMEType: ct}}}, nil, nil
		case len(body) == 0:
			return jsonResult(map[string]any{"status": resp.StatusCode, "via": route})
		case json.Valid(body):
			return jsonResult(map[string]any{"status": resp.StatusCode, "via": route, "body": redactRaw(body)})
		case utf8.Valid(body):
			return jsonResult(map[string]any{"status": resp.StatusCode, "via": route, "content_type": ct, "body": redact.Text(string(body))})
		default:
			return jsonResult(map[string]any{"status": resp.StatusCode, "via": route, "content_type": ct, "bytes": len(body)})
		}
	})
}
