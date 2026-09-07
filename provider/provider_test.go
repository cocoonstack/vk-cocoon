package provider

import "testing"

func TestParseOrphanPolicy(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want OrphanPolicy
	}{{"destroy", OrphanDestroy}, {"Alert", OrphanAlert}, {" keep ", OrphanKeep}} {
		got, err := ParseOrphanPolicy(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseOrphanPolicy(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{"", "remove"} {
		if got, err := ParseOrphanPolicy(in); err == nil {
			t.Errorf("ParseOrphanPolicy(%q) = %q, want error", in, got)
		}
	}
}
