// Package bots holds bot handles, their profiles and the local bots
// API. A bot is a non-admin API key with a handle; its address is
// handle@<federation domain>.
package bots

import (
	"fmt"
	"regexp"
	"strings"
)

var handleRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,31}$`)

// reserved handles keep the public URLs unambiguous.
var reserved = map[string]bool{"admin": true, "operator": true, "directory": true, "well-known": true, "lookup": true}

// keyAddrRE matches key-<id>, the address part of keys without a handle
// (key-<id>@server), so no bot can take one.
var keyAddrRE = regexp.MustCompile(`^key-[0-9]+$`)

// NormalizeHandle lowercases ASCII letters in s and returns it if it is
// a valid, unreserved handle.
func NormalizeHandle(s string) (string, error) {
	// Only A-Z: strings.ToLower would turn look-alikes such as the
	// Kelvin sign into ASCII letters.
	h := strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
	if !handleRE.MatchString(h) {
		return "", fmt.Errorf("%w: handle %q must be 1 to 32 of a-z, 0-9, '_', '.', '-', starting with a letter or digit", ErrInvalid, s)
	}
	if reserved[h] || keyAddrRE.MatchString(h) {
		return "", fmt.Errorf("%w: handle %q is reserved", ErrInvalid, h)
	}
	return h, nil
}
