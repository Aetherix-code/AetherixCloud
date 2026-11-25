package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/build"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const maxWebhookBody = 26 << 20 // GitHub caps payloads at 25 MB

// VerifySignature checks X-Hub-Signature-256 ("sha256=<hex>") in constant time.
func VerifySignature(secret, body []byte, header string) bool {
	hexSig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// Sign returns the header value for body (tests and tooling).
func Sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// WebhookHandler is the /api/github/webhook endpoint; any node can serve it.
// It verifies the HMAC, deduplicates on X-GitHub-Delivery and enqueues builds
// (or manages PR previews).
func (m *Module) WebhookHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		ctx := r.Context()
		secret, err := m.WebhookSecret(ctx)
		if err != nil {
			http.Error(w, "GitHub App not configured", http.StatusServiceUnavailable)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
		if err != nil {
			http.Error(w, "read body", http.StatusRequestEntityTooLarge)
			return
		}
		if !VerifySignature(secret, body, r.Header.Get("X-Hub-Signature-256")) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		delivery, event := r.Header.Get("X-GitHub-Delivery"), r.Header.Get("X-GitHub-Event")
		res, err := m.HandleEvent(ctx, delivery, event, body)
		if err != nil {
			m.logger().Warn("github webhook", "event", event, "delivery", delivery, "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(res)
	})
}

// EventResult summarises what a delivery caused.
type EventResult struct {
	Status string   `json:"status"` // ok | duplicate | ignored
	Builds []string `json:"builds,omitempty"`
	Note   string   `json:"note,omitempty"`
}

// HandleEvent processes an already authenticated delivery.
func (m *Module) HandleEvent(ctx context.Context, delivery, event string, body []byte) (*EventResult, error) {
	if delivery != "" {
		if _, err := m.Store.Get(ctx, store.TableSettings, deliveryPrefix+delivery); err == nil {
			return &EventResult{Status: "duplicate"}, nil
		}
	}
	var res *EventResult
	var err error
	switch event {
	case "ping":
		res = &EventResult{Status: "ok", Note: "pong"}
	case "push":
		res, err = m.handlePush(ctx, delivery, body)
	case "pull_request":
		res, err = m.handlePullRequest(ctx, delivery, body)
	case "installation":
		res, err = m.handleInstallation(ctx, body)
	default:
		res = &EventResult{Status: "ignored", Note: "event " + event}
	}
	if err != nil {
		return nil, err // not recorded: GitHub's redelivery is processed again
	}
	// Recorded after success. Two nodes racing on the same delivery still
	// converge because build and preview IDs are derived from the delivery.
	if delivery != "" {
		_ = store.PutJSON(ctx, m.Store, store.TableSettings, deliveryPrefix+delivery, nil,
			map[string]any{"event": event, "at": m.now().Unix()})
	}
	return res, nil
}

// ---- push

type repoRef struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}

type pushEvent struct {
	Ref        string  `json:"ref"`
	After      string  `json:"after"`
	Deleted    bool    `json:"deleted"`
	Repository repoRef `json:"repository"`
	HeadCommit *struct {
		Message string `json:"message"`
		Author  struct {
			Name string `json:"name"`
		} `json:"author"`
	} `json:"head_commit"`
	Pusher struct {
		Name string `json:"name"`
	} `json:"pusher"`
	Commits []struct {
		Added    []string `json:"added"`
		Modified []string `json:"modified"`
		Removed  []string `json:"removed"`
	} `json:"commits"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

func (m *Module) handlePush(ctx context.Context, delivery string, body []byte) (*EventResult, error) {
	var ev pushEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, fmt.Errorf("decode push: %w", err)
	}
	branch, ok := strings.CutPrefix(ev.Ref, "refs/heads/")
	if !ok || ev.Deleted || ev.After == "" || strings.Trim(ev.After, "0") == "" {
		return &EventResult{Status: "ignored", Note: "not a branch update"}, nil
	}
	var changed []string
	for _, c := range ev.Commits {
		changed = append(changed, c.Added...)
		changed = append(changed, c.Modified...)
		changed = append(changed, c.Removed...)
	}
	if len(ev.Commits) >= 20 { // GitHub truncates the commit list: file list unreliable
		changed = nil
	}
	msg, author := "", ev.Pusher.Name
	if ev.HeadCommit != nil {
		msg, author = firstLine(ev.HeadCommit.Message), firstNonEmpty(ev.HeadCommit.Author.Name, author)
	}
	svcs, err := store.ListServices(ctx, m.Store, nil)
	if err != nil {
		return nil, err
	}
	res := &EventResult{Status: "ok"}
	for _, svc := range svcs {
		gh := svc.Spec.GetSource().GetGithub()
		if gh == nil || !strings.EqualFold(gh.Repo, ev.Repository.FullName) || isPreview(svc) {
			continue
		}
		want := firstNonEmpty(gh.Branch, ev.Repository.DefaultBranch)
		if want != branch {
			continue
		}
		if !ShouldBuild(svc.Spec.GetBuild(), changed) {
			res.Note += fmt.Sprintf("%s: no watched paths changed; ", svc.Name)
			continue
		}
		b, _, err := build.Enqueue(ctx, m.Store, build.JobMeta{
			BuildID:   build.DeterministicBuildID(delivery+"/"+svc.Id, svc.Id),
			ServiceID: svc.Id, Kind: "github", Repo: ev.Repository.FullName,
			InstallationID: firstNonZero(ev.Installation.ID, gh.InstallationId),
			Ref:            ev.Ref, SHA: ev.After, Message: msg, Author: author, Delivery: delivery,
		})
		if err != nil {
			return nil, err
		}
		res.Builds = append(res.Builds, b.Id)
	}
	if len(res.Builds) == 0 && res.Note == "" {
		res.Status = "ignored"
	}
	return res, nil
}

func firstNonZero(vs ...int64) int64 {
	for _, v := range vs {
		if v != 0 {
			return v
		}
	}
	return 0
}

// ShouldBuild applies watch paths (and the root directory) to the changed
// files of a push. Unknown changes (nil) always build.
func ShouldBuild(b *v1.BuildSpec, changed []string) bool {
	if len(changed) == 0 {
		return true
	}
	patterns := b.GetWatchPaths()
	if len(patterns) == 0 {
		root := strings.Trim(path.Clean("/"+b.GetContext()), "/")
		if root == "" {
			return true
		}
		patterns = []string{root + "/**"}
	}
	for _, f := range changed {
		for _, p := range patterns {
			if MatchWatchPath(p, f) {
				return true
			}
		}
	}
	return false
}

// MatchWatchPath matches a repo-relative file against a watch pattern:
// "**" spans directories, "*" and "?" stay within one segment, and a pattern
// naming a directory ("apps/web" or "apps/web/") matches everything below it.
func MatchWatchPath(pattern, file string) bool {
	pattern = strings.TrimPrefix(strings.TrimSpace(pattern), "/")
	file = strings.TrimPrefix(file, "/")
	if pattern == "" {
		return true
	}
	if strings.HasSuffix(pattern, "/") {
		pattern += "**"
	}
	var re strings.Builder
	re.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					re.WriteString("(?:.*/)?")
				} else {
					re.WriteString(".*")
				}
			} else {
				re.WriteString("[^/]*")
			}
		case '?':
			re.WriteString("[^/]")
		default:
			re.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	re.WriteString("(?:/.*)?$") // a directory name also matches its contents
	ok, _ := regexp.MatchString(re.String(), file)
	return ok
}

// ---- pull requests

type prEvent struct {
	Action      string  `json:"action"`
	Number      int     `json:"number"`
	Repository  repoRef `json:"repository"`
	PullRequest struct {
		Title string `json:"title"`
		Draft bool   `json:"draft"`
		User  struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			SHA  string `json:"sha"`
			Ref  string `json:"ref"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
	} `json:"pull_request"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

func (m *Module) handlePullRequest(ctx context.Context, delivery string, body []byte) (*EventResult, error) {
	var ev prEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, fmt.Errorf("decode pull_request: %w", err)
	}
	switch ev.Action {
	case "opened", "reopened", "synchronize", "ready_for_review":
		return m.previewUpsert(ctx, delivery, &ev)
	case "closed":
		n, err := m.previewTeardown(ctx, ev.Repository.FullName, ev.Number)
		if err != nil {
			return nil, err
		}
		return &EventResult{Status: "ok", Note: fmt.Sprintf("removed %d preview service(s)", n)}, nil
	}
	return &EventResult{Status: "ignored", Note: "pull_request " + ev.Action}, nil
}

// ---- installation events

func (m *Module) handleInstallation(ctx context.Context, body []byte) (*EventResult, error) {
	var ev struct {
		Action       string `json:"action"`
		Installation struct {
			ID      int64 `json:"id"`
			Account struct {
				Login string `json:"login"`
				Type  string `json:"type"`
			} `json:"account"`
		} `json:"installation"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, err
	}
	in := ev.Installation
	switch ev.Action {
	case "created", "unsuspend", "new_permissions_accepted":
		if err := putInstallation(ctx, m.Store, &v1.Installation{Id: in.ID, Account: in.Account.Login,
			AccountType: in.Account.Type, CreatedAt: timestamppb.Now()}); err != nil {
			return nil, err
		}
	case "deleted":
		_ = m.Store.Delete(ctx, store.TableGitHubInstallations, fmt.Sprint(in.ID))
	default:
		return &EventResult{Status: "ignored", Note: "installation " + ev.Action}, nil
	}
	return &EventResult{Status: "ok"}, nil
}

// PruneDeliveries deletes delivery dedup rows older than the retention.
func (m *Module) PruneDeliveries(ctx context.Context) (int, error) {
	recs, err := m.Store.List(ctx, store.TableSettings, store.Query{})
	if err != nil {
		return 0, err
	}
	n := 0
	cutoff := m.now().Add(-deliveryRetain).UnixNano()
	for _, r := range recs {
		if strings.HasPrefix(r.ID, deliveryPrefix) && r.UpdatedAt < cutoff {
			if m.Store.Delete(ctx, store.TableSettings, r.ID) == nil {
				n++
			}
		}
	}
	return n, nil
}
