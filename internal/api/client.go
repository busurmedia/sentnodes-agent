package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/busurmedia/sentnodes-agent/internal/wire"
)

const userAgent = "SENTNODES-AGENT"

// Client talks to the SentNodes API over IPv4-only HTTPS.
type Client struct {
	base string
	http *http.Client
}

func New(base string) *Client {
	transport := &http.Transport{
		// Force IPv4 so the server-observed source IP matches the node's IPv4.
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp4", addr)
		},
		ForceAttemptHTTP2: true,
	}
	return &Client{base: base, http: &http.Client{Transport: transport, Timeout: 30 * time.Second}}
}

func (c *Client) Challenge(nodeAddr, apiKey string) (*wire.ChallengeData, error) {
	var out wire.ChallengeData
	err := c.do("GET", "/v1/agent/challenge?node_addr="+url.QueryEscape(nodeAddr), apiKey, nil, &out)
	return &out, err
}

func (c *Client) Register(apiKey string, req wire.RegisterReq) (*wire.RegisterData, error) {
	var out wire.RegisterData
	err := c.do("POST", "/v1/agent/register", apiKey, req, &out)
	return &out, err
}

func (c *Client) Poll(token string) ([]wire.Command, string, error) {
	var out wire.PollData
	if err := c.do("GET", "/v1/agent/commands", token, nil, &out); err != nil {
		return nil, "", err
	}
	return out.Commands, out.LatestVersion, nil
}

func (c *Client) Result(token, id string, ok bool, result map[string]interface{}) error {
	return c.do("POST", "/v1/agent/commands/"+url.PathEscape(id)+"/result", token, wire.ResultReq{OK: ok, Result: result}, nil)
}

func (c *Client) Metrics(token string, m wire.Metrics) error {
	return c.do("POST", "/v1/agent/metrics", token, wire.MetricsReq{Metrics: m}, nil)
}

func (c *Client) Heartbeat(token string, hb wire.Heartbeat) error {
	return c.do("POST", "/v1/agent/heartbeat", token, hb, nil)
}

// APIError carries the server's error envelope so callers can branch on Code
// (e.g. NODE_UNKNOWN when the node is not indexed yet).
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("http %d: %s", e.Status, e.Message) }

func (c *Client) do(method, path, bearer string, body, out interface{}) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env wire.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("http %d: %s", resp.StatusCode, string(raw))
	}
	if !env.Success {
		ae := &APIError{Status: resp.StatusCode, Message: "request failed"}
		if env.Errors != nil {
			if env.Errors.Message != "" {
				ae.Message = env.Errors.Message
			}
			ae.Code = fmt.Sprint(env.Errors.Code)
		}
		return ae
	}
	if out != nil && len(env.Data) > 0 {
		return json.Unmarshal(env.Data, out)
	}
	return nil
}
