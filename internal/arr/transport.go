package arr

import (
	"net/http"
	"time"
)

// BaseURL identifies the server for state binding without exposing its API key.
func (c *Client) BaseURL() string { return c.config.URL }

// EnforceReadOnly permanently disables mutations for this client. Configure it
// before starting requests; serving and live tests use the same transport guard.
func (c *Client) EnforceReadOnly() { c.readOnly = true }

func (c *Client) IsReadOnly() bool { return c.readOnly }

// SetTransport injects HTTP I/O before requests start. It preserves the client's
// normal deadline, redirect refusal, and independent read-only enforcement.
func (c *Client) SetTransport(transport http.RoundTripper) { c.client.Transport = transport }

func (c *Client) RequestTimeout() time.Duration { return c.client.Timeout }
