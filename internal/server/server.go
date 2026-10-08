// Package server exposes the printer as MCP tools, reaching it directly over
// the local network (PrusaLink) when possible and through Prusa Connect
// otherwise.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trevin-lee/prusactl/internal/auth"
	"github.com/trevin-lee/prusactl/internal/camera"
	"github.com/trevin-lee/prusactl/internal/connect"
	"github.com/trevin-lee/prusactl/internal/link"
	"github.com/trevin-lee/prusactl/internal/redact"

	"github.com/trevin-lee/prusactl/internal/hint"
)

const instructions = `Controls the user's Prusa 3D printer.

Two routes reach it. Direct: the printer's own API on the local network (PrusaLink), used whenever the printer is reachable. Prusa Connect: Prusa's cloud service, used when the printer isn't reachable directly (e.g. the user is away) and for things only Connect offers: camera, on-screen dialogs, the print queue, history, events, and firmware commands such as heating and moving. Tools that can take either route say which one they used ("via"); the rest are named above and always use the route they need.

Start with connection_status or get_printer. Printer arguments accept a name, serial number, or Connect UUID, and can be omitted with a single printer.

Before anything physical (starting a print, moving axes, heating), check get_printer and, if there is a camera, get_camera_snapshot: the plate must be clear and nothing may be in the way. After a finished or stopped print, the tools that start a job or move toward the plate refuse until called with plate_clear=true; pass it only once you (or the user) have confirmed the plate is empty. Marking the printer ready is the same confirmation.

Setup never happens through these tools. `

// instructionsFor ends the server's instructions with setup advice that fits
// where prusactl is running (a terminal, or the MCP bundle's settings).
func instructionsFor() string {
	if hint.Managed() {
		return instructions + "The user sets the printer's address and PrusaLink password in this extension's settings " +
			"(or with \"prusactl setup\", if they have prusactl installed). Prusa Connect can't be signed in to from here, " +
			"but a session saved by an installed prusactl is used automatically; connection_status says whether one is " +
			"available. If a tool says something isn't set up, tell the user which setting to fill in."
	}
	return instructions + `It happens in a terminal: "prusactl setup" for the direct route (the password shown on the printer's screen), ` +
		`"prusactl login" for Prusa Connect. If a tool says one isn't set up, tell the user which command to run.`
}

// Server bundles the MCP server with both routes to the printer.
type Server struct {
	session *auth.Session
	connect *connect.Client
	// The direct route. It is re-read from the saved setup on each probe, so
	// a `prusactl setup` or `setup --forget` run while the server is up takes
	// effect without restarting the MCP client.
	linkMu   sync.Mutex
	link     *link.Client // nil when direct access isn't set up
	linkErr  error        // why link is nil
	openLink func() (*link.Client, error)

	mcp *mcp.Server

	// The camera: its RTSP address (re-read on each use, like the direct
	// route) and what grabs a frame from it.
	cameraURL func() (string, error)
	grabber   snapshotter

	probeMu sync.Mutex
	probed  time.Time
	info    *linkInfo
	infoErr error
}

// New builds the MCP server and registers every tool. lc may be nil, with
// lcErr explaining why.
func New(session *auth.Session, cc *connect.Client, lc *link.Client, lcErr error, version string) *Server {
	s := &Server{session: session, connect: cc, link: lc, linkErr: lcErr, openLink: link.Open, cameraURL: link.LoadCamera, grabber: camera.Grabber{}}
	s.mcp = mcp.NewServer(
		&mcp.Implementation{Name: "prusactl", Title: "Prusa printer", Version: version},
		&mcp.ServerOptions{Instructions: instructionsFor()},
	)
	s.addStatusTools()
	s.addPrinterTools()
	s.addControlTools()
	s.addFileTools()
	s.addJobTools()
	s.addAPITool()
	return s
}

// Connect serves this server over t, for a client in the same process. The CLI
// uses it so its commands go through the very tools an agent calls.
func (s *Server) Connect(ctx context.Context, t mcp.Transport) (*mcp.ServerSession, error) {
	return s.mcp.Connect(ctx, t, nil)
}

// Run serves MCP over stdio until the client disconnects.
func (s *Server) Run(ctx context.Context) error {
	return s.mcp.Run(ctx, &mcp.StdioTransport{})
}

// Annotation presets.
func readOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true}
}

func mutating(title string, destructive bool) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: &destructive}
}

// maxResult caps a tool result's size.
const maxResult = 256 << 10

// jsonResult returns v as compact JSON text.
func jsonResult(v any) (*mcp.CallToolResult, any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, nil, err
	}
	// Every tool result is masked here, so a tool that passes Connect's JSON
	// through can't leak the printer's API keys or camera tokens.
	if masked, changed := redact.JSON(b); changed {
		b = masked
	}
	// A model can't use a result this large, and it would crowd out the
	// conversation; say how to narrow it instead.
	if len(b) > maxResult {
		return nil, nil, fmt.Errorf("the result is %d KB, over the %d KB limit; narrow it (a smaller limit, an offset, or a more specific path)", len(b)>>10, maxResult>>10)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
}

func textResult(format string, args ...any) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: redact.Text(fmt.Sprintf(format, args...))}}}, nil, nil
}

// printerRef is embedded in every per-printer tool input.
type printerRef struct {
	Printer string `json:"printer,omitempty" jsonschema:"printer name, serial number, or Connect UUID; optional with a single printer"`
	Via     string `json:"via,omitempty" jsonschema:"force a route: direct or connect; by default direct is used when the printer is reachable. Tools that only work one way (the camera, dialogs, the queue, history, run_gcode, download_printer_file) refuse the other route rather than ignoring this"`
}

// pagedRef is a printer reference for list tools, so every list pages the
// same way.
type pagedRef struct {
	printerRef
	Limit  int `json:"limit,omitempty" jsonschema:"how many entries to return; at most 500"`
	Offset int `json:"offset,omitempty" jsonschema:"entries to skip, for the next page"`
}

// linkInfo is PrusaLink's /api/v1/info.
type linkInfo struct {
	Name     string `json:"name"`
	Hostname string `json:"hostname"`
	Serial   string `json:"serial"`
	Location string `json:"location"`
}

// probeLink checks whether the printer answers directly, caching the answer
// briefly so a burst of tool calls doesn't wait on an unreachable printer.
func (s *Server) probeLink(ctx context.Context) (*linkInfo, error) {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	ttl := 30 * time.Second
	if s.infoErr != nil {
		ttl = 10 * time.Second
	}
	if !s.probed.IsZero() && time.Since(s.probed) < ttl {
		return s.info, s.infoErr
	}
	if s.reloadLink() {
		s.info, s.infoErr = nil, nil
	}
	lc, lerr := s.lc()
	if lc == nil {
		return nil, lerr
	}
	pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var info linkInfo
	_, err := lc.Get(pctx, "/api/v1/info", &info)
	if ctx.Err() != nil {
		// The caller gave up; that says nothing about the printer, so don't
		// remember it as unreachable.
		return nil, ctx.Err()
	}
	s.probed = time.Now()
	if err != nil {
		s.info, s.infoErr = nil, err
	} else {
		s.info, s.infoErr = &info, nil
	}
	return s.info, s.infoErr
}

// lc returns the direct-route client, or nil and why there is none.
func (s *Server) lc() (*link.Client, error) {
	s.linkMu.Lock()
	defer s.linkMu.Unlock()
	return s.link, s.linkErr
}

// direct returns the direct-route client, or nil when it isn't set up.
func (s *Server) direct() *link.Client {
	lc, _ := s.lc()
	return lc
}

// directErr says why there is no direct-route client.
func (s *Server) directErr() error {
	_, err := s.lc()
	return err
}

// reloadLink re-reads the saved direct setup and reports whether it changed.
func (s *Server) reloadLink() bool {
	if s.openLink == nil {
		return false
	}
	fresh, err := s.openLink()
	s.linkMu.Lock()
	defer s.linkMu.Unlock()
	switch {
	case err != nil && s.link == nil:
		s.linkErr = err
		return false
	case err != nil:
		if errors.Is(err, link.ErrNotConfigured) {
			// `prusactl setup --forget`.
			s.link, s.linkErr = nil, err
			return true
		}
		return false // e.g. the keychain is briefly unavailable: keep what works
	case fresh.Same(s.link):
		return false
	}
	s.link, s.linkErr = fresh, nil
	return true
}

// target is where a tool call goes.
type target struct {
	direct  bool
	name    string
	connect printerSummary // when !direct
	note    string         // why Connect was used although direct is set up
}

func (t target) via() string {
	if t.direct {
		return "direct"
	}
	return "connect"
}

func (i *linkInfo) matches(ref, host string) bool {
	r := strings.ToLower(strings.TrimSpace(ref))
	bare := strings.TrimPrefix(strings.TrimPrefix(host, "http://"), "https://")
	for _, c := range []string{i.Name, i.Hostname, i.Serial, host, bare} {
		if c != "" && strings.ToLower(c) == r {
			return true
		}
	}
	return false
}

// checkVia validates ref.Via for a tool that can only use one route (only is
// "direct" or "connect"), so a forced route is refused rather than ignored.
func checkVia(ref printerRef, only string) error {
	via := strings.ToLower(strings.TrimSpace(ref.Via))
	switch via {
	case "", only:
		return nil
	case "direct", "connect":
		return fmt.Errorf("this only works through %s, so via=%s isn't possible here; leave via out", routeName(only), via)
	}
	return fmt.Errorf("via must be direct or connect (got %q)", ref.Via)
}

func routeName(via string) string {
	if via == "direct" {
		return "the direct connection (PrusaLink)"
	}
	return "Prusa Connect"
}

// route picks direct or Connect for a printer reference.
func (s *Server) route(ctx context.Context, ref printerRef) (target, error) {
	via := strings.ToLower(strings.TrimSpace(ref.Via))
	if via != "" && via != "direct" && via != "connect" {
		return target{}, fmt.Errorf("via must be direct or connect (got %q)", ref.Via)
	}
	var directErr error
	if via != "connect" {
		info, err := s.probeLink(ctx)
		if ctx.Err() != nil {
			return target{}, ctx.Err()
		}
		switch {
		case err != nil:
			directErr = err
		case ref.Printer == "" || info.matches(ref.Printer, s.direct().Config.Host):
			return target{direct: true, name: displayName(info, s.direct().Config.Host)}, nil
		case s.session.SignedIn():
			// Maybe it's the Connect name of the same printer. The printer
			// list omits serial numbers, so read the record.
			if p, cerr := s.resolvePrinter(ctx, ref.Printer); cerr == nil {
				if p.SN == "" {
					var rec struct {
						SN string `json:"sn"`
					}
					if s.connect.Get(ctx, printerPath(p.UUID), nil, &rec) == nil {
						p.SN = rec.SN
					}
				}
				if p.SN != "" && strings.EqualFold(p.SN, info.Serial) {
					return target{direct: true, name: p.Name}, nil
				}
			}
			directErr = fmt.Errorf("the directly connected printer is %q, not %q", displayName(info, s.direct().Config.Host), ref.Printer)
		default:
			directErr = fmt.Errorf("the directly connected printer is %q, not %q", displayName(info, s.direct().Config.Host), ref.Printer)
		}
		if via == "direct" {
			return target{}, directErr
		}
	}
	if !s.session.SignedIn() {
		if via == "connect" {
			return target{}, errors.New("this needs Prusa Connect, which isn't signed in: " + hint.Connect())
		}
		if errors.Is(directErr, link.ErrNotConfigured) || directErr == nil {
			return target{}, fmt.Errorf("no way to reach a printer yet: %s (direct, on your network), or %s", hint.Printer(), hint.Connect())
		}
		return target{}, fmt.Errorf("%v; Prusa Connect isn't signed in either (%s, to reach the printer from anywhere)", directErr, hint.Connect())
	}
	p, err := s.resolvePrinter(ctx, ref.Printer)
	if err != nil {
		return target{}, err
	}
	t := target{name: p.Name, connect: p}
	if directErr != nil && !errors.Is(directErr, link.ErrNotConfigured) {
		t.note = "used Prusa Connect because " + directErr.Error()
	}
	return t, nil
}

func displayName(info *linkInfo, host string) string {
	switch {
	case info.Name != "":
		return info.Name
	case info.Hostname != "":
		return info.Hostname
	}
	return host
}

// withVia adds the route and printer name to a result.
func withVia(t target, fields map[string]any) map[string]any {
	fields["via"] = t.via()
	fields["printer"] = t.name
	if t.note != "" {
		fields["note"] = t.note
	}
	return fields
}

// printerSummary is the subset of a Connect printer record needed to address it.
type printerSummary struct {
	UUID         string `json:"uuid"`
	Name         string `json:"name"`
	SN           string `json:"sn"`
	TeamID       int64  `json:"team_id"`
	ConnectState string `json:"connect_state"`
	PrinterState string `json:"printer_state"`
}

func (s *Server) listPrinters(ctx context.Context) ([]json.RawMessage, error) {
	var page struct {
		Printers *[]json.RawMessage `json:"printers"`
	}
	if err := s.connect.Get(ctx, "/app/printers", nil, &page); err != nil {
		return nil, err
	}
	if page.Printers == nil {
		// Otherwise a renamed list would read as "no printers".
		return nil, connect.Missing("GET", "/app/printers", "printers")
	}
	return *page.Printers, nil
}

// resolvePrinter maps a name, serial or UUID (or nothing, with a single
// printer) to the printer's Connect record.
func (s *Server) resolvePrinter(ctx context.Context, ref string) (printerSummary, error) {
	raws, err := s.listPrinters(ctx)
	if err != nil {
		return printerSummary{}, err
	}
	printers := make([]printerSummary, 0, len(raws))
	for _, r := range raws {
		var p printerSummary
		if err := json.Unmarshal(r, &p); err == nil && p.UUID != "" {
			printers = append(printers, p)
		}
	}
	if len(printers) == 0 && len(raws) > 0 {
		return printerSummary{}, connect.Missing("GET", "/app/printers", "uuid")
	}
	if len(printers) == 0 {
		return printerSummary{}, errors.New("this Prusa Connect account has no printers")
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		if len(printers) == 1 {
			return printers[0], nil
		}
		return printerSummary{}, fmt.Errorf("the account has %d printers; pass printer as one of: %s", len(printers), names(printers))
	}
	for _, p := range printers {
		if strings.EqualFold(p.UUID, ref) || strings.EqualFold(p.Name, ref) || (p.SN != "" && strings.EqualFold(p.SN, ref)) {
			return p, nil
		}
	}
	var partial []printerSummary
	for _, p := range printers {
		if strings.Contains(strings.ToLower(p.Name), strings.ToLower(ref)) {
			partial = append(partial, p)
		}
	}
	if len(partial) == 1 {
		return partial[0], nil
	}
	return printerSummary{}, fmt.Errorf("no printer matches %q; printers: %s", ref, names(printers))
}

// connectPrinter resolves the printer for tools only Connect can serve.
func (s *Server) connectPrinter(ctx context.Context, ref printerRef) (printerSummary, error) {
	if err := checkVia(ref, "connect"); err != nil {
		return printerSummary{}, err
	}
	if !s.session.SignedIn() {
		return printerSummary{}, errors.New("this needs Prusa Connect, which isn't signed in: " + hint.Connect())
	}
	return s.resolvePrinter(ctx, ref.Printer)
}

func names(ps []printerSummary) string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = fmt.Sprintf("%q (%s)", p.Name, p.UUID)
	}
	return strings.Join(out, ", ")
}

func printerPath(uuid string, rest ...string) string {
	p := "/app/printers/" + connect.PathEscape(uuid)
	for _, r := range rest {
		p += "/" + r
	}
	return p
}
