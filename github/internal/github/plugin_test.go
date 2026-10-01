package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/hollis-labs/plugin-sdk/subprocess"
	"os"
	"strings"
	"testing"

	tangent "github.com/hollis-labs/tangent/pkg/plugin"
)

const testSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func decode(value, output any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, output)
}

type fakeAPI struct {
	pr        PullRequest
	reviews   []Review
	writes    []map[string]string
	denyMerge bool
	failure   error
}

func newAPI() *fakeAPI {
	f := &fakeAPI{}
	f.pr.Title = "Improve Inbox"
	f.pr.Body = "## PR body\nOriginal description."
	f.pr.State = "open"
	f.pr.User.Login = "author"
	f.pr.Head.SHA = testSHA
	f.pr.Head.Ref = "feature"
	f.pr.Base.Ref = "main"
	return f
}
func (f *fakeAPI) Do(_ context.Context, method, path string, input, output any) error {
	if f.failure != nil {
		return f.failure
	}
	if method == "GET" {
		switch {
		case path == "user":
			return decode(map[string]string{"login": "reviewer"}, output)
		case path == "repos/team/repo":
			return decode(Repository{AllowMerge: true, AllowSquash: true}, output)
		case strings.Contains(path, "/reviews?"):
			return decode(f.reviews, output)
		case path == "repos/team/repo/pulls/123":
			return decode(f.pr, output)
		default:
			return fmt.Errorf("unexpected read %s", path)
		}
	}
	payload := input.(map[string]string)
	f.writes = append(f.writes, payload)
	switch {
	case method == "POST" && path == "repos/team/repo/pulls/123/reviews":
		if payload["event"] != "APPROVE" || payload["commit_id"] != testSHA {
			return errors.New("wrong approval commit")
		}
		review := Review{ID: 12, State: "APPROVED", CommitID: testSHA, HTMLURL: "https://github.com/team/repo/pull/123#pullrequestreview-12"}
		review.User.Login = "reviewer"
		f.reviews = append(f.reviews, review)
		return decode(review, output)
	case method == "PUT" && path == "repos/team/repo/pulls/123/merge":
		if payload["sha"] != testSHA || payload["merge_method"] != "squash" {
			return errors.New("wrong merge payload")
		}
		if f.denyMerge {
			return decode(map[string]any{"merged": false, "message": "Required checks have not passed"}, output)
		}
		f.pr.Merged = true
		f.pr.State = "closed"
		f.pr.MergeCommitSHA = "merge-sha"
		return decode(map[string]any{"merged": true, "sha": "merge-sha"}, output)
	default:
		return fmt.Errorf("unexpected write %s %s", method, path)
	}
}

type fakeHost struct {
	records []Record
	creates int
}

func hostResult(value any) (tangent.ToolResult, error) {
	encoded, err := json.Marshal(value)
	return tangent.ToolResult{Content: encoded}, err
}
func (h *fakeHost) CallTool(_ context.Context, name string, input any) (tangent.ToolResult, error) {
	args := input.(map[string]any)
	switch name {
	case "tangent.session_list":
		if len(h.records) == 0 {
			return hostResult(map[string]any{"rooms": []any{}})
		}
		return hostResult(map[string]any{"rooms": []any{map[string]string{"id": "room-1"}}})
	case "tangent.surface_get":
		return hostResult(map[string]any{"interactions": h.records})
	case "tangent.session_create":
		h.creates++
		return hostResult(map[string]string{"roomID": "room-1", "url": "http://localhost/r/room-1"})
	case "tangent.session_advance":
		env := args["envelope"].(map[string]any)
		var data Data
		if err := decode(env["data"], &data); err != nil {
			return tangent.ToolResult{}, err
		}
		record := Record{ID: "interaction-1", RoomID: "room-1", EnvelopeID: env["id"].(string), State: "presented", Revision: 3, Data: data}
		record.Definition.Kind = Kind
		record.External.Envelope.Meta = env["meta"].(map[string]string)
		h.records = append(h.records, record)
		return hostResult(map[string]any{"status": "pending", "handle": map[string]string{"interaction_id": record.ID, "room_id": record.RoomID, "envelope_id": record.EnvelopeID}})
	default:
		return tangent.ToolResult{}, fmt.Errorf("unexpected host tool %s", name)
	}
}
func fixture(t *testing.T) (*Plugin, *fakeAPI, *fakeHost, Command) {
	t.Helper()
	api := newAPI()
	host := &fakeHost{}
	p := New(api, host)
	_, err := p.Open(context.Background(), OpenInput{PRURL: "https://github.com/team/repo/pull/123", Key: "review-1", Summary: "Agent summary", Notes: "Agent notes"})
	if err != nil {
		t.Fatal(err)
	}
	return p, api, host, Command{RoomID: "room-1", EnvelopeID: host.records[0].EnvelopeID}
}
func TestOpenRetainsPRBodyAndIsIdempotentAcrossPluginRestart(t *testing.T) {
	p, api, host, _ := fixture(t)
	data := host.records[0].Data
	if data.Content != api.pr.Body || data.Summary != "Agent summary" || data.Notes != "Agent notes" || data.Resource.Revision != testSHA {
		t.Fatal("lost PR snapshot or agent notes")
	}
	if len(data.Actions) != 2 || data.Actions[1].Options[0].Value != "squash" {
		t.Fatal("missing repository merge methods")
	}
	api.pr.Head.SHA = "new-commit"
	api.pr.Body = "New body"
	restarted := New(api, host)
	result, err := restarted.Open(context.Background(), OpenInput{PRURL: "https://github.com/team/repo/pull/123", Key: "review-1", Summary: "Agent summary", Notes: "Agent notes"})
	if err != nil || result["interaction_id"] != "interaction-1" || host.creates != 1 {
		t.Fatalf("retry created another request: %v %v", result, err)
	}
	if _, err = p.Open(context.Background(), OpenInput{PRURL: "https://github.com/team/repo/pull/123", Key: "review-1", Notes: "Changed"}); err == nil {
		t.Fatal("accepted different payload under same key")
	}
	if len(api.writes) != 0 {
		t.Fatal("opening wrote to GitHub")
	}
}
func TestApproveMergeAndReconcileRetries(t *testing.T) {
	p, api, host, cmd := fixture(t)
	ctx := context.Background()
	if _, err := p.Execute(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	if len(api.writes) != 0 {
		t.Fatal("view executed an action")
	}
	cmd.Action = "approve"
	approved, err := p.Execute(ctx, cmd)
	if err != nil || approved.Status != "approved" || approved.Complete {
		t.Fatalf("approval: %+v %v", approved, err)
	}
	p = New(api, host)
	if _, err = p.Execute(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	if len(api.writes) != 1 {
		t.Fatal("retry duplicated approval")
	}
	cmd.Action = "merge"
	cmd.Option = "squash"
	merged, err := p.Execute(ctx, cmd)
	if err != nil || !merged.Complete || merged.Result["merge_commit_sha"] != "merge-sha" {
		t.Fatalf("merge: %+v %v", merged, err)
	}
	p = New(api, host)
	if _, err = p.Execute(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	if len(api.writes) != 2 {
		t.Fatal("retry duplicated merge")
	}
	host.records[0].State = "resolved"
	if _, err = p.Execute(ctx, cmd); err == nil {
		t.Fatal("terminal review still acts")
	}
}
func TestStaleDraftSelfApprovalAndInvalidActionsNeverWrite(t *testing.T) {
	for _, name := range []string{"stale", "draft", "self", "method", "action", "wrong-envelope"} {
		t.Run(name, func(t *testing.T) {
			p, api, _, cmd := fixture(t)
			cmd.Action = "approve"
			switch name {
			case "stale":
				api.pr.Head.SHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			case "draft":
				api.pr.Draft = true
			case "self":
				api.pr.User.Login = "reviewer"
			case "method":
				cmd.Action = "merge"
				cmd.Option = "unknown"
			case "action":
				cmd.Action = "delete"
			case "wrong-envelope":
				cmd.EnvelopeID = "another-request"
			}
			if _, err := p.Execute(context.Background(), cmd); err == nil {
				t.Fatal("accepted refused action")
			}
			if len(api.writes) != 0 {
				t.Fatal("refused request wrote to GitHub")
			}
		})
	}
}
func TestMergeRefusalDoesNotClaimCompletion(t *testing.T) {
	p, api, _, cmd := fixture(t)
	api.denyMerge = true
	cmd.Action = "merge"
	cmd.Option = "squash"
	result, err := p.Execute(context.Background(), cmd)
	if err == nil || result.Complete {
		t.Fatal("claimed failed merge succeeded")
	}
}
func TestGitHubURLCannotSelectAnotherHostOrAPIPath(t *testing.T) {
	for _, raw := range []string{"http://github.com/team/repo/pull/1", "https://github.com.evil.test/team/repo/pull/1", "https://github.com/team/../pull/1", "https://github.com/user:pass@evil.test/team/repo/pull/1", "https://github.com/team/repo/pull/1/merge", "https://github.com/team/repo/pull/0", "https://github.com:443/team/repo/pull/1"} {
		if _, err := ParseURL(raw); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}
func TestLiveRead(t *testing.T) {
	raw := os.Getenv("TANGENT_GITHUB_LIVE_TEST")
	if raw == "" {
		t.Skip("opt-in read-only GitHub verification")
	}
	target, err := ParseURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	p := New(GHAPI{}, nil)
	pr, err := p.pull(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Head.SHA == "" || pr.Title == "" {
		t.Fatal("GitHub did not return the real PR shape")
	}
	if _, err = p.viewer(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStatusRouteCannotExecuteActionsOrAcceptExtraCommands(t *testing.T) {
	p, api, _, cmd := fixture(t)
	cmd.Action = "approve"
	body, err := json.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path string
		body []byte
	}{
		{StatePath, body},
		{ActionPath, append(append([]byte{}, body...), []byte(` {"action":"merge"}`)...)},
		{ActionPath, []byte(`{"room_id":"room-1","envelope_id":"bad","unknown":true}`)},
	} {
		response, err := p.HTTPHandle(context.Background(), subprocess.HTTPRequest{Method: "POST", Path: test.path, Body: test.body})
		if err != nil || response.Status != 409 {
			t.Fatalf("invalid command accepted: %+v %v", response, err)
		}
	}
	if len(api.writes) != 0 {
		t.Fatal("invalid command wrote to GitHub")
	}
}
