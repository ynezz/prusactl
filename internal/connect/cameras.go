package connect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// cameraSnapshotsQuery is the Connect web app's CamerasByPrinter query,
// trimmed to the fields used here.
const cameraSnapshotsQuery = `query CameraSnapshots($printerUuid: UUID!, $first: Int!) {
  camera {
    camerasConnection(printerUuid: $printerUuid, first: $first) {
      __typename
      ... on CameraConnection { edges { node { token snapshots { lastSnapshotUrl } } } }
      ... on CameraServiceError { errorCode message }
    }
  }
}`

// GraphQL runs a query against Connect's GraphQL API and decodes its data
// into out.
func (c *Client) GraphQL(ctx context.Context, query string, variables map[string]any, out any) error {
	var body struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	err := c.JSON(ctx, Request{Method: http.MethodPost, Path: c.GraphQLURL, JSON: map[string]any{"query": query, "variables": variables}}, &body)
	if err != nil {
		return err
	}
	if len(body.Errors) > 0 {
		msgs := make([]string, 0, len(body.Errors))
		for _, e := range body.Errors {
			msgs = append(msgs, e.Message)
		}
		return fmt.Errorf("Prusa Connect GraphQL: %s", strings.Join(msgs, "; "))
	}
	if len(body.Data) == 0 || string(body.Data) == "null" {
		return fmt.Errorf("Prusa Connect GraphQL returned no data")
	}
	return json.Unmarshal(body.Data, out)
}

// SnapshotURLs returns each of the printer's cameras' latest-snapshot URL on
// Prusa's camera service, keyed by camera token (the same token the
// /app/printers/{uuid}/cameras records carry). Fetch one with Do and an
// absolute Path.
func (c *Client) SnapshotURLs(ctx context.Context, printerUUID string) (map[string]string, error) {
	var data struct {
		Camera struct {
			Conn struct {
				Typename  string `json:"__typename"`
				ErrorCode string `json:"errorCode"`
				Message   string `json:"message"`
				Edges     []struct {
					Node struct {
						Token     string `json:"token"`
						Snapshots struct {
							LastSnapshotURL string `json:"lastSnapshotUrl"`
						} `json:"snapshots"`
					} `json:"node"`
				} `json:"edges"`
			} `json:"camerasConnection"`
		} `json:"camera"`
	}
	if err := c.GraphQL(ctx, cameraSnapshotsQuery, map[string]any{"printerUuid": printerUUID, "first": 20}, &data); err != nil {
		return nil, err
	}
	conn := data.Camera.Conn
	if conn.Typename != "CameraConnection" {
		return nil, fmt.Errorf("Prusa Connect camera service: %s", firstNonEmpty(conn.ErrorCode, conn.Message, "no cameras"))
	}
	urls := map[string]string{}
	for _, e := range conn.Edges {
		if e.Node.Token != "" && e.Node.Snapshots.LastSnapshotURL != "" {
			urls[e.Node.Token] = e.Node.Snapshots.LastSnapshotURL
		}
	}
	return urls, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
