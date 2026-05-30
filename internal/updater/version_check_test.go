package updater

import "testing"

// TestIsNewerExhaustive nails down the comparison algorithm against the real
// tag sequence and tricky cases (double-digit, missing parts, prefixes).
func TestIsNewerExhaustive(t *testing.T) {
	cases := []struct {
		cur, latest string
		want        bool
	}{
		// real release sequence
		{"v0.1.0", "v0.4.1", true},
		{"v0.4.0", "v0.4.1", true},
		{"v0.4.1", "v0.4.1", false},
		{"v0.4.1", "v0.4.0", false},
		// double-digit (string compare would be WRONG here)
		{"v0.9.0", "v0.10.0", true},
		{"v0.10.0", "v0.9.0", false},
		{"v1.0.0", "v0.99.99", false},
		{"v0.99.99", "v1.0.0", true},
		// prefix / spacing tolerance
		{"0.4.0", "v0.4.1", true},
		{" v0.4.0 ", "v0.4.1", true},
		{"V0.4.0", "v0.4.1", true},
		// short tags
		{"v1.0", "v1.0.1", true},
		{"v1", "v1.0.0", false},
		// pre-release suffix stripped
		{"v0.4.0", "v0.4.1-rc1", true},
		// dev / garbage never nags
		{"dev", "v0.4.1", false},
		{"", "v0.4.1", false},
		{"v0.4.0", "garbage", false},
	}
	for _, c := range cases {
		if got := isNewer(c.cur, c.latest); got != c.want {
			t.Errorf("isNewer(%q, %q) = %v, want %v", c.cur, c.latest, got, c.want)
		}
	}
}
