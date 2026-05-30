// Package updater checks GitHub Releases for a newer InternetMerge build and,
// on the user's request, downloads the right asset for this OS/arch and hands
// off to it (runs the Windows installer, opens the macOS app, etc.). It is
// intentionally conservative: it never replaces files silently.
package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/kaandikec/internetmerge/internal/version"
)

// Repo is the GitHub owner/repo that publishes releases.
const Repo = "dikeckaan/internetmerge"

// Info describes an available update relative to the running build.
type Info struct {
	Available      bool   `json:"available"`
	CurrentVersion string `json:"currentVersion"`
	LatestVersion  string `json:"latestVersion"`
	Notes          string `json:"notes"`
	AssetName      string `json:"assetName"`
	AssetURL       string `json:"assetURL"`
	HTMLURL        string `json:"htmlURL"` // release page (fallback for the user)
}

type ghRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Body       string `json:"body"`
	HTMLURL    string `json:"html_url"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// Check queries the latest release and reports whether it is newer than the
// running version, picking the asset that matches this OS/arch.
func Check(ctx context.Context) (*Info, error) {
	cur := version.Version
	info := &Info{CurrentVersion: cur}

	rel, err := latestRelease(ctx)
	if err != nil {
		return nil, err
	}
	info.LatestVersion = rel.TagName
	info.Notes = rel.Body
	info.HTMLURL = rel.HTMLURL

	if !isNewer(cur, rel.TagName) {
		return info, nil // up to date (or dev build)
	}

	name, url := pickAsset(rel)
	info.AssetName = name
	info.AssetURL = url
	// Even if we can't match an exact asset, mark it available so the UI can
	// offer "open the release page".
	info.Available = true
	return info, nil
}

// latestRelease fetches the newest non-draft release.
func latestRelease(ctx context.Context) (*ghRelease, error) {
	url := "https://api.github.com/repos/" + Repo + "/releases/latest"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "InternetMerge-updater")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("updater: GitHub returned %s", resp.Status)
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

// pickAsset chooses the best release asset for the current OS/arch. It prefers
// the GUI build and falls back to the CLI; on Windows it prefers the installer.
func pickAsset(rel *ghRelease) (name, url string) {
	goos, arch := runtime.GOOS, runtime.GOARCH
	// Candidate name fragments, most-preferred first.
	var prefs []string
	switch goos {
	case "windows":
		if arch == "arm64" {
			prefs = []string{"windows-arm64-setup", "windows-arm64-portable", "windows-arm64-cli", "windows-arm64"}
		} else {
			prefs = []string{"windows-amd64-setup", "windows-amd64-portable", "windows-amd64-cli", "windows-amd64"}
		}
	case "darwin":
		// Prefer the .dmg (drag-drop installer) over the raw .zip.
		if arch == "arm64" {
			prefs = []string{"macos-arm64.dmg", "macos-arm64", "darwin-arm64"}
		} else {
			prefs = []string{"macos-intel.dmg", "macos-intel", "macos-amd64", "darwin-amd64"}
		}
	case "linux":
		if arch == "arm64" {
			prefs = []string{"linux-arm64"}
		} else {
			prefs = []string{"linux-amd64"}
		}
	}
	for _, frag := range prefs {
		for _, a := range rel.Assets {
			if strings.Contains(strings.ToLower(a.Name), frag) {
				return a.Name, a.URL
			}
		}
	}
	return "", ""
}

// Download fetches the update asset to a temp file and returns its path.
func Download(ctx context.Context, info *Info) (string, error) {
	if info.AssetURL == "" {
		return "", fmt.Errorf("updater: no downloadable asset for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, info.AssetURL, nil)
	req.Header.Set("User-Agent", "InternetMerge-updater")
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("updater: download returned %s", resp.Status)
	}

	dir, err := os.MkdirTemp("", "internetmerge-update-")
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, info.AssetName)
	f, err := os.Create(out)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return out, nil
}

// isNewer reports whether latest (a tag like "v0.4.0") is strictly newer than
// current. Dev/empty current is treated as "not newer" so devs aren't nagged.
func isNewer(current, latest string) bool {
	c := parseSemver(current)
	l := parseSemver(latest)
	if c == nil || l == nil {
		return false
	}
	for i := 0; i < 3; i++ {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

// parseSemver turns "v1.2.3" (or "1.2.3") into [3]int{1,2,3}; returns nil for
// non-semver inputs (e.g. "dev").
func parseSemver(s string) []int {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "V")
	if s == "" {
		return nil
	}
	parts := strings.SplitN(s, ".", 3)
	out := make([]int, 3)
	for i := 0; i < 3; i++ {
		if i >= len(parts) {
			out[i] = 0
			continue
		}
		// strip any pre-release suffix like "3-rc1"
		num := parts[i]
		if j := strings.IndexAny(num, "-+"); j >= 0 {
			num = num[:j]
		}
		n, err := strconv.Atoi(num)
		if err != nil {
			return nil
		}
		out[i] = n
	}
	return out
}
