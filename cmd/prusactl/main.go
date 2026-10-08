// Command prusactl controls a Prusa printer from the shell or, as an MCP
// server (`prusactl mcp`), from an AI agent. It reaches the printer directly
// on the local network through PrusaLink, and optionally through Prusa
// Connect from anywhere.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/trevin-lee/prusactl/internal/auth"
	"github.com/trevin-lee/prusactl/internal/camera"
	"github.com/trevin-lee/prusactl/internal/compat"
	"github.com/trevin-lee/prusactl/internal/connect"
	"github.com/trevin-lee/prusactl/internal/link"
	"github.com/trevin-lee/prusactl/internal/redact"
	"github.com/trevin-lee/prusactl/internal/secret"
	"github.com/trevin-lee/prusactl/internal/server"
)

var version = "" // set with -ldflags "-X main.version=..."

// buildVersion reports the version the same way however prusactl was built:
// release builds set it through ldflags ("0.1.3"), `go install` reads it from
// the module ("v0.1.3").
func buildVersion() string {
	if version != "" {
		return strings.TrimPrefix(version, "v")
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return strings.TrimPrefix(bi.Main.Version, "v")
	}
	return "dev"
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	compat.Version = buildVersion()
	if err := run(ctx, os.Args[1:]); err != nil {
		// The tools are written for an agent calling them by name; spell their
		// arguments as flags on the way out, once, where a person reads them.
		fmt.Fprintln(os.Stderr, "prusactl:", asFlags(err))
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		printUsage(os.Stdout)
		return nil
	}
	name := args[0]
	switch name {
	case "-h", "--help", "-help":
		name = "help"
	case "-v", "--version", "-version":
		name = "version"
	}
	cmd := lookup(name)
	if cmd == nil {
		return unknownCommand(name)
	}
	rest := args[1:]
	if wantsHelp(rest) {
		cmd.printHelp(os.Stdout)
		return nil
	}

	// Commands that don't need the saved credentials, so they never touch the
	// keychain (which can prompt on macOS).
	switch name {
	case "help":
		return helpCommand(rest)
	case "version":
		fmt.Println("prusactl", buildVersion())
		return nil
	case "completion":
		return completionCommand(rest)
	case "setup":
		return setup(ctx, rest)
	}

	session := auth.NewSession()
	if name == "logout" {
		if err := noArguments(cmd, rest, nil); err != nil {
			return err
		}
		wasSignedIn := session.SignedIn()
		if err := session.Logout(); err != nil {
			return err
		}
		if !wasSignedIn {
			fmt.Println("You weren't signed in to Prusa Connect.")
			return nil
		}
		fmt.Println("Signed out of Prusa Connect; the saved session was removed.")
		return nil
	}
	cc := connect.New(session, "prusactl/"+buildVersion())
	lc, lcErr := link.Open()
	switch name {
	case "login":
		opts, pos, err := cmd.parse(rest)
		if err != nil {
			return err
		}
		if len(pos) > 0 {
			return fmt.Errorf("login: takes no arguments, got %q (see `prusactl help login`)", pos[0])
		}
		return login(ctx, opts, session, cc, lc, lcErr)
	case "status":
		opts, pos, err := cmd.parse(rest)
		if err != nil {
			return err
		}
		if len(pos) > 0 {
			return fmt.Errorf("status: takes no arguments, got %q (see `prusactl help status`)", pos[0])
		}
		if opts.on("json") {
			return statusJSON(ctx)
		}
		return status(ctx, server.New(session, cc, lc, lcErr, buildVersion()), lc)
	case "mcp":
		return noArguments(cmd, rest, func() error {
			return server.New(session, cc, lc, lcErr, buildVersion()).Run(ctx)
		})
	case "api":
		return apiCommand(ctx, cc, lc, lcErr, rest)
	}

	// The printer commands go through the MCP tools, so the CLI and an agent
	// share one implementation and one set of safety checks.
	handlers := map[string]func(context.Context, options, []string) error{
		"printers": printersCmd,
		"print":    printCmd,
		"start":    startCmd,
		"pause":    controlCmd("pause"),
		"resume":   controlCmd("resume"),
		"stop":     controlCmd("stop"),
		"gcode":    gcodeCmd,
		"dialog":   dialogCmd,
		"files":    filesCmd,
		// The old name for `files get`, kept working; it runs the same code.
		"download": func(ctx context.Context, o options, pos []string) error {
			return filesCmd(ctx, o, append([]string{"get"}, pos...))
		},
		"cloud":     cloudCmd,
		"queue":     queueCmd,
		"jobs":      jobsCmd,
		"events":    eventsCmd,
		"telemetry": telemetryCmd,
		"transfers": transfersCmd,
		"camera":    cameraCmd,
		"cmd":       cmdCmd,
	}
	if h, ok := handlers[name]; ok {
		opts, pos, err := cmd.parse(rest)
		if err != nil {
			return err
		}
		return h(ctx, opts, pos)
	}
	return unknownCommand(name)
}

var stdin = bufio.NewReader(os.Stdin)

func ask(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	line, err := stdin.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func askSecret(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("run this in an interactive terminal (or pass --password-stdin)")
	}
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	return string(b), err
}

func setup(ctx context.Context, args []string) error {
	opts, pos, err := lookup("setup").parse(args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return errors.New("setup: usage: prusactl setup [flags] [ADDRESS] (see `prusactl help setup`)")
	}
	user, apiKey, fromStdin := opts.str("user"), opts.on("api-key"), opts.on("password-stdin")
	cameraURL := opts.str("camera")
	if cameraURL != "" {
		var err error
		if cameraURL, err = camera.Validate(cameraURL); err != nil {
			return err
		}
	}
	if opts.on("no-camera") {
		if cameraURL != "" || opts.on("forget") || len(pos) > 0 {
			return errors.New("setup: --no-camera stands alone; it forgets the camera and changes nothing else")
		}
		if err := link.SaveCamera(""); err != nil {
			return err
		}
		fmt.Println("Forgot the saved camera address.")
		return nil
	}
	if cameraURL != "" && len(pos) == 0 && !opts.on("forget") {
		// Only the camera: leave the printer, and its password, alone.
		if err := link.SaveCamera(cameraURL); err != nil {
			return err
		}
		reportCamera(cameraURL)
		return nil
	}
	if opts.on("forget") {
		cfg, err := link.SavedConfig() // what setup saved, not PRUSACTL_* overrides
		if errors.Is(err, link.ErrNotConfigured) {
			// A camera saved on its own, with its credentials, goes too.
			saved, cerr := link.LoadCamera()
			if cerr != nil {
				return cerr
			}
			if err := link.SaveCamera(""); err != nil {
				return err
			}
			if saved != "" && os.Getenv("PRUSACTL_CAMERA_URL") == "" {
				fmt.Println("No printer is set up; forgot the saved camera address.")
			} else {
				fmt.Println("No printer is set up.")
			}
			return nil
		}
		if err != nil {
			return err
		}
		if err := link.RemoveConfig(cfg); err != nil {
			return err
		}
		kind := "password"
		if cfg.Auth == link.AuthAPIKey {
			kind = "API key"
		}
		fmt.Printf("Forgot %s and its saved %s.\n", cfg.Host, kind)
		return nil
	}

	address := ""
	if len(pos) == 1 {
		address = pos[0]
	}
	if address == "" {
		var err error
		if address, err = ask("Printer address (IP or hostname; on the printer: Settings > Network): "); err != nil {
			return err
		}
	}
	host, err := link.NormalizeHost(address)
	if err != nil {
		return err
	}
	cfg := link.Config{Host: host, User: user, Auth: link.AuthDigest}
	what := "PrusaLink password (on the printer: Settings > Network > PrusaLink)"
	if apiKey {
		cfg.Auth, what = link.AuthAPIKey, "PrusaLink API key"
	}

	var pass string
	if fromStdin {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
		if err != nil {
			return err
		}
		pass = strings.TrimSpace(string(b))
	} else if pass, err = askSecret(what + ": "); err != nil {
		return err
	}
	if pass == "" {
		return errors.New("empty password")
	}

	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var info struct {
		Name, Hostname, Serial string
	}
	if _, err := link.New(cfg, pass).Get(vctx, "/api/v1/info", &info); err != nil {
		return fmt.Errorf("checking the printer: %w", err)
	}
	prev, prevErr := link.SavedConfig()
	if err := cfg.SaveSecret(pass); err != nil {
		return fmt.Errorf("saving the printer's secret: %w", err)
	}
	if err := link.SaveConfig(cfg); err != nil {
		return err
	}
	// The direct route holds one printer: remove the secret this setup
	// replaced, so a changed address doesn't leave a password behind.
	if prevErr == nil && !prev.SameSecret(cfg) {
		if err := prev.DeleteSecret(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: couldn't remove the old saved secret for %s: %v\n", prev.Host, err)
		}
	}
	name := info.Name
	if name == "" {
		name = info.Hostname
	}
	kind := "password"
	if apiKey {
		kind = "API key"
	}
	fmt.Printf("Connected to %s at %s. The %s is saved in %s.\n", name, host, kind, secret.Where())
	if prevErr == nil && prev.Host != cfg.Host {
		fmt.Printf("This replaces %s: prusactl reaches one printer directly (others work through Prusa Connect).\n", prev.Host)
	}
	if cameraURL != "" {
		if err := link.SaveCamera(cameraURL); err != nil {
			return err
		}
		reportCamera(cameraURL)
	}
	return nil
}

// reportCamera confirms a saved camera address, and warns about what would
// stop it working: ffmpeg missing, or nothing listening at the address.
func reportCamera(addr string) {
	fmt.Printf("Saved the camera at %s.\n", camera.Mask(addr))
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		fmt.Fprintln(os.Stderr, "warning: ffmpeg isn't on PATH; it grabs the pictures, so install it before using the camera")
	}
	if u, err := url.Parse(addr); err == nil {
		port := u.Port()
		if port == "" {
			port = "554"
		}
		c, err := net.DialTimeout("tcp", net.JoinHostPort(u.Hostname(), port), 3*time.Second)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: nothing answered at %s:%s: %v\n", u.Hostname(), port, err)
			return
		}
		c.Close()
	}
}

// terminalPrompter asks for Prusa Account details on the terminal. host is
// where the password will be sent, which PRUSA_ACCOUNT_URL can change.
type terminalPrompter struct{ host string }

func (terminalPrompter) Email() (string, error) { return ask("Prusa Account email: ") }
func (p terminalPrompter) Password() (string, error) {
	return askSecret(fmt.Sprintf("Password (sent only to %s, never saved): ", p.host))
}
func (terminalPrompter) OneTimeCode() (string, error) {
	return ask("Two-factor code from your authenticator app: ")
}

func login(ctx context.Context, o options, session *auth.Session, cc *connect.Client, lc *link.Client, lcErr error) error {
	if o.given("no-open") && !o.on("manual") {
		return errors.New("login: --no-open only applies to --manual")
	}
	var err error
	if o.on("manual") {
		// The link goes to stdout on a line of its own, so a script or an
		// agent can pick it out; everything around it goes to stderr.
		_, err = session.LoginManual(ctx, func(u string) {
			fmt.Fprintln(os.Stderr, "Open this link in a browser and approve the sign-in to Prusa Connect:")
			fmt.Println(u)
			if !o.on("no-open") && openBrowser(u) == nil {
				fmt.Fprintln(os.Stderr, "(Opened it in your browser.)")
			}
			fmt.Fprintln(os.Stderr, "Then paste the address the browser ended on (https://connect.prusa3d.com/login/auth-callback?code=...) here and press Enter.")
		}, func() (string, error) { return ask("> ") })
	} else {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return errors.New("login: asking for a password needs an interactive terminal; use `prusactl login --manual` and write the final browser address to stdin (see `prusactl help login`)")
		}
		fmt.Fprintln(os.Stderr, "Signing in to Prusa Connect with your Prusa Account.")
		_, err = session.Login(ctx, terminalPrompter{host: accountHost(session)})
	}
	if err != nil {
		return err
	}
	if err := server.RegisterUser(ctx, cc); err != nil {
		fmt.Fprintln(os.Stderr, "warning:", err)
	}
	fmt.Printf("Signed in. The session is saved in %s and renews itself.\n", secret.Where())
	return status(ctx, server.New(session, cc, lc, lcErr, buildVersion()), lc)
}

// openBrowser asks the desktop to show u. It reports whether it could start a
// browser, not whether one appeared.
func openBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

func status(ctx context.Context, srv *server.Server, lc *link.Client) error {
	st := srv.Status(ctx)
	direct, _ := st["direct"].(map[string]any)
	cloud, _ := st["connect"].(map[string]any)

	switch {
	case direct["reachable"] == true:
		var s struct {
			Printer map[string]any `json:"printer"`
			Job     map[string]any `json:"job"`
		}
		line := "reachable"
		if _, err := lc.Get(ctx, "/api/v1/status", &s); err == nil {
			line = fmt.Sprint(s.Printer["state"])
			if p, ok := s.Job["progress"]; ok {
				line += fmt.Sprintf(" %v%%", p)
			}
			line += jobTiming(s.Job, time.Now())
			if _, ok := s.Printer["temp_nozzle"]; ok {
				line += fmt.Sprintf(", nozzle %s/%s°C, bed %s/%s°C", num(s.Printer["temp_nozzle"]),
					num(s.Printer["target_nozzle"]), num(s.Printer["temp_bed"]), num(s.Printer["target_bed"]))
			}
		}
		fmt.Printf("Printer (direct):  %s: %s\n", direct["host"], line)
	case direct["configured"] == true:
		fmt.Printf("Printer (direct):  %s: not reachable (%v)\n", direct["host"], direct["error"])
	default:
		fmt.Printf("Printer (direct):  not set up: %v\n", orText(direct["setup"], direct["error"]))
	}

	if cloud["signed_in"] == true {
		who := "signed in"
		if acct, ok := cloud["user"].(*server.Account); ok && acct.PublicName != "" {
			who = "signed in as " + acct.PublicName
			if acct.Name != "" {
				who += " (" + acct.Name + ")"
			}
		}
		if e, ok := cloud["error"]; ok {
			who += fmt.Sprintf(" (error: %v)", e)
		}
		fmt.Printf("Prusa Connect:     %s\n", who)
	} else {
		fmt.Printf("Prusa Connect:     not signed in (%v)\n", orText(cloud["error"], cloud["setup"]))
	}

	if cam, _ := st["camera"].(map[string]any); cam["configured"] == true {
		fmt.Printf("Camera (RTSP):     %v\n", cam["rtsp"])
	}

	// Without the direct route, show the printers as Prusa Connect sees them.
	if direct["reachable"] != true && cloud["signed_in"] == true {
		printers, err := srv.ConnectPrinters(ctx)
		if err != nil {
			fmt.Printf("Printer (Connect): %v\n", err)
		}
		for _, p := range printers {
			fmt.Printf("Printer (Connect): %v: %s\n", p["name"], connectLine(p))
		}
	}
	return nil
}

// statusJSON is the same two facts the plain output shows, for a script: how
// each route stands, and what the printer itself reports. It runs the same
// tools an agent would, so neither can see something the other can't.
func statusJSON(ctx context.Context) error {
	tc, err := openTools(ctx)
	if err != nil {
		return err
	}
	defer tc.close()
	out := map[string]any{}
	conn, err := tc.call(ctx, "connection_status", map[string]any{})
	if err != nil {
		return err
	}
	for k, v := range decodeMap(conn) {
		out[k] = v
	}
	// A printer that isn't set up or isn't answering is the connection's story
	// to tell, already in out; there is simply no printer to report.
	if printer, err := tc.call(ctx, "get_printer", map[string]any{}); err == nil {
		out["printer"] = decodeMap(printer)
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// connectLine summarizes a printer from Prusa Connect's printer list.
func connectLine(p map[string]any) string {
	line := fmt.Sprint(p["connect_state"])
	if job, ok := p["job_info"].(map[string]any); ok {
		if pr, ok := job["progress"]; ok {
			line += fmt.Sprintf(" %v%%", pr)
		}
		line += jobTiming(job, time.Now())
	}
	if t, ok := p["temp"].(map[string]any); ok {
		line += fmt.Sprintf(", nozzle %s/%s°C, bed %s/%s°C", num(t["temp_nozzle"]), num(t["target_nozzle"]), num(t["temp_bed"]), num(t["target_bed"]))
	}
	return line
}

func orText(a, b any) any {
	if a != nil {
		return a
	}
	return b
}

func apiCommand(ctx context.Context, cc *connect.Client, lc *link.Client, lcErr error, args []string) error {
	// Responses can carry the printer's API keys and camera tokens. Mask them
	// unless asked, so output pasted into a chat or read by an agent doesn't
	// leak them.
	opts, args, err := lookup("api").parse(args)
	if err != nil {
		return err
	}
	raw := opts.on("raw")

	method := http.MethodGet
	if len(args) > 0 && !strings.HasPrefix(args[0], "/") {
		method = strings.ToUpper(args[0])
		args = args[1:]
		switch method {
		case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			return fmt.Errorf("api: unknown method %q; use GET, POST, PUT, PATCH, or DELETE (see `prusactl help api`)", method)
		}
	}
	if len(args) == 0 {
		return errors.New("api: missing PATH, e.g. /api/v1/status (printer) or /app/printers (Connect)")
	}
	u, err := url.Parse(args[0])
	if err != nil {
		return err
	}
	var body []byte
	if len(args) > 1 {
		if !json.Valid([]byte(args[1])) {
			return errors.New("api: body is not valid JSON")
		}
		body = []byte(args[1])
	}

	var resp *http.Response
	switch {
	case strings.HasPrefix(u.Path, "/api/"):
		if lc == nil {
			return lcErr
		}
		req := link.Request{Method: method, Path: u.Path, Query: u.Query()}
		if body != nil {
			req.Body = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
			req.ContentLength, req.ContentType = int64(len(body)), "application/json"
		}
		resp, err = lc.Do(ctx, req)
	case strings.HasPrefix(u.Path, "/app/"):
		req := connect.Request{Method: method, Path: u.Path, Query: u.Query()}
		if body != nil {
			req.JSON = json.RawMessage(body)
		}
		resp, err = cc.Do(ctx, req)
	default:
		return errors.New("api: PATH must start with /api/ (printer) or /app/ (Prusa Connect)")
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	redacted := false
	if !raw {
		if masked, changed := redact.JSON(out); changed {
			out, redacted = masked, true
		} else if !json.Valid(out) {
			masked := redact.Text(string(out))
			out, redacted = []byte(masked), masked != string(out)
		}
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, out, "", "  ") == nil {
		out = pretty.Bytes()
	}
	fmt.Fprintf(os.Stderr, "%d %s\n", resp.StatusCode, http.StatusText(resp.StatusCode))
	os.Stdout.Write(out)
	if len(out) > 0 {
		fmt.Println()
	}
	if redacted {
		fmt.Fprintln(os.Stderr, "(API keys and tokens shown as [redacted]; add --raw to see them)")
	}
	return nil
}

// noArguments runs a command that takes neither flags nor arguments, having
// first turned down anything passed to it. A mistyped --json is worth saying
// out loud: silently ignoring it would hand back the wrong kind of output.
func noArguments(c *command, args []string, run func() error) error {
	_, pos, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("%s: takes no arguments, got %q (see `prusactl help %s`)", c.Name, pos[0], c.Name)
	}
	if run == nil {
		return nil
	}
	return run()
}

// parseFlags parses fs from args and returns the positional arguments. Unlike
// fs.Parse, it also accepts flags after them, as in `setup 10.0.0.5 --api-key`.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// num formats a reading from the printer, or "?" when it didn't send one.
func num(v any) string {
	if v == nil {
		return "?"
	}
	return fmt.Sprint(v)
}

// accountHost is the host the sign-in form will be posted to.
func accountHost(session *auth.Session) string {
	if u, err := url.Parse(session.Config.AccountURL); err == nil && u.Host != "" {
		return u.Host
	}
	return session.Config.AccountURL
}
