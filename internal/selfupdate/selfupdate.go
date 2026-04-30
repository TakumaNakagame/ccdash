// Package selfupdate replaces the running ccdash binary with the latest
// release asset from GitHub. We rely on Linux/macOS allowing a running
// executable to be unlink-replaced (rename onto the same path) which lets
// the next invocation pick up the new build without stopping anything in
// the meantime.
package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

const Repo = "TakumaNakagame/ccdash"

// Channel selects which slice of the release feed to consider. Stable
// pulls only non-prerelease tags (the GitHub /releases/latest endpoint
// already excludes prereleases). Dev pulls the newest published tag,
// prereleases included, so beta builds the maintainer cuts on a Mac
// can be tested on Linux without promoting them to stable first.
type Channel string

const (
	ChannelStable Channel = "stable"
	ChannelDev    Channel = "dev"
)

// ParseChannel normalises a CLI flag value into a Channel. Empty input
// defaults to stable; unknown values are rejected so a typo doesn't
// silently fall back.
func ParseChannel(s string) (Channel, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "stable":
		return ChannelStable, nil
	case "dev", "beta", "prerelease", "pre":
		return ChannelDev, nil
	default:
		return "", fmt.Errorf("unknown channel %q (want stable or dev)", s)
	}
}

type Result struct {
	OldVersion string
	NewVersion string
	BinaryPath string
	NoOp       bool
	Reason     string
}

// LatestTag asks GitHub for the latest release tag in the given channel
// and returns it verbatim (e.g. "v0.3.0" or "v0.3.3-beta.1"). Empty
// channel defaults to stable. Used by the TUI's startup notifier so the
// operator gets a banner when a newer release is available.
func LatestTag(ctx context.Context, channel Channel) (string, error) {
	tag, _, _, err := latestAsset(ctx, channel)
	return tag, err
}

// ReleaseInfo fetches the release body (notes) for the given tag, or
// for the latest release when tag is empty. The body is whatever was
// pasted into the GitHub UI / set by `gh release edit --notes` — we
// don't strip Markdown so the renderer can decide what to do with it.
func ReleaseInfo(ctx context.Context, tag string) (notes string, err error) {
	url := "https://api.github.com/repos/" + Repo + "/releases/latest"
	if tag != "" {
		url = "https://api.github.com/repos/" + Repo + "/releases/tags/" + tag
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		hint := ""
		if resp.StatusCode == http.StatusForbidden {
			hint = " — likely GitHub anonymous API rate limit (60/hr)"
		}
		return "", fmt.Errorf("github api: %s%s", resp.Status, hint)
	}
	var rel struct {
		Body string `json:"body"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", err
	}
	return rel.Body, nil
}

// Run resolves the latest release in the given channel and replaces the
// running binary when its tag is newer than currentVersion. Channel
// "stable" excludes prereleases; channel "dev" includes them. Comparison
// is semver-aware — a v0.3.3-beta.1 on disk plus a v0.3.2 in stable is
// treated as already-newer, not a downgrade prompt.
func Run(ctx context.Context, currentVersion string, channel Channel) (Result, error) {
	tag, asset, sumURL, err := latestAsset(ctx, channel)
	if err != nil {
		return Result{}, err
	}
	res := Result{OldVersion: currentVersion, NewVersion: tag}
	// Skip when current is at or beyond the channel's latest. semver.Compare
	// returns >= 0 when the first arg is newer or equal. Dev / unparseable
	// versions fall back to string equality.
	if currentVersion != "" && currentVersion != "dev" {
		if semver.IsValid(currentVersion) && semver.IsValid(tag) {
			if semver.Compare(currentVersion, tag) >= 0 {
				res.NoOp = true
				res.Reason = fmt.Sprintf("already on %s (latest in %s channel: %s)", currentVersion, channel, tag)
				return res, nil
			}
		} else if tag == currentVersion {
			res.NoOp = true
			res.Reason = "already on the latest release"
			return res, nil
		}
	}
	self, err := os.Executable()
	if err != nil {
		return res, fmt.Errorf("locate self: %w", err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return res, fmt.Errorf("resolve self path: %w", err)
	}
	dir := filepath.Dir(self)
	tmp, err := os.CreateTemp(dir, ".ccdash-update-*")
	if err != nil {
		return res, fmt.Errorf("temp file: %w", err)
	}
	defer os.Remove(tmp.Name())

	if err := download(ctx, asset, tmp); err != nil {
		tmp.Close()
		return res, err
	}
	if err := tmp.Close(); err != nil {
		return res, err
	}
	if sumURL != "" {
		if err := verifyChecksum(ctx, tmp.Name(), sumURL); err != nil {
			return res, err
		}
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return res, fmt.Errorf("chmod: %w", err)
	}
	if err := os.Rename(tmp.Name(), self); err != nil {
		return res, fmt.Errorf("replace binary: %w", err)
	}
	res.BinaryPath = self
	return res, nil
}

// release is the slice of the GitHub releases payload we care about.
type release struct {
	TagName    string `json:"tag_name"`
	Prerelease bool   `json:"prerelease"`
	Draft      bool   `json:"draft"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// latestAsset finds the newest release that matches the channel and
// returns its asset URLs for our GOOS/GOARCH. Stable hits the cheap
// /releases/latest endpoint (already excludes prereleases). Dev hits
// the paged /releases endpoint and picks the highest semver among
// non-draft entries — including prereleases.
func latestAsset(ctx context.Context, channel Channel) (tag, assetURL, sumURL string, err error) {
	if channel == "" {
		channel = ChannelStable
	}
	var picked release
	switch channel {
	case ChannelStable:
		var r release
		if err := getJSON(ctx, "https://api.github.com/repos/"+Repo+"/releases/latest", &r); err != nil {
			return "", "", "", err
		}
		picked = r
	case ChannelDev:
		var rs []release
		if err := getJSON(ctx, "https://api.github.com/repos/"+Repo+"/releases?per_page=20", &rs); err != nil {
			return "", "", "", err
		}
		// Pick the highest semver tag that isn't a draft. The API
		// returns newest-first by created_at, which usually matches
		// semver order, but we sort defensively in case the maintainer
		// re-cuts an older line for a hotfix.
		for _, r := range rs {
			if r.Draft || r.TagName == "" {
				continue
			}
			if !semver.IsValid(r.TagName) {
				continue
			}
			if picked.TagName == "" || semver.Compare(r.TagName, picked.TagName) > 0 {
				picked = r
			}
		}
		if picked.TagName == "" {
			return "", "", "", fmt.Errorf("no release found in dev channel")
		}
	default:
		return "", "", "", fmt.Errorf("unknown channel %q", channel)
	}
	if picked.TagName == "" {
		return "", "", "", fmt.Errorf("github api returned no tag name")
	}
	wanted := fmt.Sprintf("ccdash-%s-%s", runtime.GOOS, runtime.GOARCH)
	for _, a := range picked.Assets {
		if a.Name == wanted {
			assetURL = a.URL
		} else if a.Name == wanted+".sha256" {
			sumURL = a.URL
		}
	}
	if assetURL == "" {
		return "", "", "", fmt.Errorf("no release asset for %s/%s in %s", runtime.GOOS, runtime.GOARCH, picked.TagName)
	}
	return picked.TagName, assetURL, sumURL, nil
}

// getJSON fetches the URL and decodes the body into v. Returns the same
// rate-limit-aware error message as the old inline code.
func getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		hint := ""
		if resp.StatusCode == http.StatusForbidden {
			hint = " — likely GitHub anonymous API rate limit (60/hr)"
		}
		return fmt.Errorf("github api: %s%s", resp.Status, hint)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func download(ctx context.Context, url string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	c := &http.Client{Timeout: 5 * time.Minute}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("download body: %w", err)
	}
	return nil
}

func verifyChecksum(ctx context.Context, path, sumURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sumURL, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		// A missing checksum sidecar shouldn't block the upgrade; only
		// fail when something we DID get back is malformed.
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return nil
	}
	expected := strings.Fields(strings.TrimSpace(string(body)))
	if len(expected) == 0 {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != expected[0] {
		return fmt.Errorf("checksum mismatch (got %s, expected %s)", got, expected[0])
	}
	return nil
}
