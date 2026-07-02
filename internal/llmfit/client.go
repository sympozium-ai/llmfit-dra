package llmfit

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Client queries a long-lived `llmfit serve` over its versioned REST API
// (GET /api/v1/system) instead of forking the binary each probe cycle.
// URL forms:
//
//	unix:///run/llmfit/llmfit.sock   — Unix domain socket (sidecar; the
//	                                   only mode the chart ships, since the
//	                                   pod is hostNetwork and a TCP bind
//	                                   would land on the node's loopback)
//	http://127.0.0.1:8787            — plain TCP, for dev/testing
type Client struct {
	http *http.Client
	base string
}

// NewClient builds a client for the given llmfit serve URL.
func NewClient(rawURL string) (*Client, error) {
	if path, ok := strings.CutPrefix(rawURL, "unix://"); ok {
		if path == "" {
			return nil, fmt.Errorf("empty unix socket path in %q", rawURL)
		}
		return &Client{
			base: "http://llmfit", // dummy authority; the dialer ignores it
			http: &http.Client{
				Transport: &http.Transport{
					DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
						var d net.Dialer
						return d.DialContext(ctx, "unix", path)
					},
				},
			},
		}, nil
	}
	if strings.HasPrefix(rawURL, "http://") || strings.HasPrefix(rawURL, "https://") {
		return &Client{base: strings.TrimRight(rawURL, "/"), http: &http.Client{}}, nil
	}
	return nil, fmt.Errorf("unsupported llmfit URL %q (want unix:// or http://)", rawURL)
}

// Detect fetches and parses /api/v1/system. The response envelope is a
// superset of the CLI's `--json system` output, so Parse is shared.
func (c *Client) Detect(ctx context.Context) (*System, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1/system", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llmfit API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("llmfit API: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("llmfit API read: %w", err)
	}
	return Parse(data)
}
