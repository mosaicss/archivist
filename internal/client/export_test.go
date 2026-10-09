package client

import "time"

// SetHTTPTimeoutForTest shortens the HTTP client timeout so a test can
// trigger a client timeout quickly.
func (c *Client) SetHTTPTimeoutForTest(d time.Duration) {
	c.httpClient.Timeout = d
}
