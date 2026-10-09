package client

import (
	"net/http"
	"time"
)

// SetHTTPTimeoutForTest shortens the HTTP client timeout so a test can
// trigger a client timeout quickly.
func (c *Client) SetHTTPTimeoutForTest(d time.Duration) {
	c.httpClient.Timeout = d
}

// SetTransportForTest replaces the HTTP transport, e.g. with one whose dial
// fails.
func (c *Client) SetTransportForTest(rt http.RoundTripper) {
	c.httpClient.Transport = rt
}
