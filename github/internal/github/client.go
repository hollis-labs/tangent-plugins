// Package github maps GitHub PRs onto Tangent's domain-free review kind.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// API owns GitHub access; Tangent holds neither its credentials nor its client.
type API interface {
	Do(context.Context, string, string, any, any) error
}

// GHAPI deliberately uses the operator's gh authentication. Unlike Cerberus's
// connector this plugin declares that credential source as part of its contract.
type GHAPI struct{}

var apiPath = regexp.MustCompile(`^(user|repos/[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9_.-]+(?:/pulls/[1-9][0-9]*(?:/merge|/reviews(?:\?per_page=100&page=[1-9][0-9]*)?)?)?)$`)

func (GHAPI) Do(ctx context.Context, method, path string, input, output any) error {
	if !apiPath.MatchString(path) {
		return errors.New("invalid GitHub API path")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{"api", "--hostname", "github.com", "--method", method, "-H", "Accept: application/vnd.github+json", "-H", "X-GitHub-Api-Version: 2022-11-28", path}
	var body []byte
	var err error
	if input != nil {
		body, err = json.Marshal(input)
		if err != nil {
			return err
		}
		args = append(args, "--input", "-")
	}
	cmd := exec.CommandContext(ctx, "gh", args...) // #nosec G204 G702 -- fixed program, no shell; validated API path is one argument.
	cmd.Stdin = bytes.NewReader(body)
	// Debug logs and an unrelated default host must never influence this client.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GH_DEBUG=") && !strings.HasPrefix(entry, "GH_HOST=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	encoded, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("GitHub request timed out; refresh to check whether it completed: %w", ctx.Err())
		}
		message := strings.TrimSpace(stderr.String())
		if len(message) > 1000 {
			message = message[:1000]
		}
		if message == "" {
			message = "Install gh and run gh auth login as the Tangent service user."
		}
		return fmt.Errorf("GitHub: %s: %w", message, err)
	}
	if output == nil {
		return nil
	}
	return json.Unmarshal(encoded, output)
}

type Target struct {
	Owner, Repo string
	Number      int
}

var prPath = regexp.MustCompile(`^/([A-Za-z0-9][A-Za-z0-9-]*)/([A-Za-z0-9_.-]+)/pull/([1-9][0-9]*)/?$`)

func ParseURL(raw string) (Target, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil {
		return Target{}, errors.New("use a https://github.com/OWNER/REPO/pull/NUMBER URL")
	}
	parts := prPath.FindStringSubmatch(u.Path)
	if parts == nil || parts[2] == "." || parts[2] == ".." {
		return Target{}, errors.New("invalid GitHub pull request URL")
	}
	number, err := strconv.Atoi(parts[3])
	if err != nil {
		return Target{}, errors.New("invalid pull request number")
	}
	return Target{parts[1], parts[2], number}, nil
}
func (t Target) Path() string { return fmt.Sprintf("repos/%s/%s/pulls/%d", t.Owner, t.Repo, t.Number) }
func (t Target) URL() string {
	return fmt.Sprintf("https://github.com/%s/%s/pull/%d", t.Owner, t.Repo, t.Number)
}

type PullRequest struct {
	Title          string `json:"title"`
	Body           string `json:"body"`
	State          string `json:"state"`
	Draft          bool   `json:"draft"`
	Merged         bool   `json:"merged"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Mergeable      *bool  `json:"mergeable"`
	MergeableState string `json:"mergeable_state"`
	User           struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Additions    int `json:"additions"`
	Deletions    int `json:"deletions"`
	ChangedFiles int `json:"changed_files"`
}
type Repository struct {
	AllowMerge  bool `json:"allow_merge_commit"`
	AllowSquash bool `json:"allow_squash_merge"`
	AllowRebase bool `json:"allow_rebase_merge"`
}
type Review struct {
	ID       int64  `json:"id"`
	State    string `json:"state"`
	CommitID string `json:"commit_id"`
	HTMLURL  string `json:"html_url"`
	User     struct {
		Login string `json:"login"`
	} `json:"user"`
}

func (p *Plugin) pull(ctx context.Context, t Target) (PullRequest, error) {
	var pr PullRequest
	err := p.api.Do(ctx, "GET", t.Path(), nil, &pr)
	return pr, err
}
func (p *Plugin) viewer(ctx context.Context) (string, error) {
	var user struct {
		Login string `json:"login"`
	}
	err := p.api.Do(ctx, "GET", "user", nil, &user)
	if err == nil && user.Login == "" {
		err = errors.New("GitHub did not return the authenticated user")
	}
	return user.Login, err
}
func (p *Plugin) approved(ctx context.Context, t Target, sha, login string) (*Review, error) {
	var latest *Review
	for page := 1; page <= 20; page++ {
		var reviews []Review
		if err := p.api.Do(ctx, "GET", fmt.Sprintf("%s/reviews?per_page=100&page=%d", t.Path(), page), nil, &reviews); err != nil {
			return nil, err
		}
		for _, review := range reviews {
			if strings.EqualFold(review.User.Login, login) && review.State != "PENDING" {
				candidate := review
				latest = &candidate
			}
		}
		if len(reviews) < 100 {
			if latest != nil && latest.State == "APPROVED" && latest.CommitID == sha {
				return latest, nil
			}
			return nil, nil
		}
	}
	return nil, errors.New("PR review history is too large to verify approval safely; review it on GitHub")
}
