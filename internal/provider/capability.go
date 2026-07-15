// Package provider defines provider-neutral model capabilities.
package provider

import "fmt"

type HostedCapabilityKind string

// HostedCapabilityWebSearch identifies provider-hosted web search.
const HostedCapabilityWebSearch HostedCapabilityKind = "web_search"

// HostedCapability describes an optional provider-hosted tool and per-response cap.
type HostedCapability struct {
	Kind     HostedCapabilityKind `json:"kind"`
	MaxCalls int                  `json:"max_calls,omitempty"`
}

// CapabilityFailure contains sanitized capability-negotiation failure data.
type CapabilityFailure struct {
	Capability HostedCapabilityKind `json:"capability"`
	Reason     string               `json:"reason"`
}

// UnsupportedCapabilityError reports a provider rejection safe for runner recovery.
type UnsupportedCapabilityError struct {
	Failure CapabilityFailure
}

func (e *UnsupportedCapabilityError) Error() string {
	return fmt.Sprintf("provider does not support %s: %s", e.Failure.Capability, e.Failure.Reason)
}
