// Tests for the rate-limit helpers in ratelimit.go.
//
// The burst formula is defined in ADR-0006: `max(30, rpm/20)`.
// This test pins the formula so a future refactor cannot
// silently change the burst size without updating the ADR.
package server

import "testing"

func TestBurstFromRate(t *testing.T) {
	cases := []struct {
		name string
		rpm  int
		want int
	}{
		// Spec default: 600 rpm yields burst 30.
		{"spec default 600", 600, 30},
		// Zero and negative fall back to the spec default.
		{"zero falls back to 600", 0, 30},
		{"negative falls back to 600", -1, 30},
		// Floor of 30: low-rpm configs still get a usable burst.
		{"60 rpm, burst 30 by floor", 60, 30},
		{"599 rpm, burst 30 by floor", 599, 30},
		// Scale region: rpm/20 wins when above the floor.
		{"1200 rpm, burst 60", 1200, 60},
		{"3000 rpm, burst 150", 3000, 150},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := BurstFromRate(c.rpm); got != c.want {
				t.Errorf("BurstFromRate(%d) = %d, want %d", c.rpm, got, c.want)
			}
		})
	}
}
