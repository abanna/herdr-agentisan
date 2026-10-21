package trail

import "net/http"

// TransportOf is the client's transport, for tests that check where a
// request can go.
func TransportOf(c *Client) http.RoundTripper { return c.http.Transport }
