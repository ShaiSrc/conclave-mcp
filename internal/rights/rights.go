// Package rights is what a key may do in a room: the set of rights on
// its membership (room_members.rights, migration 0008).
package rights

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Rights a membership can hold besides the message types. Each message
// type is the right to send messages of that type; a message without a
// type is stored as System.
const (
	Read   = "read"
	Claim  = "claim"
	System = "system"
)

// Order lists every right in storage order.
var Order = []string{Read, Claim, "question", "answer", "decision", "handoff",
	"status_update", "task_assignment", "alert", System}

// Set is a membership's rights in Order. Parse and FromStorage never
// return nil, so an empty Set encodes as [] in JSON.
type Set []string

// All is every right.
func All() Set { return slices.Clone(Set(Order)) }

// Parse checks rights from the API or the CLI: names from Order plus
// the shorthands "all" and "none". "all" is expanded here, so a right
// added later is not granted to existing members.
func Parse(in []string) (Set, error) {
	want := map[string]bool{}
	for _, r := range in {
		switch r = strings.TrimSpace(r); {
		case r == "all":
			for _, o := range Order {
				want[o] = true
			}
		case r == "none" || r == "":
		case slices.Contains(Order, r):
			want[r] = true
		default:
			return nil, fmt.Errorf("unknown right %q (known: all, none, %s)", r, strings.Join(Order, ", "))
		}
	}
	out := Set{}
	for _, o := range Order {
		if want[o] {
			out = append(out, o)
		}
	}
	return out, nil
}

// FromRole translates the role an old client sends instead of rights.
func FromRole(role string) (Set, error) {
	switch role {
	case "owner", "writer":
		return All(), nil
	case "reader":
		return Set{Read}, nil
	}
	return nil, errors.New("role must be owner|writer|reader")
}

// FromStorage reads the room_members.rights column.
func FromStorage(s string) Set {
	if s == "" {
		return Set{}
	}
	return Set(strings.Split(s, ","))
}

// Storage is the room_members.rights value: comma-separated, in Order.
func (s Set) Storage() string { return strings.Join(s, ",") }

// Has reports whether s holds right.
func (s Set) Has(right string) bool { return slices.Contains(s, right) }
