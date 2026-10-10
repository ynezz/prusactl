// Package connect is a client for the Prusa Connect web API
// (https://connect.prusa3d.com/app/...), the same API the Connect web app uses.
// Prusa does not publish it; paths and payloads here were read from the web
// app and checked against live responses.
package connect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/trevin-lee/prusactl/internal/compat"
	"github.com/trevin-lee/prusactl/internal/redact"
)

// DefaultBaseURL is the production Connect origin.
const DefaultBaseURL = "https://connect.prusa3d.com"

// DefaultGraphQLURL is Connect's GraphQL API (GRAPHQL_API_URL in
// https://connect.prusa3d.com/environment.js), where the web app has read
// camera snapshots since Prusa moved cameras to its camera service in 2026-09.
const DefaultGraphQLURL = "https://connect-api.prusa3d.com/graphql"

// TokenSource supplies bearer tokens.
type TokenSource interface {
	AccessToken(ctx context.Context) (string, error)
	Invalidate(access string)
}

// Client calls the Connect API.
type Client struct {
	BaseURL    string
	GraphQLURL string
	HTTP       *http.Client
	Tokens     TokenSource
	UserAgent  string
}

// New builds a client; PRUSA_CONNECT_URL overrides the origin and
// PRUSA_CONNECT_GRAPHQL_URL the GraphQL endpoint.
func New(tokens TokenSource, userAgent string) *Client {
	base := DefaultBaseURL
	if v := os.Getenv("PRUSA_CONNECT_URL"); v != "" {
		base = strings.TrimRight(v, "/")
	}
	graphql := DefaultGraphQLURL
	if v := os.Getenv("PRUSA_CONNECT_GRAPHQL_URL"); v != "" {
		graphql = v
	}
	return &Client{
		BaseURL:    base,
		GraphQLURL: graphql,
		HTTP:       &http.Client{Timeout: 60 * time.Second},
		Tokens:     tokens,
		UserAgent:  userAgent,
	}
}

// APIError is a non-2xx response.
type APIError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	body := redact.Text(strings.TrimSpace(e.Body))
	if len(body) > 2000 {
		body = body[:2000] + "…"
	}
	msg := fmt.Sprintf("Prusa Connect: %s %s -> %d %s", e.Method, e.Path, e.Status, http.StatusText(e.Status))
	if body != "" {
		msg += ": " + body
	}
	return msg
}

// apiChanged says why an error response means the API itself moved, or
// returns "". Connect answers an unknown route with 404 NOT_FOUND_ENDPOINT, and
// a missing record with a specific code (NOT_FOUND_PRINTER, ...), which is a
// normal error; a 404 without any code didn't come from Connect's API at all.
func apiChanged(status int, body []byte) string {
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &e)
	switch {
	case status == http.StatusNotFound && e.Code == "NOT_FOUND_ENDPOINT":
		return "is no longer an endpoint"
	case status == http.StatusNotFound && e.Code == "":
		return "returned 404 without Prusa Connect's usual error code"
	case status == http.StatusMethodNotAllowed:
		return "is no longer allowed"
	case status == http.StatusGone:
		return "was removed (410 Gone)"
	}
	return ""
}

// IsStatus reports whether err is an APIError with the given status.
func IsStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status
}

// Request describes one call. Body, when set, is re-created for the retry
// that follows an expired-token 401.
type Request struct {
	Method        string
	Path          string // "/app/..."
	Query         url.Values
	JSON          any                           // marshalled as the body
	Body          func() (io.ReadCloser, error) // raw body (uploads)
	ContentLength int64                         // for Body; avoids chunked uploads
	ContentType   string
	Header        http.Header
	Timeout       time.Duration // overrides the client timeout (uploads)
}

// Do performs req and returns the response with its body unread; the caller
// must close it. Non-2xx responses are returned as *APIError.
func (c *Client) Do(ctx context.Context, req Request) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		token, err := c.Tokens.AccessToken(ctx)
		if err != nil {
			return nil, err
		}
		resp, err := c.send(ctx, req, token)
		if err != nil {
			return nil, err
		}
		// An expired or revoked access token: refresh once and retry. A 401
		// means the request was not processed, so retrying a mutation is safe.
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			resp.Body.Close()
			c.Tokens.Invalidate(token)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			defer resp.Body.Close()
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			apiErr := &APIError{Method: req.Method, Path: req.Path, Status: resp.StatusCode, Body: string(b)}
			if detail := apiChanged(resp.StatusCode, b); detail != "" {
				return nil, &compat.Error{Service: compat.Connect, Detail: fmt.Sprintf("%s %s %s", req.Method, req.Path, detail), Err: apiErr}
			}
			return nil, apiErr
		}
		return resp, nil
	}
}

func (c *Client) send(ctx context.Context, req Request, token string) (*http.Response, error) {
	u := c.BaseURL + req.Path
	if strings.HasPrefix(req.Path, "https://") || strings.HasPrefix(req.Path, "http://") {
		// An absolute URL, e.g. a snapshot on Prusa's camera service. The
		// session's token only ever goes to Prusa.
		if !c.trusted(req.Path) {
			return nil, fmt.Errorf("refusing to send the Prusa Connect session to %s", hostOf(req.Path))
		}
		u = req.Path
	}
	if len(req.Query) > 0 {
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		u += sep + req.Query.Encode()
	}
	var body io.Reader
	contentType := req.ContentType
	switch {
	case req.Body != nil:
		rc, err := req.Body()
		if err != nil {
			return nil, err
		}
		body = rc
	case req.JSON != nil:
		b, err := json.Marshal(req.JSON)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
		contentType = "application/json"
	}
	hr, err := http.NewRequestWithContext(ctx, req.Method, u, body)
	if err != nil {
		return nil, err
	}
	if req.ContentLength > 0 {
		hr.ContentLength = req.ContentLength
	}
	for k, vs := range req.Header {
		for _, v := range vs {
			hr.Header.Add(k, v)
		}
	}
	if contentType != "" {
		hr.Header.Set("Content-Type", contentType)
	}
	hr.Header.Set("Authorization", "Bearer "+token)
	hr.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		hr.Header.Set("User-Agent", c.UserAgent)
	}
	httpClient := c.HTTP
	if req.Timeout > 0 {
		clone := *c.HTTP
		clone.Timeout = req.Timeout
		httpClient = &clone
	}
	resp, err := httpClient.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("Prusa Connect: %s %s: %w", req.Method, req.Path, err)
	}
	return resp, nil
}

// trusted reports whether an absolute URL may receive the session: HTTPS on
// prusa3d.com or a subdomain, or exactly the configured Connect or GraphQL
// origin (scheme, host and port).
func (c *Client) trusted(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	for _, own := range []string{c.BaseURL, c.GraphQLURL} {
		if o, err := url.Parse(own); err == nil && o.Host != "" && strings.EqualFold(o.Scheme, u.Scheme) && strings.EqualFold(o.Host, u.Host) {
			return true
		}
	}
	host := strings.ToLower(u.Hostname())
	return u.Scheme == "https" && (host == "prusa3d.com" || strings.HasSuffix(host, ".prusa3d.com"))
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return "another site"
}

// JSON performs req and decodes a JSON response into out (which may be nil,
// or a *json.RawMessage to keep the payload verbatim).
func (c *Client) JSON(ctx context.Context, req Request, out any) error {
	resp, err := c.Do(ctx, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if len(bytes.TrimSpace(b)) == 0 {
		if raw, ok := out.(*json.RawMessage); ok {
			*raw = json.RawMessage("null")
		}
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		// Not JSON, or JSON of a different shape (a list where an object was,
		// a string where a number was): the response format changed.
		return &compat.Error{Service: compat.Connect, Detail: fmt.Sprintf("%s %s returned data in a different format (%v)", req.Method, req.Path, err), Err: err}
	}
	return nil
}

// Missing reports a response that decoded but lacks a field prusactl needs,
// such as the "printers" list, which would otherwise read as empty.
func Missing(method, path, field string) error {
	return compat.New(compat.Connect, "%s %s no longer includes %q", method, path, field)
}

// Get is shorthand for a GET returning JSON.
func (c *Client) Get(ctx context.Context, path string, query url.Values, out any) error {
	return c.JSON(ctx, Request{Method: http.MethodGet, Path: path, Query: query}, out)
}

// PathEscape escapes one path segment.
func PathEscape(s string) string { return url.PathEscape(s) }
