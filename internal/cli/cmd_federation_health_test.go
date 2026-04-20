package cli

import "testing"

func TestVersionSeriesMatches(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"same series", "v0.4.1-19-g1cc3be6", "v0.4.1-20-gabcd123", true},
		{"different minor", "v0.4.1", "v0.5.0", false},
		{"different major", "v1.0.0", "v0.9.0", false},
		{"without v prefix", "0.4.1", "v0.4.2", true},
		{"dev build on one side — don't warn", "dev", "v0.4.1", true},
		{"commit hash on one side — don't warn", "1cc3be6-dirty", "v0.4.1", true},
		{"both dev — don't warn", "dev", "dev", true},
		{"both empty — don't warn", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := versionSeriesMatches(tc.a, tc.b); got != tc.want {
				t.Errorf("versionSeriesMatches(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestSemverSeries(t *testing.T) {
	cases := []struct {
		in     string
		prefix string
		ok     bool
	}{
		{"v0.4.1", "0.4", true},
		{"0.5.0-rc1", "0.5", true},
		{"v1.2", "1.2", true},
		{"v0.4.1-19-g1cc3be6", "0.4", true},
		{"v0.4.1-dirty", "0.4", true},
		{"dev", "", false},
		{"", "", false},
		{"abc", "", false},
		{"v.1", "", false},
	}
	for _, tc := range cases {
		got, ok := semverSeries(tc.in)
		if got != tc.prefix || ok != tc.ok {
			t.Errorf("semverSeries(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.prefix, tc.ok)
		}
	}
}
