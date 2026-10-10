package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trevin-lee/prusactl/internal/camera"
)

// fakeGrabber stands in for ffmpeg and records the address it was asked for.
type fakeGrabber struct {
	urls []string
	err  error
}

func (g *fakeGrabber) Snapshot(_ context.Context, rtsp string) ([]byte, error) {
	g.urls = append(g.urls, rtsp)
	if g.err != nil {
		return nil, g.err
	}
	return []byte{0xFF, 0xD8, 0xFF, 0xD9}, nil
}

func withCamera(g *fakeGrabber, rtsp string) func(*Server) {
	return func(s *Server) {
		s.grabber = g
		s.cameraURL = func() (string, error) { return rtsp, nil }
	}
}

// snapshot calls the tool and splits the answer into its text and image.
func snapshot(t *testing.T, cs *mcp.ClientSession, args map[string]any) (text string, image []byte, isErr bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_camera_snapshot", Arguments: args})
	if err != nil {
		return err.Error(), nil, true
	}
	for _, c := range res.Content {
		switch c := c.(type) {
		case *mcp.TextContent:
			text += c.Text
		case *mcp.ImageContent:
			image = c.Data
		}
	}
	return text, image, res.IsError
}

// The Buddy printer's PrusaLink has no camera endpoint, so a saved RTSP
// address is what serves the picture.
func TestSnapshotFromRTSPWhenThePrinterHasNoCameraAPI(t *testing.T) {
	g := &fakeGrabber{}
	fp := &fakePrinter{state: "IDLE"}
	cs := connectToolsWith(t, fp, withCamera(g, "rtsp://10.0.0.9/live"))
	for i := 0; i < 2; i++ {
		text, img, isErr := snapshot(t, cs, nil)
		if isErr || len(img) == 0 {
			t.Fatalf("call %d: %q (error=%v)", i, text, isErr)
		}
		if !strings.Contains(text, "10.0.0.9/live") {
			t.Errorf("the note should name the camera: %q", text)
		}
	}
	if len(g.urls) != 2 || g.urls[0] != "rtsp://10.0.0.9/live" {
		t.Errorf("grabbed %v", g.urls)
	}
}

// An address with credentials is refused wherever it came from, the saved
// config or PRUSACTL_CAMERA_URL, and the refusal repeats neither.
func TestSnapshotRefusesASavedAddressWithCredentials(t *testing.T) {
	g := &fakeGrabber{}
	cs := connectToolsWith(t, &fakePrinter{state: "IDLE"}, withCamera(g, "rtsp://cam:hunter2@10.0.0.9/live"))
	text, _, isErr := snapshot(t, cs, nil)
	if !isErr || !strings.Contains(text, "credentials") || strings.Contains(text, "hunter2") {
		t.Errorf("%q (error=%v)", text, isErr)
	}
	if len(g.urls) != 0 {
		t.Errorf("the address reached ffmpeg: %v", g.urls)
	}
}

// A printer that errors on its camera endpoint (rebooting, busy) says nothing
// about the camera, which is its own device: the RTSP source is still tried.
func TestSnapshotFallsThroughWhenThePrinterCameraErrors(t *testing.T) {
	g := &fakeGrabber{}
	fp := &fakePrinter{state: "IDLE", handler: func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/v1/cameras/snap" {
			return false
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	}}
	cs := connectToolsWith(t, fp, withCamera(g, "rtsp://10.0.0.9/live"))
	if text, img, isErr := snapshot(t, cs, nil); isErr || len(img) == 0 {
		t.Fatalf("%q (error=%v)", text, isErr)
	}
	if len(g.urls) != 1 {
		t.Errorf("RTSP wasn't tried: %v", g.urls)
	}
}

// A PrusaLink that does serve pictures wins over the saved address.
func TestSnapshotPrefersThePrintersOwnCameraAPI(t *testing.T) {
	g := &fakeGrabber{}
	fp := &fakePrinter{state: "IDLE", handler: func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/v1/cameras/snap" {
			return false
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte{0xFF, 0xD8, 0x01, 0xFF, 0xD9})
		return true
	}}
	cs := connectToolsWith(t, fp, withCamera(g, "rtsp://10.0.0.9/live"))
	text, img, isErr := snapshot(t, cs, nil)
	if isErr || len(img) != 5 || len(g.urls) != 0 {
		t.Fatalf("%q image=%d grabbed=%v error=%v", text, len(img), g.urls, isErr)
	}
	if !strings.Contains(text, "printer's own camera API") {
		t.Errorf("the note should say where the picture came from: %q", text)
	}
}

// With nothing local and no Connect, the error says how to get a camera, and
// via=direct says so without suggesting Connect.
func TestSnapshotWithoutACamera(t *testing.T) {
	cs := connectTools(t, &fakePrinter{state: "IDLE"})
	for _, via := range []string{"", "direct"} {
		text, _, isErr := snapshot(t, cs, map[string]any{"via": via})
		if !isErr || !strings.Contains(text, "--camera rtsp://") {
			t.Errorf("via=%q: %q", via, text)
		}
		if via == "" && !strings.Contains(text, "Prusa Connect") {
			t.Errorf("without a route, Connect should be offered: %q", text)
		}
	}
	text, _, isErr := snapshot(t, cs, map[string]any{"via": "connect"})
	if !isErr || !strings.Contains(text, "Prusa Connect") {
		t.Errorf("via=connect: %q", text)
	}
}

func TestSnapshotReportsFFmpegProblems(t *testing.T) {
	g := &fakeGrabber{err: camera.ErrNoFFmpeg}
	cs := connectToolsWith(t, &fakePrinter{state: "IDLE"}, withCamera(g, "rtsp://10.0.0.9/live"))
	text, _, isErr := snapshot(t, cs, nil)
	if !isErr || !strings.Contains(text, "needs ffmpeg") {
		t.Errorf("%q", text)
	}
}

// The camera is its own device: the printer being off doesn't stop the picture.
func TestSnapshotWhenThePrinterIsUnreachable(t *testing.T) {
	g := &fakeGrabber{}
	fp := &fakePrinter{handler: func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	}}
	cs := connectToolsWith(t, fp, withCamera(g, "rtsp://10.0.0.9/live"))
	if text, img, isErr := snapshot(t, cs, nil); isErr || len(img) == 0 {
		t.Fatalf("%q", text)
	}
	// But a named printer isn't assumed to be the one the camera watches.
	if text, _, isErr := snapshot(t, cs, map[string]any{"printer": "another"}); !isErr {
		t.Errorf("another printer got the saved camera: %q", text)
	}
}

// A camera that can't be grabbed doesn't hide Prusa Connect's picture: the
// automatic source order goes on to Connect, and via=direct stops.
func TestSnapshotFallsBackToConnectWhenTheCameraFails(t *testing.T) {
	g := &fakeGrabber{err: errors.New("Connection refused")}
	cs := connectConnectTools(t, &fakeConnect{state: "IDLE"}, withCamera(g, "rtsp://10.0.0.9/live"))
	text, img, isErr := snapshot(t, cs, nil)
	if isErr || len(img) == 0 {
		t.Fatalf("Connect's picture should have come through: %q", text)
	}
	// A camera that was skipped must not look like one that worked.
	if !strings.Contains(text, "Connection refused") || !strings.Contains(text, "Prusa Connect") {
		t.Errorf("the note should say the camera was skipped, and why: %q", text)
	}
	// With nothing on Connect either, both reasons show.
	none := connectConnectTools(t, &fakeConnect{state: "IDLE", noCameras: true}, withCamera(g, "rtsp://10.0.0.9/live"))
	if text, _, isErr := snapshot(t, none, nil); !isErr || !strings.Contains(text, "Connection refused") || !strings.Contains(text, "Prusa Connect didn't have a picture") {
		t.Errorf("the local error and Connect's should both show: %q", text)
	}
	text, _, isErr = snapshot(t, cs, map[string]any{"via": "direct"})
	if !isErr || !strings.Contains(text, "Connection refused") || strings.Contains(text, "Prusa Connect didn't") {
		t.Errorf("via=direct must not try Connect: %q", text)
	}
}
