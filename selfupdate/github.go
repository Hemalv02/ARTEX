package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Repo is the release source. Hard-coded rather than made a config option: a configurable update source means
// a remote-code-execution channel for anyone who can change the config, and for a pentest platform that hole must not be opened.
const Repo = "Hinln/ARTEX"

// GitHubTokenEnv is read only by the backend process; the token is never passed through the frontend or config API.
const GitHubTokenEnv = "ARTEX_UPDATE_GITHUB_TOKEN"

// latestURL is GitHub's "latest stable release" endpoint. It automatically skips prereleases and drafts.
const latestURL = "https://api.github.com/repos/" + Repo + "/releases/latest"

// allowedHosts restricts the domains the upgrade chain can reach. Together with checkRedirect below,
// any hop redirected to a host off the list fails outright — this is the first gate against DNS poisoning / a man-in-the-middle
// swapping the binary; the second gate is the SHA256SUMS comparison.
var allowedHosts = map[string]bool{
	"api.github.com":                       true,
	"github.com":                           true,
	"objects.githubusercontent.com":        true, // the object storage where release assets actually land
	"release-assets.githubusercontent.com": true,
	"raw.githubusercontent.com":            true,
}

// Release is the fields we care about in a GitHub Release.
type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []Asset   `json:"assets"`
}

// Asset is one file attached to a Release.
type Asset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	APIURL string `json:"url"`
	Size   int64  `json:"size"`
}

// Attachments of a private repo must be downloaded with authentication via the Release Asset API.
func (a Asset) downloadURL() string {
	if a.APIURL != "" {
		return a.APIURL
	}
	return a.URL
}

// setUpdateAuth sends the token only to the fixed release repo's HTTPS API. When an attachment redirects to
// GitHub object storage it uses a signed URL, so Authorization must not be forwarded along with it.
func setUpdateAuth(req *http.Request) {
	req.Header.Del("Authorization")
	u := req.URL
	if u.Scheme != "https" || !strings.EqualFold(u.Hostname(), "api.github.com") ||
		(u.Port() != "" && u.Port() != "443") || u.User != nil ||
		!strings.HasPrefix(strings.ToLower(u.EscapedPath()), strings.ToLower("/repos/"+Repo+"/releases/")) {
		return
	}
	if token := strings.TrimSpace(os.Getenv(GitHubTokenEnv)); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// NewClient builds an HTTP client that recognizes only GitHub domains. An empty proxy means a direct connection.
//
// Deliberately does not reuse the default Transport: the upgrade chain must force TLS and validate certificates,
// and must not be affected by an InsecureSkipVerify (or the like) set elsewhere.
func NewClient(proxy string) *http.Client {
	tr := &http.Transport{
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	if p := strings.TrimSpace(proxy); p != "" {
		if pu, err := url.Parse(p); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	return &http.Client{
		Transport: tr,
		Timeout:   30 * time.Minute, // downloads the whole package, so it mustn't be capped by a per-request timeout
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			if err := checkURL(req.URL); err != nil {
				return err
			}
			setUpdateAuth(req)
			return nil
		},
	}
}

// checkURL enforces https + the domain allowlist.
func checkURL(u *url.URL) error {
	if u.Scheme != "https" {
		return fmt.Errorf("refusing non-HTTPS address: %s", u.Scheme+"://"+u.Host)
	}
	if !allowedHosts[strings.ToLower(u.Hostname())] {
		return fmt.Errorf("refusing non-GitHub domain: %s", u.Hostname())
	}
	return nil
}

// FetchLatest queries the latest stable release.
func FetchLatest(ctx context.Context, c *http.Client) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "artex-selfupdate")
	setUpdateAuth(req)

	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to reach GitHub (you can configure a global proxy in system settings): %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, fmt.Errorf("GitHub authentication failed; check the backend environment variable %s", GitHubTokenEnv)
	case resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("GitHub denied access: check the token's repository Contents read permission or API quota")
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("GitHub API rate-limited; please try again later")
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("repository %s is inaccessible or has no official release yet; for private repositories, set %s on the backend (requires Contents read permission)", Repo, GitHubTokenEnv)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub returned %d", resp.StatusCode)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("failed to parse release: %w", err)
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return nil, fmt.Errorf("release is missing a tag")
	}
	return &rel, nil
}

// AssetName returns the release package name for the current platform, matching build.sh's package_binary:
// artex-<version>-<os>-<arch>.zip (the version has no v prefix).
func AssetName(tag, goos, goarch string) string {
	return fmt.Sprintf("artex-%s-%s-%s.zip", strings.TrimPrefix(tag, "v"), goos, goarch)
}

// FindAsset finds an asset in a Release by name.
func (r *Release) FindAsset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if strings.EqualFold(a.Name, name) {
			return a, true
		}
	}
	return Asset{}, false
}
