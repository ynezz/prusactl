package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/trevin-lee/prusactl/internal/appdir/appdirtest"
	"github.com/trevin-lee/prusactl/internal/link"
)

// standIn answers the PrusaLink info endpoint like a printer would.
func standIn(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hostname":"stand-in"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runSetup runs `prusactl setup` with the secret piped on stdin.
func runSetup(t *testing.T, secretValue string, args ...string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.WriteString(secretValue + "\n")
	w.Close()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()
	if err := setup(context.Background(), append(args, "--password-stdin")); err != nil {
		t.Fatalf("setup %v: %v", args, err)
	}
}

// Re-running setup must not leave the replaced printer's secret behind, and
// --forget must leave nothing but the lock file.
func TestSetupReplacesAndForgetsSecrets(t *testing.T) {
	home := appdirtest.Use(t)
	t.Setenv("PRUSACTL_KEYRING", "file")
	t.Setenv("PRUSACTL_CONFIG", filepath.Join(home, "config.json"))
	a, b := standIn(t), standIn(t)

	runSetup(t, "pass-a", a.URL)
	runSetup(t, "pass-b", b.URL)
	runSetup(t, "key-b", b.URL, "--api-key")

	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(dir, "prusactl", "secrets.json")
	raw, err := os.ReadFile(store)
	if err != nil {
		t.Fatal(err)
	}
	for _, old := range []string{"pass-a", "pass-b"} {
		if contains(string(raw), old) {
			t.Errorf("a replaced secret (%s) is still stored", old)
		}
	}
	if !contains(string(raw), "key-b") {
		t.Error("the current API key isn't stored")
	}

	if err := setup(context.Background(), []string{"--forget"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store); !os.IsNotExist(err) {
		t.Errorf("secrets.json should be gone after --forget: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// The camera is saved beside the printer, not instead of it: re-running setup
// keeps the camera, --camera alone keeps the printer, and PRUSACTL_CAMERA_URL
// wins over what was saved.
func TestSetupSavesTheCameraBesideThePrinter(t *testing.T) {
	home := appdirtest.Use(t)
	t.Setenv("PRUSACTL_KEYRING", "file")
	t.Setenv("PRUSACTL_CONFIG", filepath.Join(home, "config.json"))
	srv := standIn(t)

	// Only the camera: no printer address, no password asked.
	if err := setup(context.Background(), []string{"--camera", "rtsp://127.0.0.1:1/live"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := link.LoadCamera(); got != "rtsp://127.0.0.1:1/live" {
		t.Fatalf("saved camera %q", got)
	}
	if _, err := link.SavedConfig(); err == nil {
		t.Fatal("saving only the camera set up a printer")
	}

	runSetup(t, "pw", srv.URL)
	if got, _ := link.LoadCamera(); got != "rtsp://127.0.0.1:1/live" {
		t.Errorf("setup dropped the camera: %q", got)
	}
	if cfg, err := link.SavedConfig(); err != nil || cfg.Host != srv.URL {
		t.Errorf("printer %v, %v", cfg, err)
	}

	t.Setenv("PRUSACTL_CAMERA_URL", "rtsp://127.0.0.1:2/live")
	if got, _ := link.LoadCamera(); got != "rtsp://127.0.0.1:2/live" {
		t.Errorf("the environment should win: %q", got)
	}
	t.Setenv("PRUSACTL_CAMERA_URL", "")

	if err := setup(context.Background(), []string{"--no-camera"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := link.LoadCamera(); got != "" {
		t.Errorf("--no-camera left %q", got)
	}
	if _, err := link.SavedConfig(); err != nil {
		t.Errorf("--no-camera forgot the printer: %v", err)
	}

	for _, bad := range [][]string{{"--camera", "http://x/live"}, {"--camera", "x", "--no-camera"}, {"--no-camera", srv.URL}} {
		if err := setup(context.Background(), bad); err == nil {
			t.Errorf("setup %v should be refused", bad)
		}
	}
}

// --forget with no printer set up still removes a camera saved on its own.
func TestSetupForgetRemovesACameraSavedAlone(t *testing.T) {
	home := appdirtest.Use(t)
	t.Setenv("PRUSACTL_KEYRING", "file")
	cfgPath := filepath.Join(home, "config.json")
	t.Setenv("PRUSACTL_CONFIG", cfgPath)

	if err := setup(context.Background(), []string{"--camera", "rtsp://127.0.0.1:1/live"}); err != nil {
		t.Fatal(err)
	}
	if err := setup(context.Background(), []string{"--forget"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := link.LoadCamera(); got != "" {
		t.Errorf("--forget left the camera %q", got)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Errorf("config file should be gone: %v", err)
	}
}
