// The control messages of the protocol: everything that is not a request
// or a result. See protocol.md sections 1, 2 and 4.
package protocol

// AgentInfo describes the running agent inside hello.
type AgentInfo struct {
	Version  string `json:"version"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Hostname string `json:"hostname"`
}

// Hello is the first frame the agent sends. It is the only frame that
// carries the token.
type Hello struct {
	Type         string    `json:"type"`
	Protocol     int       `json:"protocol"`
	Token        string    `json:"token"`
	Engine       string    `json:"engine"`
	Database     string    `json:"database"`
	Agent        AgentInfo `json:"agent"`
	Capabilities []string  `json:"capabilities"`
}

// Welcome is the gateway's acceptance of hello.
type Welcome struct {
	Type           string `json:"type"`
	ConnectionID   string `json:"connection_id"`
	Name           string `json:"name"`
	PingIntervalMs int    `json:"ping_interval_ms"`
	MaxRows        int    `json:"max_rows"`
}

// Reject is the gateway's refusal; the socket closes right after.
type Reject struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

// Ping is sent by either side; T is echoed back in the Pong.
type Ping struct {
	Type string `json:"type"`
	T    int64  `json:"t"`
}

// Pong answers a Ping with the same T.
type Pong struct {
	Type string `json:"type"`
	T    int64  `json:"t"`
}

// Bye is the agent's notice of a clean shutdown.
type Bye struct {
	Type   string `json:"type"`
	Reason string `json:"reason,omitempty"`
}

// ErrorMsg is informational and not tied to a query.
type ErrorMsg struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}
