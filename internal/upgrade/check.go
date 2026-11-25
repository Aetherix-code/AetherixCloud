package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultRepo = "Aetherix-code/AetherixCloud"

// Release is a resolved GitHub release.
type Release struct {
	Tag         string
	URL         string
	Notes       string
	PublishedAt time.Time
}

// CheckLatest resolves the newest non-draft release for repo.
// channel is currently informational (both map to GitHub "latest").
func CheckLatest(ctx context.Context, repo, channel string, hc *http.Client) (Release, error) {
	if repo == "" {
		repo = defaultRepo
	}
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	_ = channel
	url := "https://api.github.com/repos/" + repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "aetherixcloud-upgrade")
	resp, err := hc.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("github releases: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var raw struct {
		TagName     string `json:"tag_name"`
		HTMLURL     string `json:"html_url"`
		Body        string `json:"body"`
		PublishedAt string `json:"published_at"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Release{}, err
	}
	if raw.TagName == "" {
		return Release{}, fmt.Errorf("github releases: empty tag")
	}
	rel := Release{Tag: Normalize(raw.TagName), URL: raw.HTMLURL, Notes: trimNotes(raw.Body)}
	if t, err := time.Parse(time.RFC3339, raw.PublishedAt); err == nil {
		rel.PublishedAt = t
	}
	return rel, nil
}

func trimNotes(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 2000 {
		return s[:2000] + "…"
	}
	return s
}

func repoOrDefault(repo string) string {
	if repo == "" {
		return defaultRepo
	}
	return repo
}
