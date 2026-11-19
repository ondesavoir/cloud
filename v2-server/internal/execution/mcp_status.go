package execution

// SessionMCPStatus contains only durable identities and a fresh, optional Node observation.
type SessionMCPStatus struct {
	Generation  *int64            `json:"generation,omitempty"`
	Convergence string            `json:"convergence"`
	Active      *int64            `json:"activeGeneration,omitempty"`
	Capacity    *bool             `json:"probeCapacityAvailable,omitempty"`
	CanProbe    bool              `json:"canProbe"`
	Members     []MCPMemberStatus `json:"members"`
}

type MCPMemberStatus struct {
	ID         string `json:"id"`
	Configured bool   `json:"configured"`
	Code       string `json:"code,omitempty"`
	Version    string `json:"version,omitempty"`
	Revision   int64  `json:"revision,omitempty"`
	Health     string `json:"health"`
	Queued     bool   `json:"queued,omitempty"`
	InFlight   bool   `json:"inFlight,omitempty"`
	Cleanup    bool   `json:"cleanupPending,omitempty"`
}
