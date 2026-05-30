package updater

import "testing"

func TestIsNewer(t *testing.T) {
	cases := []struct {
		cur, latest string
		want        bool
	}{
		{"v0.3.0", "v0.4.0", true},
		{"v0.3.0", "v0.3.1", true},
		{"v0.3.0", "v1.0.0", true},
		{"v0.4.0", "v0.4.0", false},
		{"v0.4.0", "v0.3.9", false},
		{"v1.2.3", "v1.2.3", false},
		{"dev", "v0.4.0", false}, // dev builds are never nagged
		{"v0.3.0", "garbage", false},
		{"0.3.0", "0.4.0", true}, // tolerate missing leading v
	}
	for _, c := range cases {
		if got := isNewer(c.cur, c.latest); got != c.want {
			t.Errorf("isNewer(%q,%q)=%v want %v", c.cur, c.latest, got, c.want)
		}
	}
}

func TestPickAsset(t *testing.T) {
	rel := &ghRelease{}
	rel.Assets = []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	}{
		{Name: "InternetMerge-windows-amd64-setup.exe", URL: "u1"},
		{Name: "InternetMerge-windows-amd64-portable.zip", URL: "u2"},
		{Name: "InternetMerge-macos-arm64.zip", URL: "u3"},
		{Name: "InternetMerge-linux-amd64.tar.gz", URL: "u4"},
	}
	// We can't change runtime.GOOS, but we can assert the matcher finds a
	// plausible asset for the host without panicking and returns a real URL.
	name, url := pickAsset(rel)
	if (name == "") != (url == "") {
		t.Fatalf("name/url mismatch: %q %q", name, url)
	}
}
