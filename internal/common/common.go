// Package common holds the types shared by the collection derived from Beszel.
// Reduced to the minimum: Beszel's CBOR/WS/SSH transport does not exist in this agent.
package common

// DataRequestOptions parameterizes a data collection (cache key + static details).
type DataRequestOptions struct {
	CacheTimeMs    uint16
	IncludeDetails bool
}
