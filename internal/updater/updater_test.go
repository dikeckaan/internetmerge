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

func TestPickAssetForRealReleaseNames(t *testing.T) {
	rel := &ghRelease{}
	rel.Assets = []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	}{
		{Name: "InternetMerge-macos-arm64.zip", URL: "mac-zip"},
		{Name: "InternetMerge-windows-amd64-portable.zip", URL: "win-amd64-portable"},
		{Name: "InternetMerge-windows-arm64-cli.exe", URL: "win-arm64-cli"},
		{Name: "InternetMerge-linux-arm64-cli.tar.gz", URL: "linux-arm64-cli"},
		{Name: "InternetMerge-windows-arm64-portable.zip", URL: "win-arm64-portable"},
		{Name: "InternetMerge-linux-amd64.tar.gz", URL: "linux-amd64"},
		{Name: "InternetMerge-windows-amd64-cli.exe", URL: "win-amd64-cli"},
		{Name: "InternetMerge-windows-arm64-setup.exe", URL: "win-arm64-setup"},
		{Name: "InternetMerge-macos-arm64.dmg", URL: "mac-dmg"},
		{Name: "InternetMerge-windows-amd64-setup.exe", URL: "win-amd64-setup"},
	}

	cases := []struct {
		goos, arch string
		wantName   string
		wantURL    string
	}{
		{"darwin", "arm64", "InternetMerge-macos-arm64.dmg", "mac-dmg"},
		{"windows", "amd64", "InternetMerge-windows-amd64-setup.exe", "win-amd64-setup"},
		{"windows", "arm64", "InternetMerge-windows-arm64-setup.exe", "win-arm64-setup"},
		{"linux", "amd64", "InternetMerge-linux-amd64.tar.gz", "linux-amd64"},
		{"linux", "arm64", "InternetMerge-linux-arm64-cli.tar.gz", "linux-arm64-cli"},
	}

	for _, c := range cases {
		name, url := pickAssetFor(rel, c.goos, c.arch)
		if name != c.wantName || url != c.wantURL {
			t.Fatalf("pickAssetFor(%s,%s) = (%q,%q), want (%q,%q)", c.goos, c.arch, name, url, c.wantName, c.wantURL)
		}
	}
}

func TestPickAssetForMacFallsBackToZip(t *testing.T) {
	rel := &ghRelease{}
	rel.Assets = []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	}{
		{Name: "InternetMerge-macos-arm64.zip", URL: "mac-zip"},
	}

	name, url := pickAssetFor(rel, "darwin", "arm64")
	if name != "InternetMerge-macos-arm64.zip" || url != "mac-zip" {
		t.Fatalf("pickAssetFor(darwin,arm64) = (%q,%q), want mac zip", name, url)
	}
}
