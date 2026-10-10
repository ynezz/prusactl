package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trevin-lee/prusactl/internal/auth"
	"github.com/trevin-lee/prusactl/internal/connect"
	"github.com/trevin-lee/prusactl/internal/link"
	"github.com/trevin-lee/prusactl/internal/server"
)

// The printer commands call the very tools the MCP server exposes, over an
// in-memory connection. The CLI and an agent therefore reach the printer by the
// same code path, with the same safety checks, and neither can gain a
// capability the other lacks.

type toolClient struct {
	session *mcp.ClientSession
	close   func()
}

func openTools(ctx context.Context) (*toolClient, error) {
	session := auth.NewSession()
	cc := connect.New(session, "prusactl/"+buildVersion())
	lc, lcErr := link.Open()
	srv := server.New(session, cc, lc, lcErr, buildVersion())

	clientT, serverT := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, serverT); err != nil {
		return nil, err
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "prusactl-cli", Version: buildVersion()}, &mcp.ClientOptions{
		// A tool that waits on the printer says how far it is.
		ProgressNotificationHandler: func(_ context.Context, r *mcp.ProgressNotificationClientRequest) {
			fmt.Fprintln(os.Stderr, r.Params.Message)
		},
	}).Connect(ctx, clientT, nil)
	if err != nil {
		return nil, err
	}
	return &toolClient{session: cs, close: func() { cs.Close() }}, nil
}

// call runs a tool and returns its JSON result. A tool that reports an error
// returns it as an error, so the CLI exits non-zero with the tool's own message.
func (t *toolClient) call(ctx context.Context, name string, args map[string]any) (json.RawMessage, error) {
	params := &mcp.CallToolParams{Name: name, Arguments: args}
	params.SetProgressToken("prusactl")
	res, err := t.session.CallTool(ctx, params)
	if err != nil {
		return nil, err
	}
	var text strings.Builder
	for _, c := range res.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			text.WriteString(v.Text)
		case *mcp.ImageContent:
			return json.Marshal(map[string]any{"image_bytes": len(v.Data), "mime_type": v.MIMEType})
		}
	}
	if res.IsError {
		return nil, errors.New(strings.TrimSpace(text.String()))
	}
	return json.RawMessage(text.String()), nil
}

// image runs a tool that returns a picture and hands back its bytes.
func (t *toolClient) image(ctx context.Context, name string, args map[string]any) ([]byte, string, string, error) {
	res, err := t.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, "", "", err
	}
	var note strings.Builder
	for _, c := range res.Content {
		if v, ok := c.(*mcp.TextContent); ok {
			note.WriteString(v.Text)
		}
	}
	if res.IsError {
		return nil, "", "", errors.New(strings.TrimSpace(note.String()))
	}
	for _, c := range res.Content {
		if v, ok := c.(*mcp.ImageContent); ok {
			return v.Data, v.MIMEType, strings.TrimSpace(note.String()), nil
		}
	}
	return nil, "", "", errors.New("the tool returned no image")
}

// runTool is the shape of every printer command: build the arguments, then
// print the result.
func runTool(ctx context.Context, name string, args map[string]any, jsonOut bool, render func(json.RawMessage) error) error {
	tc, err := openTools(ctx)
	if err != nil {
		return err
	}
	defer tc.close()
	raw, err := tc.call(ctx, name, args)
	if err != nil {
		return err
	}
	if jsonOut || render == nil {
		return printJSON(raw)
	}
	return render(raw)
}

// asFlags spells a tool's arguments the way the terminal takes them, for the
// one place that shows an error to a person. It must not run any earlier:
// withPlateConfirm decides whether to ask about the plate by reading the
// tool's own words, and a rewritten message stopped matching, which quietly
// turned the question into a refusal.
// Each rule rewrites one sentence a tool produces. anchor is a stretch of that
// sentence carrying no interpolated value, so a test can check the tool still
// says it: these are prose couplings across a package boundary, and the only
// thing that keeps them honest is noticing when the prose moves.
type spelling struct{ anchor, from, to string }

var spellings = []spelling{
	{"; set overwrite to replace it",
		"set overwrite to replace it", "add --overwrite to replace it"},
	{"pass overwrite=true to replace it",
		"pass overwrite=true to replace it", "add --overwrite to replace it"},
	{"isn't possible here; leave via out",
		"isn't possible here; leave via out", "isn't possible here; leave --via out"},
	{"pass exactly one of path or hash",
		"pass exactly one of path or hash", "pass a path, or a hash with --hash"},
	{"then call again with plate_clear=true",
		"call again with plate_clear=true", "run it again with --plate-clear"},
	{"; pass printer as one of",
		"pass printer as one of", "pass --printer as one of"},
	{"; pass version", "pass version", "pass --version"},
	{"or delete it through Prusa Connect with",
		`delete it through Prusa Connect with via="connect"`, "delete it through Prusa Connect with --via connect"},
	// via=connect and via=direct reach the reader interpolated, so they are
	// rewritten on their own rather than as part of a sentence.
	{"", "via=connect", "--via connect"},
	{"", "via=direct", "--via direct"},
}

var argSpellings = func() *strings.Replacer {
	pairs := make([]string, 0, len(spellings)*2)
	for _, r := range spellings {
		pairs = append(pairs, r.from, r.to)
	}
	return strings.NewReplacer(pairs...)
}()

func asFlags(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if out := argSpellings.Replace(msg); out != msg {
		return errors.New(out)
	}
	return err
}

// mustJSON renders a result the CLI assembled itself, for the commands whose
// answer isn't a tool's JSON (a saved picture, say) but which still take --json.
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

func printJSON(raw json.RawMessage) error {
	var pretty any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if dec.Decode(&pretty) != nil {
		fmt.Println(string(raw))
		return nil
	}
	b, err := json.MarshalIndent(pretty, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// --- shared argument helpers ---------------------------------------------------

// printerArgs carries the flags every printer command accepts.
func printerArgs(o options) map[string]any {
	args := map[string]any{}
	if p := o.str("printer"); p != "" {
		args["printer"] = p
	}
	if v := o.str("via"); v != "" {
		args["via"] = v
	}
	return args
}

var printerFlags = []flagSpec{
	{Name: "printer", Value: "NAME", Usage: "printer name, serial number, or Connect UUID (with several printers)"},
	{Name: "via", Value: "ROUTE", Usage: "force a route: direct or connect"},
	{Name: "json", Usage: "print the raw JSON result"},
}

func pageArgs(o options, args map[string]any) (map[string]any, error) {
	for _, name := range []string{"limit", "offset"} {
		v, ok, err := intFlag(o, name)
		if err != nil {
			return nil, err
		}
		if ok {
			args[name] = v
		}
	}
	return args, nil
}

// intFlag reads a flag that takes a number. A value that isn't one is refused:
// silently falling back to the default would answer a different question than
// the one asked, and look like it worked.
func intFlag(o options, name string) (int, bool, error) {
	raw := o.str(name)
	if raw == "" {
		return 0, false, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false, fmt.Errorf("--%s takes a number (got %q)", name, raw)
	}
	return v, true, nil
}

// noArgs refuses the arguments a command doesn't take. Accepting and ignoring
// them hides a typo: `prusactl telemetry 60` looks like it asked for an hour.
func noArgs(name string, pos []string) error {
	if len(pos) == 0 {
		return nil
	}
	return fmt.Errorf("%s: takes no arguments, got %q (see `prusactl help %s`)", name, pos[0], name)
}

// notForAction refuses a flag that the chosen action ignores. A flag quietly
// dropped is the same trap as an argument quietly dropped: it looks like it
// worked and answers a different question.
func notForAction(o options, name, act string, flags ...string) error {
	for _, f := range flags {
		if o.given(f) {
			return fmt.Errorf("%s %s: --%s does not apply here (see `prusactl help %s`)", name, act, f, name)
		}
	}
	return nil
}

// atMost refuses more arguments than a command reads.
func atMost(name string, pos []string, n int, usage string) error {
	if len(pos) <= n {
		return nil
	}
	return fmt.Errorf("%s: unexpected argument %q; usage: %s", name, pos[n], usage)
}

var pageFlags = []flagSpec{
	{Name: "limit", Value: "N", Usage: "how many entries to return"},
	{Name: "offset", Value: "N", Usage: "entries to skip, for the next page"},
}

func flags(sets ...[]flagSpec) []flagSpec {
	var out []flagSpec
	for _, s := range sets {
		out = append(out, s...)
	}
	return out
}

// field reads a string from decoded JSON.
func field(m map[string]any, path ...string) string {
	cur := any(m)
	for _, k := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = obj[k]
	}
	if cur == nil {
		return ""
	}
	return fmt.Sprint(cur)
}

func decodeMap(raw json.RawMessage) map[string]any {
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if dec.Decode(&m) != nil {
		return nil
	}
	return m
}

// stamp turns one of Connect's epoch seconds into a local time. Anything that
// isn't a time is handed back unchanged, so a missing field prints as itself.
func stamp(v string) string {
	if v == "" {
		return ""
	}
	secs, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return v
	}
	return time.Unix(int64(secs), 0).Local().Format(time.DateTime)
}

// numbers pulls the readings out of one of Connect's sparse data series,
// dropping the empty buckets.
func numbers(v any) []float64 {
	list, _ := v.([]any)
	out := make([]float64, 0, len(list))
	for _, item := range list {
		n, ok := item.(json.Number)
		if !ok {
			continue
		}
		if f, err := n.Float64(); err == nil {
			out = append(out, f)
		}
	}
	return out
}

func rows(m map[string]any, key string) []map[string]any {
	list, _ := m[key].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, v := range list {
		if r, ok := v.(map[string]any); ok {
			out = append(out, r)
		}
	}
	return out
}

// confirm asks a yes/no question on the terminal, for the checks an agent
// answers with a flag.
func confirm(question string) (bool, error) {
	answer, err := ask(question + " [y/N]: ")
	if err != nil {
		return false, err
	}
	a := strings.ToLower(strings.TrimSpace(answer))
	return a == "y" || a == "yes", nil
}

// stdoutIsFile reports whether output is redirected, so binary (a snapshot)
// can be written there safely.
func stdoutIsFile() bool {
	st, err := os.Stdout.Stat()
	return err == nil && st.Mode().IsRegular()
}
