package rights

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"none"}, ""},
		{[]string{"handoff", "read", "handoff"}, "read,handoff"},
		{[]string{"all"}, "read,claim,question,answer,decision,handoff,status_update,task_assignment,alert,system"},
		{[]string{" read ", "none", ""}, "read"},
	} {
		got, err := Parse(c.in)
		if err != nil || got.Storage() != c.want {
			t.Errorf("Parse(%q) = %q, %v; want %q", c.in, got.Storage(), err, c.want)
		}
	}
	if _, err := Parse([]string{"read", "admin"}); err == nil || !strings.Contains(err.Error(), `"admin"`) {
		t.Errorf("unknown right: err = %v, want one naming \"admin\"", err)
	}
	if got, _ := Parse(nil); got == nil {
		t.Error("Parse(nil) must return an empty, non-nil Set (it encodes as [])")
	}
}

func TestFromRoleAndStorage(t *testing.T) {
	for role, want := range map[string]string{"owner": All().Storage(), "writer": All().Storage(), "reader": "read"} {
		if got, err := FromRole(role); err != nil || got.Storage() != want {
			t.Errorf("FromRole(%q) = %q, %v; want %q", role, got.Storage(), err, want)
		}
	}
	if _, err := FromRole("admin"); err == nil {
		t.Error("FromRole(admin): want an error")
	}
	s := FromStorage("read,handoff")
	if !s.Has(Read) || !s.Has("handoff") || s.Has(Claim) {
		t.Errorf("FromStorage(read,handoff) = %v", s)
	}
	if e := FromStorage(""); e == nil || len(e) != 0 {
		t.Errorf("FromStorage(\"\") = %#v, want an empty non-nil Set", e)
	}
}

func TestSplit(t *testing.T) {
	if got := Split(" read, handoff ,"); len(got) != 2 || got[0] != "read" || got[1] != "handoff" {
		t.Errorf("Split = %q", got)
	}
	if got := Split(""); got == nil || len(got) != 0 {
		t.Errorf("Split(\"\") = %#v, want an empty non-nil slice (sent as [])", got)
	}
}
