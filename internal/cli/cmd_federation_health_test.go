package cli

import "testing"

func TestVersionSeriesMatches(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		// --- same baseline, no warning ---
		{"exact tagged match", "v0.9.2", "v0.9.2", true},
		{"dev commits on same baseline", "v0.9.2-10-g2473442", "v0.9.2-5-gabcd123", true},
		{"tag vs dev build on same baseline", "v0.9.2", "v0.9.2-10-g2473442", true},
		{"without v prefix on one side", "0.9.2", "v0.9.2", true},

		// --- different baseline, warn ---
		{"patch drift (the bug this test file is pinning)", "v0.9.1", "v0.9.2", false},
		{"patch drift with dev suffix on local", "v0.9.1", "v0.9.2-10-g2473442", false},
		{"minor drift", "v0.9.0", "v0.10.0", false},
		{"major drift", "v0.9.0", "v1.0.0", false},

		// --- unparseable on either side, skip warning ---
		{"dev on one side — no warning", "dev", "v0.9.1", true},
		{"commit hash on one side — no warning", "1cc3be6-dirty", "v0.9.1", true},
		{"both dev — no warning", "dev", "dev", true},
		{"both empty — no warning", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := versionSeriesMatches(tc.a, tc.b); got != tc.want {
				t.Errorf("versionSeriesMatches(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestSemverBaseline(t *testing.T) {
	cases := []struct {
		in       string
		baseline string
		ok       bool
	}{
		// --- clean tagged versions ---
		{"v0.9.2", "0.9.2", true},
		{"v0.9.1", "0.9.1", true},
		{"0.9.0", "0.9.0", true},
		{"v1.2", "1.2", true}, // allows major.minor only

		// --- dev builds: strip the -N-gSHA suffix ---
		{"v0.9.2-10-g2473442", "0.9.2", true},
		{"v0.9.2-10-g2473442-dirty", "0.9.2", true},
		{"v0.9.2-dirty", "0.9.2", true},

		// --- unparseable ---
		{"dev", "", false},
		{"", "", false},
		{"abc", "", false},
		{"v", "", false},
		{"v.1", "", false},
		{"1", "", false},
	}
	for _, tc := range cases {
		got, ok := semverBaseline(tc.in)
		if got != tc.baseline || ok != tc.ok {
			t.Errorf("semverBaseline(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.baseline, tc.ok)
		}
	}
}
