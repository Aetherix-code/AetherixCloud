// Package github implements the GitHub App integration: manifest-flow setup,
// encrypted credentials, webhook receiver (HMAC + delivery dedup), build
// enqueueing, installation-token clones, commit statuses / checks, PR preview
// environments and the GitHubService RPCs.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Default endpoints.
const (
	DefaultAPIBase = "https://api.github.com"
	DefaultWebBase = "https://github.com"
)

// Client talks to the GitHub REST API as an App (JWT) or as an installation.
type Client struct {
	APIBase string
	HTTP    *http.Client
	AppID   int64
	Key     *rsa.PrivateKey
	Now     func() time.Time

	mu     sync.Mutex
	tokens map[int64]cachedToken
}

type cachedToken struct {
	token   string
	expires time.Time
}

// APIError is a non-2xx GitHub response.
type APIError struct {
	Status  int
	Message string
	Path    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github %s: %d %s", e.Path, e.Status, e.Message)
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) base() string {
	if c.APIBase != "" {
		return strings.TrimRight(c.APIBase, "/")
	}
	return DefaultAPIBase
}

func (c *Client) httpc() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// ParsePrivateKey decodes a PKCS#1 or PKCS#8 PEM RSA key.
func ParsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		return nil, errors.New("github: invalid private key PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("github: parse private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github: private key is not RSA")
	}
	return rk, nil
}

// AppJWT returns a short-lived RS256 JWT authenticating as the App.
func (c *Client) AppJWT() (string, error) {
	if c.Key == nil {
		return "", errors.New("github: app private key not configured")
	}
	now := c.now()
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-60 * time.Second).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": fmt.Sprint(c.AppID)})
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(hdr) + "." + enc.EncodeToString(claims)
	h := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.Key, crypto.SHA256, h[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

type authKind int

const (
	authNone authKind = iota
	authApp
	authInstall
)

func (c *Client) do(ctx context.Context, kind authKind, installID int64, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "AetherixCloud")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch kind {
	case authApp:
		j, err := c.AppJWT()
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+j)
	case authInstall:
		t, err := c.InstallationToken(ctx, installID)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := c.httpc().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(raw))
		}
		return &APIError{Status: resp.StatusCode, Message: truncate(e.Message, 300), Path: path}
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// InstallationToken returns a cached installation access token.
func (c *Client) InstallationToken(ctx context.Context, installID int64) (string, error) {
	c.mu.Lock()
	if t, ok := c.tokens[installID]; ok && c.now().Add(2*time.Minute).Before(t.expires) {
		c.mu.Unlock()
		return t.token, nil
	}
	c.mu.Unlock()
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := c.do(ctx, authApp, 0, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", installID), map[string]any{}, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", errors.New("github: empty installation token")
	}
	c.mu.Lock()
	if c.tokens == nil {
		c.tokens = map[int64]cachedToken{}
	}
	c.tokens[installID] = cachedToken{token: out.Token, expires: out.ExpiresAt}
	c.mu.Unlock()
	return out.Token, nil
}

// Installation is an App installation.
type Installation struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"account"`
	CreatedAt time.Time `json:"created_at"`
}

// ListInstallations lists installations of the App.
func (c *Client) ListInstallations(ctx context.Context) ([]Installation, error) {
	var all []Installation
	for page := 1; page <= 10; page++ {
		var out []Installation
		if err := c.do(ctx, authApp, 0, http.MethodGet, fmt.Sprintf("/app/installations?per_page=100&page=%d", page), nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out...)
		if len(out) < 100 {
			break
		}
	}
	return all, nil
}

// Repo is a repository visible to an installation.
type Repo struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
}

// ListRepos lists repositories accessible to an installation.
func (c *Client) ListRepos(ctx context.Context, installID int64) ([]Repo, error) {
	var all []Repo
	for page := 1; page <= 20; page++ {
		var out struct {
			Repositories []Repo `json:"repositories"`
		}
		if err := c.do(ctx, authInstall, installID, http.MethodGet, fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page), nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Repositories...)
		if len(out.Repositories) < 100 {
			break
		}
	}
	return all, nil
}

// ListBranches lists branch names of repo.
func (c *Client) ListBranches(ctx context.Context, installID int64, repo string) ([]string, error) {
	var all []string
	for page := 1; page <= 10; page++ {
		var out []struct {
			Name string `json:"name"`
		}
		if err := c.do(ctx, authInstall, installID, http.MethodGet, fmt.Sprintf("/repos/%s/branches?per_page=100&page=%d", repo, page), nil, &out); err != nil {
			return nil, err
		}
		for _, b := range out {
			all = append(all, b.Name)
		}
		if len(out) < 100 {
			break
		}
	}
	return all, nil
}

// Commit is the subset of a commit the pipeline needs.
type Commit struct {
	SHA     string
	Message string
	Author  string
}

// GetCommit resolves ref (branch, tag or SHA) to a commit.
func (c *Client) GetCommit(ctx context.Context, installID int64, repo, ref string) (*Commit, error) {
	var out struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
			Author  struct {
				Name string `json:"name"`
			} `json:"author"`
		} `json:"commit"`
	}
	if err := c.do(ctx, authInstall, installID, http.MethodGet, fmt.Sprintf("/repos/%s/commits/%s", repo, url.PathEscape(ref)), nil, &out); err != nil {
		return nil, err
	}
	return &Commit{SHA: out.SHA, Message: firstLine(out.Commit.Message), Author: out.Commit.Author.Name}, nil
}

// Status is a commit status.
type Status struct {
	State       string `json:"state"` // pending | success | failure | error
	TargetURL   string `json:"target_url,omitempty"`
	Description string `json:"description,omitempty"`
	Context     string `json:"context"`
}

// CreateStatus posts a commit status.
func (c *Client) CreateStatus(ctx context.Context, installID int64, repo, sha string, s Status) error {
	s.Description = truncate(s.Description, 140)
	return c.do(ctx, authInstall, installID, http.MethodPost, fmt.Sprintf("/repos/%s/statuses/%s", repo, sha), s, nil)
}

// CheckRun is a GitHub check run (create / update body).
type CheckRun struct {
	Name       string `json:"name,omitempty"`
	HeadSHA    string `json:"head_sha,omitempty"`
	Status     string `json:"status,omitempty"`     // queued | in_progress | completed
	Conclusion string `json:"conclusion,omitempty"` // success | failure | ...
	DetailsURL string `json:"details_url,omitempty"`
	Output     *struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	} `json:"output,omitempty"`
}

// CreateCheckRun creates a check run and returns its ID.
func (c *Client) CreateCheckRun(ctx context.Context, installID int64, repo string, cr CheckRun) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	err := c.do(ctx, authInstall, installID, http.MethodPost, fmt.Sprintf("/repos/%s/check-runs", repo), cr, &out)
	return out.ID, err
}

// UpdateCheckRun patches a check run.
func (c *Client) UpdateCheckRun(ctx context.Context, installID int64, repo string, id int64, cr CheckRun) error {
	return c.do(ctx, authInstall, installID, http.MethodPatch, fmt.Sprintf("/repos/%s/check-runs/%d", repo, id), cr, nil)
}

// CommentPR adds a comment to a pull request (issues API).
func (c *Client) CommentPR(ctx context.Context, installID int64, repo string, number int, body string) error {
	return c.do(ctx, authInstall, installID, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number), map[string]string{"body": body}, nil)
}

// SetWebhookURL updates the App's webhook URL.
func (c *Client) SetWebhookURL(ctx context.Context, hookURL string) error {
	return c.do(ctx, authApp, 0, http.MethodPatch, "/app/hook/config", map[string]string{"url": hookURL}, nil)
}

// ManifestConversion is the response of the manifest code exchange.
type ManifestConversion struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	HTMLURL       string `json:"html_url"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	WebhookSecret string `json:"webhook_secret"`
	PEM           string `json:"pem"`
}

// ConvertManifest exchanges the temporary manifest code for the App
// credentials (unauthenticated endpoint).
func (c *Client) ConvertManifest(ctx context.Context, code string) (*ManifestConversion, error) {
	var out ManifestConversion
	if err := c.do(ctx, authNone, 0, http.MethodPost, "/app-manifests/"+url.PathEscape(code)+"/conversions", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}
