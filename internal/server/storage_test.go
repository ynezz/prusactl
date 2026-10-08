package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/trevin-lee/prusactl/internal/appdir/appdirtest"
	"github.com/trevin-lee/prusactl/internal/auth"
	"github.com/trevin-lee/prusactl/internal/connect"
	"github.com/trevin-lee/prusactl/internal/link"
)

// connectServer returns a Server whose Connect client talks to h.
func connectServer(t *testing.T, h http.Handler) *Server {
	t.Helper()
	appdirtest.Use(t) // the refresh lock file lives under the config dir
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	session := &auth.Session{Store: signedIn{}}
	cc := connect.New(session, "test")
	cc.BaseURL = srv.URL
	s := New(session, cc, nil, link.ErrNotConfigured, "test")
	s.openLink = func() (*link.Client, error) { return nil, link.ErrNotConfigured }
	return s
}

func TestDefaultStorage(t *testing.T) {
	tests := []struct {
		name     string
		storages string
		want     string
		wantErr  string
	}{
		{"prefers USB over the first writable", `[{"path":"/local","type":"LOCAL"},{"path":"/usb","type":"USB"}]`, "/usb/", ""},
		{"USB type is case-insensitive", `[{"path":"/local","type":"local"},{"path":"/usb","type":"usb"}]`, "/usb/", ""},
		{"skips read-only USB", `[{"path":"/local","type":"LOCAL"},{"path":"/usb","type":"USB","read_only":true}]`, "/local/", ""},
		{"skips corrupted USB", `[{"path":"/usb","type":"USB","corrupted":true},{"path":"/local","type":"LOCAL"}]`, "/local/", ""},
		{"falls back to the first writable", `[{"path":"/a","type":"LOCAL"},{"path":"/b","type":"SD"}]`, "/a/", ""},
		{"uses mountpoint without a path", `[{"mountpoint":"/usb/","type":"USB"}]`, "/usb/", ""},
		{"skips entries without a path", `[{"type":"USB"},{"path":"/local","type":"LOCAL"}]`, "/local/", ""},
		{"no writable storage", `[{"path":"/usb","type":"USB","read_only":true}]`, "", "no writable storage"},
		{"no storage at all", `[]`, "", "no writable storage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := connectServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/app/printers/u1/storages" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"storages":%s}`, tt.storages)
			}))
			got, err := s.defaultStorage(context.Background(), "u1")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("got %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

// A command refused for the printer's state is a *stateError, so a caller can
// tell it from any other failure.
func TestRunCommandStateError(t *testing.T) {
	tests := []struct {
		state    string
		wantType bool
	}{
		{"PRINTING", true},
		{"IDLE", false}, // allowed: it is sent, and the fake accepts it
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			s := connectServer(t, &fakeConnect{state: tt.state})
			p := printerSummary{UUID: "u1", Name: "Core One", TeamID: 1}
			_, err := s.runCommand(context.Background(), p, "HOME", nil, false, 0)
			var se *stateError
			if got := errors.As(err, &se); got != tt.wantType {
				t.Fatalf("errors.As(%v) = %v, want %v", err, got, tt.wantType)
			}
			if tt.wantType && !strings.Contains(err.Error(), "can't run HOME while PRINTING") {
				t.Errorf("message = %q", err)
			}
			if !tt.wantType && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}
