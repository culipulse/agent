package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"time"
)

// maxRespBytes bounds how much of any server response the agent buffers, so a
// misbehaving/compromised control plane can't OOM the agent with a huge body.
// (Probe-target bodies are separately capped at 1 MiB in prober.go.) var, not
// const, so tests can shrink it.
var maxRespBytes int64 = 8 << 20 // 8 MiB

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	// Discovery is this agent's scan configuration, set once at startup from the environment and
	// sent on every pull. main() always assigns a non-nil (possibly zero-value) report, so the
	// "discovery" key is always present — the server distinguishes "switched off" (present, empty)
	// from "too old to report" (key absent) by that presence alone.
	Discovery *DiscoveryReport
}

func NewClient(baseURL, token string) *Client {
	return &Client{BaseURL: baseURL, Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) do(ctx context.Context, path string, body any, out any) error {
	var buf io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+path, buf)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("User-Agent", agentUA)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if resp.StatusCode/100 != 2 {
		n := len(data)
		if n > 200 {
			n = 200
		}
		return fmt.Errorf("%s -> %d: %s", path, resp.StatusCode, string(data[:n]))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *Client) Pull(ctx context.Context) (*PullResponse, error) {
	var pr PullResponse
	body := PullRequest{Capabilities: AgentCaps, Version: Version, Arch: runtime.GOARCH, Discovery: c.Discovery}
	if err := c.do(ctx, "/agent/v1/pull", body, &pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

func (c *Client) Ingest(ctx context.Context, results []IngestResult) error {
	return c.do(ctx, "/agent/v1/ingest", IngestBody{Results: results}, nil)
}

func (c *Client) Discovered(ctx context.Context, items []DiscoveredItem) error {
	return c.do(ctx, "/agent/v1/discovered", map[string]any{"discovered": items}, nil)
}

func (c *Client) CloudResources(ctx context.Context, provider string, resources []CloudResource, errMsg string) error {
	body := map[string]any{"provider": provider, "resources": resources}
	if errMsg != "" {
		body["error"] = errMsg
	}
	return c.do(ctx, "/agent/v1/cloud-resources", body, nil)
}
