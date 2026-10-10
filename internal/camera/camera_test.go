package camera

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// The test binary doubles as a stand-in ffmpeg: Grabber.Binary points at it and
// FAKE_FFMPEG says how to behave, so the real exec path is what runs.
func TestMain(m *testing.M) {
	switch os.Getenv("FAKE_FFMPEG") {
	case "":
		os.Exit(m.Run())
	case "ok":
		// Record the arguments on stderr (ffmpeg's log channel) and answer a
		// JPEG, with the noise real ffmpeg makes before the first key frame.
		fmt.Fprintln(os.Stderr, "DecodeFrame failed")
		os.Stdout.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0, 'j', 'f', 'i', 'f', 0xFF, 0xD9})
		if os.Getenv("FAKE_ARGS_FILE") != "" {
			os.WriteFile(os.Getenv("FAKE_ARGS_FILE"), []byte(strings.Join(os.Args[1:], "\n")), 0o600)
		}
	case "fail":
		fmt.Fprintln(os.Stderr, "rtsp://cam:hunter2@10.0.0.9/live: Connection refused")
		os.Exit(1)
	case "garbage":
		os.Stdout.Write([]byte("not an image"))
	case "truncated":
		os.Stdout.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0, 'j', 'f', 'i', 'f'})
	case "huge":
		os.Stdout.Write([]byte{0xFF, 0xD8})
		os.Stdout.Write(make([]byte, maxImage))
		os.Stdout.Write([]byte{0xFF, 0xD9})
	case "hang":
		select {}
	}
	os.Exit(0)
}

func TestSnapshotRunsFFmpegWithAnArgumentList(t *testing.T) {
	argsFile := t.TempDir() + "/args"
	g := Grabber{Binary: os.Args[0], Env: []string{"FAKE_FFMPEG=ok", "FAKE_ARGS_FILE=" + argsFile}}
	img, err := g.Snapshot(context.Background(), "rtsp://10.0.0.9/live")
	if err != nil {
		t.Fatal(err)
	}
	if len(img) < 3 || img[0] != 0xFF || img[1] != 0xD8 {
		t.Fatalf("not a JPEG: %x", img)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(string(raw), "\n")
	want := []string{"-rtsp_transport", "tcp", "-i", "rtsp://10.0.0.9/live", "-frames:v", "1"}
	for i := range got {
		if got[i] == want[0] {
			if strings.Join(got[i:i+len(want)], " ") != strings.Join(want, " ") {
				t.Errorf("args %v, want ...%v", got, want)
			}
			return
		}
	}
	t.Errorf("args %v lack %v", got, want)
}

func TestSnapshotErrors(t *testing.T) {
	for _, tc := range []struct {
		name, mode, url, want string
	}{
		{"ffmpeg fails", "fail", "rtsp://10.0.0.9/live", "Connection refused"}, // the fake echoes a password; Mask must catch it
		{"not an image", "garbage", "rtsp://10.0.0.9/live", "ffmpeg produced no picture"},
		{"cut short", "truncated", "rtsp://10.0.0.9/live", "cut short"},
		{"over the cap", "huge", "rtsp://10.0.0.9/live", "over 16 MB"},
		{"not rtsp", "ok", "http://10.0.0.9/live", "not a camera address"},
		{"flag lookalike", "ok", "-i rtsp://x", "not a camera address"},
	} {
		g := Grabber{Binary: os.Args[0], Env: []string{"FAKE_FFMPEG=" + tc.mode}}
		_, err := g.Snapshot(context.Background(), tc.url)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: the camera password leaked: %v", tc.name, err)
		}
	}
}

func TestSnapshotNeedsFFmpeg(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := Grabber{}.Snapshot(context.Background(), "rtsp://10.0.0.9/live")
	if !errors.Is(err, ErrNoFFmpeg) {
		t.Fatalf("got %v, want ErrNoFFmpeg", err)
	}
}

func TestSnapshotGivesUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g := Grabber{Binary: os.Args[0], Env: []string{"FAKE_FFMPEG=hang"}}
	if _, err := g.Snapshot(ctx, "rtsp://10.0.0.9/live"); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want the cancellation", err)
	}
}

// FFmpeg 7.1.5 doesn't check the certificate of an rtsps:// camera even with
// -tls_verify 1 and -ca_file: its RTSP code doesn't pass them on to TLS. So
// rtsps:// is refused before ffmpeg runs.
func TestRTSPSIsRefused(t *testing.T) {
	for _, in := range []string{"rtsps://10.0.0.9/live", "RTSPS://cam:pw@10.0.0.9/live"} {
		if _, err := Validate(in); !errors.Is(err, ErrRTSPS) {
			t.Errorf("Validate(%q) = %v, want ErrRTSPS", in, err)
		}
		if strings.Contains(fmt.Sprint(Validate(in)), "pw") {
			t.Errorf("the refusal shows the password")
		}
		// The fake ffmpeg leaves its arguments in a file when it runs.
		ran := t.TempDir() + "/ran"
		g := Grabber{Binary: os.Args[0], Env: []string{"FAKE_FFMPEG=ok", "FAKE_ARGS_FILE=" + ran}}
		if _, err := g.Snapshot(context.Background(), in); !errors.Is(err, ErrRTSPS) {
			t.Errorf("Snapshot(%q) = %v, want ErrRTSPS", in, err)
		}
		if _, err := os.Stat(ran); err == nil {
			t.Errorf("Snapshot(%q) ran ffmpeg", in)
		}
	}
	if _, err := Validate("rtsp://10.0.0.9/live"); err != nil {
		t.Errorf("plain rtsp refused: %v", err)
	}
}

// ffmpeg doesn't know an uppercase scheme ("Protocol not found"), so Validate
// lowercases it and leaves the rest alone.
func TestValidateLowercasesTheScheme(t *testing.T) {
	for in, want := range map[string]string{
		"RTSP://10.0.0.9/live":    "rtsp://10.0.0.9/live",
		"  RtSp://10.0.0.9/Live ": "rtsp://10.0.0.9/Live",
	} {
		if got, err := Validate(in); err != nil || got != want {
			t.Errorf("Validate(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
}

// An address url.Parse refuses must not leak its password through the error,
// whatever the password holds or the scheme is. A leak shows up as the secret
// itself or as its tail.
func TestValidateErrorsNeverShowAPassword(t *testing.T) {
	for _, in := range []string{
		"rtsp://cam:my secret@10.0.0.9/live",
		`rtsp://cam:pa"ss@10.0.0.9/live`,
		"rtsp://cam:pa/ss@10.0.0.9/live",
		"rtsp://cam:pa\x7fss@10.0.0.9/live",
		"rtsps://cam:se cret@10.0.0.9",
		"http://cam:secret@10.0.0.9/live",
		"cam:secret@10.0.0.9/live",
		"rtsp://cam:secret%zz@10.0.0.9/live",
		"rtsp://cam:pa\nss@10.0.0.9/live",
		"rtsp://cam:pa\r\nss@10.0.0.9/live",
		"rtsp://cam:pa\rss@10.0.0.9/live",
	} {
		_, err := Validate(in)
		if err == nil {
			t.Errorf("Validate(%q) accepted a bad address", in)
			continue
		}
		for _, leak := range []string{"secret", "my sec", "ss@", "pa\"", "pa/", "hun", "ter2", "cam:"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("Validate(%q) error shows %q: %v", in, leak, err)
			}
		}
		if !strings.Contains(err.Error(), "10.0.0.9") {
			t.Errorf("Validate(%q) error lost the host: %v", in, err)
		}
	}
}

// Mask is used on saved addresses and on ffmpeg's output, neither of which is
// guaranteed to be a clean URL.
func TestMaskHidesPasswordsInMalformedAddresses(t *testing.T) {
	for in, want := range map[string]string{
		"rtsp://cam:my secret@10.0.0.9/live":     "rtsp://[redacted]@10.0.0.9/live",
		"rtsp://cam:pa/ss@10.0.0.9/live: failed": "rtsp://[redacted]@10.0.0.9/live: failed",
		`rtsp://cam:pa"ss@10.0.0.9/live`:         "rtsp://[redacted]@10.0.0.9/live",
		"rtsp://10.0.0.9/live":                   "rtsp://10.0.0.9/live",
		"rtsp://cam:pa\nss@10.0.0.9/live":        "rtsp://[redacted]@10.0.0.9/live",
		"rtsp://cam:pa\r\nss@10.0.0.9/live":      "rtsp://[redacted]@10.0.0.9/live",
	} {
		if got := Mask(in); got != want {
			t.Errorf("Mask(%q) = %q, want %q", in, got, want)
		}
	}
}

// Credentials in the address are refused rather than handled: a password would
// reach ffmpeg as an argument, readable by any local user with ps, and sit in
// plain text in the config. The Buddy3D needs none. The refusal says what to
// use instead and never repeats the credentials.
func TestValidateRefusesCredentials(t *testing.T) {
	for _, in := range []string{"rtsp://cam:hunter2@10.0.0.9/live", "rtsp://cam@10.0.0.9/live"} {
		_, err := Validate(in)
		if !errors.Is(err, ErrCredentials) {
			t.Errorf("Validate(%q) = %v, want ErrCredentials", in, err)
			continue
		}
		if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "cam:") || strings.Contains(err.Error(), "cam@") {
			t.Errorf("the refusal shows the credentials: %v", err)
		}
		if !strings.Contains(err.Error(), "rtsp://10.0.0.9/live") {
			t.Errorf("the refusal should say what to use instead: %v", err)
		}
		ran := t.TempDir() + "/ran"
		g := Grabber{Binary: os.Args[0], Env: []string{"FAKE_FFMPEG=ok", "FAKE_ARGS_FILE=" + ran}}
		if _, err := g.Snapshot(context.Background(), in); !errors.Is(err, ErrCredentials) {
			t.Errorf("Snapshot(%q) = %v, want ErrCredentials", in, err)
		}
		if _, err := os.Stat(ran); err == nil {
			t.Errorf("Snapshot(%q) ran ffmpeg", in)
		}
	}
}
