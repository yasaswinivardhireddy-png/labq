// Package integrations connects LabQ to the systems around a lab: GitHub (webhooks in, commit
// statuses out) and business-intelligence tools (CSV and newline-delimited JSON exports).
package integrations

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yasaswinivardhireddy-png/labq/internal/lab"
)

// GitHub posts commit statuses with a token.
type GitHub struct {
	API     string // default https://api.github.com
	Token   string
	Context string // status context; default "labq/qualification"
	Client  *http.Client
}

// VerifySignature checks a webhook's X-Hub-Signature-256 header: "sha256=" followed by the hex
// HMAC-SHA256 of the body under the shared secret. The comparison takes constant time.
func VerifySignature(secret, body []byte, header string) bool {
	sig, ok := strings.CutPrefix(header, "sha256=")
	if !ok || len(secret) == 0 {
		return false
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// Sign returns the X-Hub-Signature-256 header value for a body, as GitHub computes it.
func Sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// ParseWebhook turns a push or pull-request delivery into a job source. ok is false for events
// LabQ ignores, such as pings, branch deletions or closed pull requests.
func ParseWebhook(event string, body []byte) (src lab.Source, ok bool, err error) {
	var v struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Deleted    bool   `json:"deleted"`
		Action     string `json:"action"`
		Number     int    `json:"number"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		HeadCommit *struct {
			URL string `json:"url"`
		} `json:"head_commit"`
		PullRequest *struct {
			HTMLURL string `json:"html_url"`
			Head    struct {
				SHA string `json:"sha"`
				Ref string `json:"ref"`
			} `json:"head"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return src, false, fmt.Errorf("github: invalid %s payload: %w", event, err)
	}
	switch event {
	case "push":
		if v.Deleted || strings.Trim(v.After, "0") == "" {
			return src, false, nil
		}
		src = lab.Source{Kind: "push", Repo: v.Repository.FullName, SHA: v.After, Ref: v.Ref}
		if v.HeadCommit != nil {
			src.URL = v.HeadCommit.URL
		}
	case "pull_request":
		if v.PullRequest == nil || (v.Action != "opened" && v.Action != "synchronize" && v.Action != "reopened") {
			return src, false, nil
		}
		src = lab.Source{Kind: "pull_request", Repo: v.Repository.FullName, SHA: v.PullRequest.Head.SHA,
			Ref: v.PullRequest.Head.Ref, PR: v.Number, URL: v.PullRequest.HTMLURL}
	default:
		return src, false, nil
	}
	if src.Repo == "" || len(src.SHA) < 7 {
		return src, false, fmt.Errorf("github: %s payload lacks a repository or commit", event)
	}
	return src, true, nil
}

// SetStatus sets the commit status for sha in repo ("owner/name"). state is pending, success,
// failure or error. It retries server errors and rate limiting with backoff.
func (g *GitHub) SetStatus(ctx context.Context, repo, sha, state, description, targetURL string) error {
	if len(description) > 140 {
		description = description[:137] + "..."
	}
	body, _ := json.Marshal(map[string]string{
		"state": state, "description": description, "context": or(g.Context, "labq/qualification"), "target_url": targetURL,
	})
	url := strings.TrimRight(or(g.API, "https://api.github.com"), "/") + "/repos/" + repo + "/statuses/" + sha
	client := g.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	var last error
	for attempt, wait := 0, 200*time.Millisecond; attempt < 4; attempt, wait = attempt+1, wait*3 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+g.Token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			last = err
			continue
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		switch {
		case resp.StatusCode < 300:
			return nil
		case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
			last = fmt.Errorf("github: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
		default:
			return fmt.Errorf("github: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
		}
	}
	return fmt.Errorf("github: giving up after 4 attempts: %w", last)
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
