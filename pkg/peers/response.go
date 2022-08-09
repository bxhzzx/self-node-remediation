package peers

type Response struct {
	IsHealthy bool
	Reason    reason
}

type reason string

const (
	HealthyBecauseCRNotFound                reason = "CR Not found, node is considered healthy"
	HealthyBecauseErrorsThresholdNotReached reason = "Errors number hasn't reached threshold not querying peers yet, node is considered healthy"
	HealthyBecauseNoPeersWereFound          reason = "No Peers where found, node is considered healthy"
	HealthyBecauseMostPeersCantAccessAPIServer reason = "Most peers couldn't access API server, node is considered healthy"

	UnHealthyBecauseCRFound        reason = "CR found, node is considered unhealthy"
	UnHealthyBecauseNodeIsIsolated reason = "Node is isolated, node is considered unhealthy"
)
