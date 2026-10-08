// Package camera grabs one picture from a camera that serves RTSP, such as the
// Prusa Buddy3D camera (rtsp://<camera-ip>/live). The camera is a separate
// device on the network, not part of the printer, so PrusaLink can't hand out
// its pictures; ffmpeg does the grabbing.
package camera

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/trevin-lee/prusactl/internal/redact"
)

// Timeout bounds one grab: connecting, waiting for a key frame, decoding.
const Timeout = 30 * time.Second

// maxImage caps the picture read from ffmpeg.
const maxImage = 16 << 20

// ErrNoFFmpeg means the grab needs ffmpeg and it isn't installed.
var ErrNoFFmpeg = errors.New("grabbing a frame from the camera needs ffmpeg, which isn't on PATH; install it (e.g. `brew install ffmpeg` or `apt install ffmpeg`)")

// Grabber runs ffmpeg. The zero value looks ffmpeg up on PATH.
type Grabber struct {
	// Binary is the ffmpeg to run; empty means "ffmpeg" from PATH. Tests point
	// it at a stand-in.
	Binary string
	// Env, when set, is the environment ffmpeg runs in (in addition to the
	// parent's).
	Env []string
}

// ErrRTSPS means the address asks for TLS, which ffmpeg can't be made to check.
var ErrRTSPS = errors.New("rtsps:// cameras aren't supported: ffmpeg does not verify the camera's TLS certificate (its RTSP code never passes -tls_verify or -ca_file on to TLS, tested on 7.1.5), so anyone on the network could pose as the camera and read the password; use the camera's plain rtsp:// address")

// Validate checks that raw is an rtsp:// address, and returns it with the
// scheme in lowercase. An rtsps://
// address is refused, see ErrRTSPS.
func Validate(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err == nil && u.Host != "" && strings.EqualFold(u.Scheme, "rtsps") {
		return "", ErrRTSPS
	}
	if err != nil || u.Host == "" || !strings.EqualFold(u.Scheme, "rtsp") {
		return "", fmt.Errorf("not a camera address: %q (expected rtsp://<camera-ip>/live)", maskAny(raw))
	}
	// ffmpeg only knows the lowercase scheme.
	return "rtsp" + raw[len("rtsp"):], nil
}

// userinfo finds everything between "rtsp://" and the last @ on the line. It is
// greedier than redact's pattern on purpose: an address url.Parse refuses (a
// space, a quote or a slash in the password) has no reliable end for the
// password, so all of it goes, at the cost of hiding some of the host.
var userinfo = regexp.MustCompile(`(?is)(rtsps?://).*@`)

// anyUserinfo is userinfo for whatever the person typed: any scheme, or none.
var anyUserinfo = regexp.MustCompile(`(?is)^([a-z][a-z0-9+.-]*://)?.*@`)

// maskAny hides what comes before the last @ in an address that may not be an
// RTSP address at all (a typo in the scheme must not leak the password).
func maskAny(s string) string {
	return Mask(anyUserinfo.ReplaceAllString(s, "${1}"+redact.Mask+"@"))
}

// Mask hides a password in an RTSP address, for anything shown to a person or
// a model. It works on text that isn't a valid URL too, since the address came
// from a person or a config file and may be malformed.
func Mask(s string) string {
	return redact.Text(userinfo.ReplaceAllString(s, "${1}"+redact.Mask+"@"))
}

// args is ffmpeg's command line: one frame over TCP (UDP loses packets on
// Wi-Fi), as a JPEG on stdout. It is a list, never a shell command. The address
// is plain rtsp://; rtsps:// never gets this far (see ErrRTSPS).
func args(rtsp string) []string {
	return []string{
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-rtsp_transport", "tcp",
		"-i", rtsp,
		"-frames:v", "1", "-q:v", "3",
		"-f", "image2pipe", "-c:v", "mjpeg", "pipe:1",
	}
}

// completeJPEG reports whether img starts with a JPEG's start marker and ends
// with its end marker, so a picture cut short isn't passed on.
func completeJPEG(img []byte) bool {
	n := len(img)
	return n >= 4 && img[0] == 0xFF && img[1] == 0xD8 && img[n-2] == 0xFF && img[n-1] == 0xD9
}

// Snapshot returns one JPEG frame from the RTSP stream at rtsp.
func (g Grabber) Snapshot(ctx context.Context, rtsp string) ([]byte, error) {
	rtsp, err := Validate(rtsp)
	if err != nil {
		return nil, err
	}
	bin := g.Binary
	if bin == "" {
		if bin, err = exec.LookPath("ffmpeg"); err != nil {
			return nil, ErrNoFFmpeg
		}
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args(rtsp)...)
	if len(g.Env) > 0 {
		cmd.Env = append(cmd.Environ(), g.Env...)
	}
	var out, stderr bytes.Buffer
	lw := &limitWriter{w: &out, n: maxImage}
	cmd.Stdout = lw
	cmd.Stderr = &limitWriter{w: &stderr, n: 4 << 10}
	runErr := cmd.Run()
	img := out.Bytes()
	// ffmpeg complains about the frames before the first key frame yet still
	// delivers the picture, so what counts is whether a whole JPEG came out.
	if !lw.over && completeJPEG(img) {
		return img, nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("the camera at %s sent no picture within %s", Mask(rtsp), Timeout)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	msg := strings.TrimSpace(Mask(strings.ReplaceAll(stderr.String(), rtsp, Mask(rtsp))))
	if msg == "" && lw.over {
		msg = fmt.Sprintf("the picture is over %d MB", maxImage>>20)
	}
	if msg == "" && len(img) > 1 && img[0] == 0xFF && img[1] == 0xD8 {
		msg = "ffmpeg's picture was cut short"
	}
	if msg == "" && runErr != nil {
		msg = runErr.Error()
	}
	if msg == "" {
		msg = "ffmpeg produced no picture"
	}
	return nil, fmt.Errorf("couldn't grab a frame from %s: %s", Mask(rtsp), msg)
}

// limitWriter stops accepting bytes after n, so a runaway child can't fill
// memory; the surplus is dropped without an error so the child isn't killed by
// a broken pipe mid-frame.
type limitWriter struct {
	w    *bytes.Buffer
	n    int
	over bool // bytes were dropped
}

func (l *limitWriter) Write(p []byte) (int, error) {
	room := max(l.n-l.w.Len(), 0)
	l.w.Write(p[:min(len(p), room)])
	if len(p) > room {
		l.over = true
	}
	return len(p), nil
}
