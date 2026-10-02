package main

type Assertion struct {
	Source string `json:"source"`
	Op     string `json:"op"`
	Value  string `json:"value,omitempty"`
	Path   string `json:"path,omitempty"`
	Name   string `json:"name,omitempty"`
}

type Auth struct {
	Type     string `json:"type"`
	Token    string `json:"token,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

type Request struct {
	Headers     map[string]string `json:"headers,omitempty"`
	Auth        *Auth             `json:"auth,omitempty"`
	Body        string            `json:"body,omitempty"`
	ContentType string            `json:"contentType,omitempty"`
	// Set by the worker for first-party agents only: which of Headers are secret, and whether Body
	// is. Used to withhold them when a redirect leaves the original host or drops to http (#312).
	SecretHeaderNames []string `json:"secretHeaderNames,omitempty"`
	BodySecret        bool     `json:"bodySecret,omitempty"`
}

// Transport mirrors src/types.ts TransportSpec: connectionless probe params (udp today).
// Payload is decoded per PayloadEncoding (default "utf8"); Expect is a substring the reply must
// contain, matched in the same encoding.
type Transport struct {
	Payload         string `json:"payload,omitempty"`
	PayloadEncoding string `json:"payloadEncoding,omitempty"`
	Expect          string `json:"expect,omitempty"`
	ExpectMode      string `json:"expectMode,omitempty"` // "" / "contains" (default) or "regex"
}

// Ping mirrors src/types.ts CheckSpec.ping: multi-packet ICMP config. All optional.
type Ping struct {
	Count           int `json:"count,omitempty"`           // echoes per check (default 4, clamp 1-10)
	LossDegradedPct int `json:"lossDegradedPct,omitempty"` // degrade when lossPct exceeds this (1-100)
	RttDegradedMs   int `json:"rttDegradedMs,omitempty"`   // degrade when avg RTT exceeds this (ms)
}

type CheckSpec struct {
	Request    *Request    `json:"request,omitempty"`
	Assertions []Assertion `json:"assertions,omitempty"`
	Transport  *Transport  `json:"transport,omitempty"`
	Ping       *Ping       `json:"ping,omitempty"`
}

type WorkItem struct {
	MonitorID       string    `json:"monitorId"`
	SourceKey       string    `json:"sourceKey"`
	Type            string    `json:"type"`
	Target          string    `json:"target"`
	IntervalSeconds int       `json:"intervalSeconds"`
	TimeoutMs       int       `json:"timeoutMs"`
	CheckSpec       CheckSpec `json:"checkSpec"`
	Method          string    `json:"method"`
	ExpectedStatus  string    `json:"expectedStatus"`
	FollowRedirects bool      `json:"followRedirects"`
	RequestID       string    `json:"requestId,omitempty"`
}

// DiscoveryReport is what this agent is configured to scan, sent on every pull so the console can
// show it. The server can't know any of this otherwise — it lives entirely in the agent's env.
type DiscoveryReport struct {
	CIDR       string `json:"cidr,omitempty"`
	Ports      string `json:"ports,omitempty"`
	AWSRegions string `json:"awsRegions,omitempty"`
}

type PullRequest struct {
	Capabilities []string         `json:"capabilities"`
	Version      string           `json:"version"`
	Arch         string           `json:"arch"`
	Discovery    *DiscoveryReport `json:"discovery,omitempty"`
}

type PullResponse struct {
	AgentID      string     `json:"agentId"`
	AgentKind    string     `json:"agentKind,omitempty"`
	Capabilities []string   `json:"capabilities"`
	Work         []WorkItem `json:"work"`
	Diagnose     []WorkItem `json:"diagnose,omitempty"`
}

type IngestResult struct {
	MonitorID  string         `json:"monitorId"`
	OK         bool           `json:"ok"`
	Cause      *string        `json:"cause"`
	LatencyMs  int64          `json:"latencyMs"`
	HTTPStatus int            `json:"httpStatus"`
	Degraded   bool           `json:"degraded"`
	TS         int64          `json:"ts"`
	Detail     map[string]any `json:"detail,omitempty"`
	RequestID  string         `json:"requestId,omitempty"`
}

type IngestBody struct {
	Results []IngestResult `json:"results"`
}

type DiscoveredItem struct {
	Host         string `json:"host"`
	Port         int    `json:"port"`
	ServiceGuess string `json:"serviceGuess"`
}

// CloudResource is one discovered cloud resource pushed to /agent/v1/cloud-resources.
type CloudResource struct {
	Provider     string            `json:"provider"`
	ResourceType string            `json:"resourceType"`
	ResourceID   string            `json:"resourceId"`
	Address      string            `json:"address"`
	Ports        []int             `json:"ports"`
	Tags         map[string]string `json:"tags"`
	Meta         map[string]string `json:"meta"`
}
