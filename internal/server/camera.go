package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trevin-lee/prusactl/internal/camera"
	"github.com/trevin-lee/prusactl/internal/hint"
	"github.com/trevin-lee/prusactl/internal/link"
)

// snapshotter grabs one picture from an RTSP camera; camera.Grabber, with
// ffmpeg, in production.
type snapshotter interface {
	Snapshot(ctx context.Context, rtsp string) ([]byte, error)
}

// cameraSnapshot picks where a picture comes from, the way route picks where
// any other call goes: the direct route when it can serve the picture, Prusa
// Connect otherwise or when asked (via=connect).
//
// Buddy firmware's PrusaLink has no camera endpoint: the Buddy3D camera is a
// separate Wi-Fi device that talks to Prusa Connect itself and serves RTSP at
// rtsp://<camera-ip>/live. So locally there are two sources, tried in order:
// the printer's /api/v1/cameras/snap (other PrusaLink builds have one), then
// the saved RTSP address, grabbed with ffmpeg.
func (s *Server) cameraSnapshot(ctx context.Context, in snapshotInput) (*mcp.CallToolResult, error) {
	via := strings.ToLower(strings.TrimSpace(in.Via))
	if via != "" && via != "direct" && via != "connect" {
		return nil, fmt.Errorf("via must be direct or connect (got %q)", in.Via)
	}
	if via == "connect" {
		return s.connectSnapshot(ctx, in)
	}

	// Is the printer the user means the directly connected one? Resolve it as
	// direct only, so a Connect lookup doesn't happen before it's needed.
	ref := in.printerRef
	ref.Via = "direct"
	t, rerr := s.route(ctx, ref)
	var rtsp string
	if s.cameraURL != nil {
		var cerr error
		if rtsp, cerr = s.cameraURL(); cerr != nil {
			return nil, cerr
		}
		// Checked here, not only when it was saved: PRUSACTL_CAMERA_URL is
		// never checked at all otherwise, and what is allowed can change
		// between versions.
		if rtsp != "" {
			if rtsp, cerr = camera.Validate(rtsp); cerr != nil {
				return nil, cerr
			}
		}
	}

	var reason error // why the local sources can't answer
	switch {
	case rerr == nil:
		res, ok, err := s.printerCameraSnap(ctx, in.CameraID, t.name)
		if ok {
			return res, nil
		}
		// The camera is its own device: a printer that errors on its camera
		// endpoint (rebooting, busy) says nothing about it. The error only
		// ends the call when there is nothing else local to try.
		if err != nil && (rtsp == "" || in.CameraID != "") {
			return nil, err
		}
		if rtsp != "" && in.CameraID == "" {
			return s.rtspOrConnect(ctx, in, via, rtsp, t.name)
		}
		reason = noLocalCamera(in.CameraID)
	case rtsp != "" && in.Printer == "" && in.CameraID == "":
		// The camera is its own device on the network: the printer being
		// unreachable says nothing about it.
		return s.rtspOrConnect(ctx, in, via, rtsp, "the printer")
	default:
		reason = rerr
	}

	if via == "direct" {
		return nil, fmt.Errorf("no local camera: %w", reason)
	}
	if !s.session.SignedIn() {
		return nil, fmt.Errorf("no camera to use: %v; or sign in to Prusa Connect, which has the camera (%s)", reason, hint.Connect())
	}
	return s.connectSnapshot(ctx, in)
}

// rtspOrConnect grabs from the camera's RTSP stream and, when that fails and
// the caller didn't insist on a direct source, falls back to Prusa Connect: an
// offline camera or a missing ffmpeg shouldn't hide a picture Connect has.
func (s *Server) rtspOrConnect(ctx context.Context, in snapshotInput, via, rtsp, printerName string) (*mcp.CallToolResult, error) {
	res, err := s.rtspSnapshot(ctx, rtsp, printerName)
	if err == nil || via == "direct" || !s.session.SignedIn() || ctx.Err() != nil {
		return res, err
	}
	res, cerr := s.connectSnapshot(ctx, in)
	if cerr != nil {
		return nil, fmt.Errorf("%w; Prusa Connect didn't have a picture either: %v", err, cerr)
	}
	// Say so. A camera that was skipped looks exactly like one that worked,
	// and the likely cause (ffmpeg on the terminal's PATH but not on the MCP
	// server's) would otherwise never be noticed.
	prefixNote(res, fmt.Sprintf("The RTSP camera was skipped (%v), so this is Prusa Connect's picture. ", err))
	return res, nil
}

// prefixNote puts text in front of the sentence that goes with a picture.
func prefixNote(res *mcp.CallToolResult, text string) {
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			t.Text = text + t.Text
			return
		}
	}
	res.Content = append([]mcp.Content{&mcp.TextContent{Text: strings.TrimSpace(text)}}, res.Content...)
}

// noLocalCamera says how to give prusactl a camera it can reach locally.
func noLocalCamera(cameraID string) error {
	if cameraID != "" {
		return fmt.Errorf("the printer has no camera API, and camera_id only names cameras on it or on Prusa Connect")
	}
	return errors.New("the printer has no camera API (Buddy firmware doesn't; the Buddy3D camera is a separate device) " +
		"and no camera address is saved: " + cameraAdvice())
}

func cameraAdvice() string {
	if hint.Managed() {
		return "enter the camera's RTSP address (rtsp://<camera-ip>/live) in the extension's settings; ffmpeg has to be installed where the app can find it"
	}
	return "run `prusactl setup --camera rtsp://<camera-ip>/live`"
}

// imageResult is a picture and the sentence that goes with it.
func imageResult(img []byte, mime, note string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: note},
		&mcp.ImageContent{Data: img, MIMEType: mime},
	}}
}

// readImage reads a picture from a response, and names its type when the
// server doesn't.
func readImage(resp *http.Response) ([]byte, string, error) {
	img, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, "", err
	}
	mime := resp.Header.Get("Content-Type")
	if mime == "" || mime == "application/octet-stream" {
		mime = http.DetectContentType(img)
	}
	return img, mime, nil
}

// printerCameraSnap asks PrusaLink for a picture. ok is false, without an
// error, when the printer has no such endpoint or no camera; an unreachable or
// unauthorized printer is an error.
func (s *Server) printerCameraSnap(ctx context.Context, cameraID, printerName string) (*mcp.CallToolResult, bool, error) {
	lc := s.direct()
	path := "/api/v1/cameras/snap"
	if cameraID != "" {
		path = "/api/v1/cameras/" + url.PathEscape(cameraID) + "/snap"
	}
	resp, err := lc.Do(ctx, link.Request{Method: http.MethodGet, Path: path, Header: http.Header{"Accept": {"image/jpeg"}}})
	switch {
	case link.IsStatus(err, http.StatusNotFound), link.IsStatus(err, http.StatusMethodNotAllowed):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, false, nil
	}
	img, mime, err := readImage(resp)
	if err != nil {
		return nil, false, err
	}
	return imageResult(img, mime, fmt.Sprintf("Camera on %s, from the printer's own camera API.", printerName)), true, nil
}

// rtspSnapshot grabs one frame from the camera's RTSP stream.
func (s *Server) rtspSnapshot(ctx context.Context, rtsp, printerName string) (*mcp.CallToolResult, error) {
	img, err := s.grabber.Snapshot(ctx, rtsp)
	if err != nil {
		return nil, err
	}
	note := fmt.Sprintf("Camera for %s, one frame just grabbed from %s with ffmpeg.", printerName, camera.Mask(rtsp))
	return imageResult(img, "image/jpeg", note), nil
}
