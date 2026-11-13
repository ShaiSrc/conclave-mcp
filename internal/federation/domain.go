package federation

import "github.com/hermes/conclave-mcp/internal/fedrooms"

// ErrInvalidDomain is returned by NormalizeDomain.
var ErrInvalidDomain = fedrooms.ErrInvalidDomain

// NormalizeDomain returns the canonical form of a federation domain;
// see fedrooms.NormalizeDomain, which stand-in keys share.
func NormalizeDomain(s string) (string, error) { return fedrooms.NormalizeDomain(s) }
