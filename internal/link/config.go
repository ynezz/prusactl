package link

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/trevin-lee/prusactl/internal/secret"

	"github.com/trevin-lee/prusactl/internal/appdir"

	"github.com/trevin-lee/prusactl/internal/hint"
)

// ErrNotConfigured means no printer has been set up for direct access.
var ErrNotConfigured = errors.New("no printer set up for direct access: " + hint.Printer())

// Auth modes PrusaLink supports.
const (
	AuthDigest = "digest"  // username + password from the printer's screen
	AuthAPIKey = "api-key" // X-Api-Key header
)

// Config is the saved printer address. The secret lives in the OS keychain.
type Config struct {
	Host string `json:"host"`           // e.g. http://192.168.8.162
	User string `json:"user,omitempty"` // Digest username, default "maker"
	Auth string `json:"auth,omitempty"` // AuthDigest (default) or AuthAPIKey
}

type fileFormat struct {
	Printer *Config `json:"printer,omitempty"`
	// Camera is the RTSP address of the printer's camera, e.g.
	// rtsp://192.168.8.60/live. It is a separate device, so it is saved apart
	// from the printer.
	Camera string `json:"camera,omitempty"`
}

// readFile reads the config file; a missing one is empty.
func readFile() (fileFormat, string, error) {
	var f fileFormat
	path, err := configPath()
	if err != nil {
		return f, "", err
	}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return f, path, nil
	case err != nil:
		return f, path, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, path, fmt.Errorf("reading %s: %w", path, err)
	}
	return f, path, nil
}

func writeFile(path string, f fileFormat) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// LoadCamera returns the camera's RTSP address: PRUSACTL_CAMERA_URL if set,
// else the one saved by `prusactl setup --camera`, else "".
func LoadCamera() (string, error) {
	if v := env("PRUSACTL_CAMERA_URL"); v != "" {
		return v, nil
	}
	f, _, err := readFile()
	return f.Camera, err
}

// SaveCamera saves the camera's address, or forgets it when addr is empty,
// leaving the saved printer alone.
func SaveCamera(addr string) error {
	f, path, err := readFile()
	if err != nil {
		return err
	}
	f.Camera = addr
	if f.Printer == nil && addr == "" {
		// Nothing left to keep.
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	return writeFile(path, f)
}

// NormalizeHost turns "192.168.8.162" or "prusa.local:8080" into an origin.
func NormalizeHost(h string) (string, error) {
	h = strings.TrimSpace(h)
	if h == "" {
		return "", errors.New("empty printer address")
	}
	if !strings.Contains(h, "://") {
		h = "http://" + h
	}
	u, err := url.Parse(h)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("not a printer address: %q", h)
	}
	return u.Scheme + "://" + u.Host, nil
}

// env reads an override, ignoring blanks and unfilled "${...}" placeholders
// (an MCP bundle passes those through when an optional setting is left empty).
func env(name string) string {
	v := strings.TrimSpace(os.Getenv(name))
	if strings.HasPrefix(v, "${") {
		return ""
	}
	return v
}

func configPath() (string, error) {
	if p := os.Getenv("PRUSACTL_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := appdir.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// LoadConfig reads the saved printer, applying PRUSACTL_HOST / PRUSACTL_USER /
// PRUSACTL_AUTH overrides. It returns ErrNotConfigured if there is none.
func LoadConfig() (Config, error) {
	f, _, err := readFile()
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if f.Printer != nil {
		cfg = *f.Printer
	}
	if v := env("PRUSACTL_HOST"); v != "" {
		cfg.Host = v
	}
	if v := env("PRUSACTL_USER"); v != "" {
		cfg.User = v
	}
	if v := env("PRUSACTL_AUTH"); v != "" {
		cfg.Auth = v
	}
	if cfg.Host == "" {
		return cfg, ErrNotConfigured
	}
	if cfg.Host, err = NormalizeHost(cfg.Host); err != nil {
		return cfg, err
	}
	if cfg.User == "" {
		cfg.User = "maker"
	}
	if cfg.Auth == "" {
		cfg.Auth = AuthDigest
	}
	return cfg, nil
}

// SaveConfig writes the printer address (never the secret).
func SaveConfig(cfg Config) error {
	f, path, err := readFile()
	if path == "" {
		return err
	}
	// An unreadable file is replaced, as before; a readable one keeps the
	// camera saved beside the printer.
	f.Printer = &cfg
	return writeFile(path, f)
}

// SavedConfig returns the printer saved by `prusactl setup`, ignoring the
// PRUSACTL_* overrides, or ErrNotConfigured.
func SavedConfig() (Config, error) {
	f, _, err := readFile()
	if err != nil {
		return Config{}, err
	}
	if f.Printer == nil || f.Printer.Host == "" {
		return Config{}, ErrNotConfigured
	}
	cfg := *f.Printer
	if cfg.User == "" {
		cfg.User = "maker"
	}
	if cfg.Auth == "" {
		cfg.Auth = AuthDigest
	}
	return cfg, nil
}

// SameSecret reports whether two configs keep their secret in the same place.
func (cfg Config) SameSecret(o Config) bool { return cfg.secretAccount() == o.secretAccount() }

// DeleteSecret removes the stored password or API key for cfg. It is not an
// error if there was none.
func (cfg Config) DeleteSecret() error { return secret.Delete(cfg.secretAccount()) }

// RemoveConfig forgets the printer and its secret.
func RemoveConfig(cfg Config) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	errs := []error{}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	if err := secret.Delete(cfg.secretAccount()); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (cfg Config) secretAccount() string {
	if cfg.Auth == AuthAPIKey {
		return "printer-api-key:" + cfg.Host
	}
	return "printer-password:" + cfg.Host + ":" + cfg.User
}

// Secret returns the printer password or API key: PRUSACTL_PASSWORD /
// PRUSACTL_API_KEY if set, else the stored one.
func (cfg Config) Secret() (string, error) {
	name := "PRUSACTL_PASSWORD"
	if cfg.Auth == AuthAPIKey {
		name = "PRUSACTL_API_KEY"
	}
	if v := env(name); v != "" {
		return v, nil
	}
	s, err := secret.Get(cfg.secretAccount())
	if errors.Is(err, secret.ErrNotFound) {
		return "", fmt.Errorf("no saved password for %s: %s", cfg.Host, hint.Printer())
	}
	return s, err
}

// SaveSecret stores the printer password or API key (see package secret).
func (cfg Config) SaveSecret(value string) error {
	return secret.Set(cfg.secretAccount(), value)
}
