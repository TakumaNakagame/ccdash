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
)

const Repo = "TakumaNakagame/ccdash"

type Result struct {
	OldVersion string
	NewVersion string
	BinaryPath string
	NoOp       bool
	Reason     string
}

// Run resolves the latest release and replaces the running binary in
// place when its tag differs from currentVersion. Returns details of the
// outcome; an error is returned only when we tried and failed.
func Run(ctx context.Context, currentVersion string) (Result, error) {
	tag, asset, sumURL, err := latestAsset(ctx)
	if err != nil {
		return Result{}, err
	}
	res := Result{OldVersion: currentVersion, NewVersion: tag}
	if currentVersion != "" && currentVersion != "dev" && tag == currentVersion {
		res.NoOp = true
		res.Reason = "already on the latest release"
		return res, nil
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

func latestAsset(ctx context.Context) (tag, assetURL, sumURL string, err error) {
	req, err := http.NewRequestWithContext(ctx,
		http.MethodGet,
		"https://api.github.com/repos/"+Repo+"/releases/latest", nil)
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 403 + secondary-rate-limit body is the most common cause; surface
		// a hint so the operator can switch to an explicit-version flow
		// (currently only via the install script's CCDASH_VERSION env).
		hint := ""
		if resp.StatusCode == http.StatusForbidden {
			hint = " — likely GitHub anonymous API rate limit (60/hr)"
		}
		return "", "", "", fmt.Errorf("github api: %s%s", resp.Status, hint)
	}
	var rel struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", "", "", err
	}
	if rel.TagName == "" {
		return "", "", "", fmt.Errorf("github api returned no tag name")
	}
	wanted := fmt.Sprintf("ccdash-%s-%s", runtime.GOOS, runtime.GOARCH)
	for _, a := range rel.Assets {
		if a.Name == wanted {
			assetURL = a.URL
		} else if a.Name == wanted+".sha256" {
			sumURL = a.URL
		}
	}
	if assetURL == "" {
		return "", "", "", fmt.Errorf("no release asset for %s/%s in %s", runtime.GOOS, runtime.GOARCH, rel.TagName)
	}
	return rel.TagName, assetURL, sumURL, nil
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
