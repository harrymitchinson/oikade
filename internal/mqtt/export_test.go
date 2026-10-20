package mqtt

// TerminateForTest drops the connection so tests can exercise reconnects.
func TerminateForTest(c *Client) { c.cm.TerminateConnectionForTest() }
