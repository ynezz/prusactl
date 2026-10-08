package server

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trevin-lee/prusactl/internal/auth"
	"github.com/trevin-lee/prusactl/internal/camera"
	"github.com/trevin-lee/prusactl/internal/link"

	"github.com/trevin-lee/prusactl/internal/hint"
)

// Status reports how the printer can be reached, for the CLI and MCP alike.
func (s *Server) Status(ctx context.Context) map[string]any {
	direct := map[string]any{"configured": s.direct() != nil}
	switch {
	case s.direct() != nil:
		direct["host"] = s.direct().Config.Host
		if info, err := s.probeLink(ctx); err != nil {
			direct["reachable"], direct["error"] = false, err.Error()
		} else {
			direct["reachable"], direct["printer"] = true, info
		}
	case errors.Is(s.directErr(), link.ErrNotConfigured):
		direct["setup"] = hint.Printer()
	case s.directErr() != nil:
		direct["error"] = s.directErr().Error()
	}

	cloud := map[string]any{"signed_in": false}
	if s.session.SignedIn() {
		me, err := WhoAmI(ctx, s.connect)
		switch {
		case errors.Is(err, auth.ErrNotLoggedIn):
			cloud["error"] = err.Error()
		case err != nil:
			cloud["signed_in"], cloud["error"] = true, err.Error()
		default:
			cloud["signed_in"], cloud["user"] = true, me
		}
	}
	if cloud["signed_in"] != true {
		cloud["setup"] = "optional, for remote access, camera, dialogs, queue and history: " + hint.Connect()
	}
	cam := map[string]any{"configured": false}
	if s.cameraURL != nil {
		if addr, err := s.cameraURL(); err == nil && addr != "" {
			cam["configured"], cam["rtsp"] = true, camera.Mask(addr)
		}
	}
	return map[string]any{"direct": direct, "connect": cloud, "camera": cam}
}

// ConnectPrinters returns the account's printers as Prusa Connect lists them
// (name, connect_state, temp, ...), for the CLI's status.
func (s *Server) ConnectPrinters(ctx context.Context) ([]map[string]any, error) {
	raws, err := s.listPrinters(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(raws))
	for _, r := range raws {
		var p map[string]any
		if json.Unmarshal(r, &p) == nil {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *Server) addStatusTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "connection_status",
		Description: "How the printer can be reached right now: directly on the local network (PrusaLink) " +
			"and/or through Prusa Connect, which account is signed in, and whether a local camera (RTSP) is saved. " +
			"Says what to set up if neither route works.",
		Annotations: readOnly("Connection status"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		return jsonResult(s.Status(ctx))
	})
}
