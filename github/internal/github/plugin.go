package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/hollis-labs/plugin-sdk/subprocess"
	tangent "github.com/hollis-labs/tangent/pkg/plugin"
)

const (
	ID          = "tangent.plugin.github"
	OpenTool    = "tangent.github_open_pr"
	Kind        = "tangent.external-review"
	ActionPath  = "/api/plugins/github-pr/action"
	StatePath   = "/api/plugins/github-pr/state"
	Description = "Fetch a GitHub PR and send it to Tangent's Inbox with agent summary/notes, PR body and revision-pinned approval/merge controls. Uses the plugin's gh authentication. Markdown fields render as Markdown. No approval or merge occurs until the operator clicks its button. A stable idempotency_key reuses the same retained request; use a new key to review a new commit."
)

var OpenSchema = json.RawMessage(`{"type":"object","properties":{"pr_url":{"type":"string","maxLength":2048},"idempotency_key":{"type":"string","minLength":1,"maxLength":200},"summary":{"type":"string","maxLength":4000},"notes":{"type":"string","maxLength":16000}},"required":["pr_url","idempotency_key"],"additionalProperties":false}`)

type Plugin struct {
	api   API
	tools tangent.ToolCaller
	mu    sync.Mutex
}

func New(api API, tools tangent.ToolCaller) *Plugin { return &Plugin{api: api, tools: tools} }

type OpenInput struct {
	PRURL   string `json:"pr_url"`
	Key     string `json:"idempotency_key"`
	Summary string `json:"summary,omitempty"`
	Notes   string `json:"notes,omitempty"`
}
type Resource struct {
	URL      string `json:"url"`
	Revision string `json:"revision"`
	Label    string `json:"label"`
}
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}
type Action struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Options []Option `json:"options,omitempty"`
}
type Field struct {
	Label string `json:"label"`
	Value string `json:"value"`
}
type Data struct {
	ReviewID  string   `json:"review_id"`
	Title     string   `json:"title"`
	Summary   string   `json:"summary,omitempty"`
	Notes     string   `json:"notes,omitempty"`
	Content   string   `json:"content_markdown"`
	Resource  Resource `json:"resource"`
	Fields    []Field  `json:"fields,omitempty"`
	ActionURL string   `json:"action_url"`
	StateURL  string   `json:"state_url"`
	Actions   []Action `json:"actions"`
}
type Record struct {
	ID         string `json:"interaction_id"`
	State      string `json:"state"`
	Revision   int64  `json:"revision"`
	RoomID     string `json:"legacy_room_id"`
	EnvelopeID string `json:"legacy_envelope_id"`
	Data       Data   `json:"request_snapshot"`
	Definition struct {
		Kind string `json:"kind"`
	} `json:"definition_binding"`
	External struct {
		Envelope struct {
			Meta map[string]string `json:"meta"`
		} `json:"legacy_envelope"`
	} `json:"external_refs"`
}

func (p *Plugin) host(ctx context.Context, name string, input, output any) error {
	result, err := p.tools.CallTool(ctx, name, input)
	if err != nil {
		return err
	}
	return result.Unmarshal(output)
}
func (p *Plugin) records(ctx context.Context, room string) ([]Record, error) {
	result, err := p.tools.CallTool(ctx, "tangent.surface_get", map[string]any{"surface_id": room, "requester_scope": "anonymous"})
	if err != nil {
		return nil, err
	}
	if result.IsError {
		var refusal struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(result.Content, &refusal) == nil && refusal.Code == "not_found" {
			return []Record{}, nil
		}
	}
	var snapshot struct {
		Interactions []Record `json:"interactions"`
	}
	if err = result.Unmarshal(&snapshot); err != nil {
		return nil, err
	}
	return snapshot.Interactions, nil
}

func digest(input any) (string, error) {
	bytes, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(bytes)
	return hex.EncodeToString(hash[:]), nil
}
func (p *Plugin) Open(ctx context.Context, input OpenInput) (map[string]any, error) {
	if input.Key == "" || len(input.Key) > 200 || len(input.Summary) > 4000 || len(input.Notes) > 16000 {
		return nil, errors.New("provide an idempotency_key and keep summary/notes within the advertised bounds")
	}
	target, err := ParseURL(input.PRURL)
	if err != nil {
		return nil, err
	}
	input.PRURL = target.URL()
	key, err := digest(input.Key)
	if err != nil {
		return nil, err
	}
	hash, err := digest(input)
	if err != nil {
		return nil, err
	}
	envelopeID := "github-pr-" + key
	p.mu.Lock()
	defer p.mu.Unlock()
	var list struct {
		Rooms []struct {
			ID string `json:"id"`
		} `json:"rooms"`
	}
	if err = p.host(ctx, "tangent.session_list", map[string]any{"active_only": false}, &list); err != nil {
		return nil, err
	}
	for _, room := range list.Rooms {
		records, readErr := p.records(ctx, room.ID)
		if readErr != nil {
			return nil, readErr
		}
		for _, record := range records {
			if record.EnvelopeID == envelopeID && record.Definition.Kind == Kind {
				if record.External.Envelope.Meta["github_request_hash"] != hash {
					return nil, errors.New("idempotency conflict: that key already names a different PR request")
				}
				return map[string]any{"status": record.State, "room_id": room.ID, "url": "/r/" + room.ID, "item_url": "/inbox/items/" + record.ID, "interaction_id": record.ID, "envelope_id": envelopeID, "handle": map[string]string{"interaction_id": record.ID, "room_id": room.ID, "surface_id": room.ID, "envelope_id": envelopeID, "url": "/r/" + room.ID}, "resume": map[string]any{"get_tool": "tangent.interaction_get", "await_tool": "tangent.interaction_await", "retry_original": true}}, nil
			}
		}
	}
	pr, err := p.pull(ctx, target)
	if err != nil {
		return nil, err
	}
	if pr.Head.SHA == "" {
		return nil, errors.New("GitHub returned no head commit")
	}
	if len(pr.Body) > 180000 {
		return nil, errors.New("PR body exceeds the room payload limit; review this PR on GitHub")
	}
	var repository Repository
	if err = p.api.Do(ctx, "GET", fmt.Sprintf("repos/%s/%s", target.Owner, target.Repo), nil, &repository); err != nil {
		return nil, err
	}
	methods := []Option{}
	if repository.AllowSquash {
		methods = append(methods, Option{"squash", "Squash"})
	}
	if repository.AllowMerge {
		methods = append(methods, Option{"merge", "Merge commit"})
	}
	if repository.AllowRebase {
		methods = append(methods, Option{"rebase", "Rebase"})
	}
	actions := []Action{{ID: "approve", Label: "Approve"}}
	if len(methods) > 0 {
		actions = append(actions, Action{ID: "merge", Label: "Merge", Options: methods})
	}
	data := Data{ReviewID: envelopeID, Title: fmt.Sprintf("%s/%s #%d — %s", target.Owner, target.Repo, target.Number, pr.Title), Summary: input.Summary, Notes: input.Notes, Content: pr.Body, Resource: Resource{target.URL(), pr.Head.SHA, "GitHub"}, Fields: []Field{{"Author", pr.User.Login}, {"Branches", pr.Head.Ref + " → " + pr.Base.Ref}, {"Changes", fmt.Sprintf("%d files · +%d / −%d", pr.ChangedFiles, pr.Additions, pr.Deletions)}}, ActionURL: ActionPath, StateURL: StatePath, Actions: actions}
	if len([]rune(data.Title)) > 200 {
		data.Title = string([]rune(data.Title)[:199]) + "…"
	}
	var created struct {
		RoomID string `json:"roomID"`
		URL    string `json:"url"`
	}
	if err = p.host(ctx, "tangent.session_create", map[string]any{"title": data.Title, "meta": map[string]string{"app": "github", "surface": "pr"}}, &created); err != nil {
		return nil, err
	}
	var result map[string]any
	envelope := map[string]any{"v": 1, "id": envelopeID, "type": Kind, "data": data, "meta": map[string]string{"github_request_hash": hash}}
	if err = p.host(ctx, "tangent.session_advance", map[string]any{"roomID": created.RoomID, "envelope": envelope, "completion": map[string]string{"mode": "async"}}, &result); err != nil {
		return nil, err
	}
	result["url"] = "/r/" + created.RoomID
	result["room_id"] = created.RoomID
	return result, nil
}

type Command struct {
	RoomID     string `json:"room_id"`
	EnvelopeID string `json:"envelope_id"`
	Action     string `json:"action,omitempty"`
	Option     string `json:"option,omitempty"`
}
type State struct {
	Status   string         `json:"status"`
	Message  string         `json:"message"`
	Result   map[string]any `json:"result,omitempty"`
	Complete bool           `json:"complete,omitempty"`
	Disabled []string       `json:"disabled_actions"`
}

func terminal(state string) bool {
	switch state {
	case "resolved", "canceled", "expired", "failed", "superseded":
		return true
	}
	return false
}
func (p *Plugin) read(ctx context.Context, command Command) (Record, Target, error) {
	if command.RoomID == "" || command.EnvelopeID == "" {
		return Record{}, Target{}, errors.New("room_id and envelope_id are required")
	}
	records, err := p.records(ctx, command.RoomID)
	if err != nil {
		return Record{}, Target{}, err
	}
	for _, record := range records {
		if record.EnvelopeID == command.EnvelopeID && record.Definition.Kind == Kind && record.Data.ActionURL == ActionPath && record.Data.StateURL == StatePath {
			target, err := ParseURL(record.Data.Resource.URL)
			return record, target, err
		}
	}
	return Record{}, Target{}, errors.New("this room does not contain that GitHub review")
}
func (p *Plugin) state(ctx context.Context, record Record, t Target, pr PullRequest) (State, error) {
	s := State{Status: pr.State, Message: "Open for review.", Disabled: []string{}}
	if pr.Head.SHA != record.Data.Resource.Revision {
		s.Status = "outdated"
		s.Message = "The PR has new commits. Ask the agent to send a fresh review before approving or merging."
		s.Disabled = []string{"approve", "merge"}
		return s, nil
	}
	if pr.Merged {
		s.Status = "merged"
		s.Message = "Merged on GitHub."
		s.Complete = true
		s.Disabled = []string{"approve", "merge"}
		s.Result = map[string]any{"outcome": "merged", "merge_commit_sha": pr.MergeCommitSHA}
		return s, nil
	}
	if pr.State != "open" {
		s.Message = "This PR is closed."
		s.Disabled = []string{"approve", "merge"}
		s.Result = map[string]any{"outcome": "closed"}
		return s, nil
	}
	if pr.Draft {
		s.Message = "This PR is a draft. Mark it ready for review on GitHub first."
		s.Disabled = []string{"approve", "merge"}
		return s, nil
	}
	login, err := p.viewer(ctx)
	if err != nil {
		return State{}, err
	}
	if strings.EqualFold(pr.User.Login, login) {
		s.Disabled = append(s.Disabled, "approve")
		s.Message = "You authored this PR. GitHub does not allow self-approval; you can still merge it."
	} else {
		review, err := p.approved(ctx, t, pr.Head.SHA, login)
		if err != nil {
			return State{}, err
		}
		if review != nil {
			s.Status = "approved"
			s.Message = "Approved on GitHub. You can merge now or finish this review."
			s.Disabled = append(s.Disabled, "approve")
			s.Result = map[string]any{"outcome": "approved", "github_review_id": review.ID, "review_url": review.HTMLURL}
		}
	}
	if pr.Mergeable != nil && !*pr.Mergeable {
		s.Disabled = append(s.Disabled, "merge")
		s.Message += " Resolve merge conflicts on GitHub before merging."
	}
	if terminal(record.State) {
		s.Disabled = []string{"approve", "merge"}
	}
	return s, nil
}
func (p *Plugin) Execute(ctx context.Context, command Command) (State, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	record, target, err := p.read(ctx, command)
	if err != nil {
		return State{}, err
	}
	pr, err := p.pull(ctx, target)
	if err != nil {
		return State{}, err
	}
	state, err := p.state(ctx, record, target, pr)
	if err != nil {
		return State{}, err
	}
	if command.Action == "" {
		return state, nil
	}
	if terminal(record.State) {
		return State{}, errors.New("this review is finished; send a new review to act again")
	}
	if state.Status == "outdated" {
		return State{}, errors.New(state.Message)
	}
	// A retry reconciles completed external work instead of submitting it again.
	if pr.Merged {
		return state, nil
	}
	if command.Action == "approve" && state.Status == "approved" {
		return state, nil
	}
	allowed := false
	for _, action := range record.Data.Actions {
		if action.ID == command.Action {
			allowed = true
			if len(action.Options) > 0 {
				allowed = false
				for _, option := range action.Options {
					if option.Value == command.Option {
						allowed = true
					}
				}
			}
		}
	}
	if !allowed {
		return State{}, errors.New("that action or method was not offered by this review")
	}
	for _, disabled := range state.Disabled {
		if disabled == command.Action {
			return State{}, errors.New(state.Message)
		}
	}
	switch command.Action {
	case "approve":
		var review Review
		if err = p.api.Do(ctx, "POST", target.Path()+"/reviews", map[string]string{"event": "APPROVE", "commit_id": record.Data.Resource.Revision}, &review); err != nil {
			return State{}, err
		}
		if review.State != "APPROVED" || review.CommitID != record.Data.Resource.Revision {
			return State{}, errors.New("GitHub did not confirm approval of the displayed commit; refresh to reconcile")
		}
		return State{Status: "approved", Message: "Approved on GitHub. You can merge now or finish this review.", Disabled: []string{"approve"}, Result: map[string]any{"outcome": "approved", "github_review_id": review.ID, "review_url": review.HTMLURL}}, nil
	case "merge":
		var merged struct {
			Merged  bool   `json:"merged"`
			SHA     string `json:"sha"`
			Message string `json:"message"`
		}
		if err = p.api.Do(ctx, "PUT", target.Path()+"/merge", map[string]string{"sha": record.Data.Resource.Revision, "merge_method": command.Option}, &merged); err != nil {
			return State{}, err
		}
		if !merged.Merged {
			return State{}, fmt.Errorf("GitHub did not merge the PR: %s", merged.Message)
		}
		return State{Status: "merged", Message: "Merged on GitHub.", Complete: true, Disabled: []string{"approve", "merge"}, Result: map[string]any{"outcome": "merged", "merge_commit_sha": merged.SHA, "merge_method": command.Option}}, nil
	default:
		return State{}, errors.New("unsupported PR action")
	}
}
func (p *Plugin) MCPCallTool(ctx context.Context, request subprocess.MCPCallRequest) (subprocess.MCPCallResult, error) {
	if request.ToolName != OpenTool {
		return subprocess.MCPCallResult{}, errors.New("unknown GitHub plugin tool")
	}
	var input OpenInput
	encodedArgs, err := json.Marshal(request.Arguments)
	if err != nil {
		return subprocess.MCPCallResult{}, err
	}
	if decodeErr := json.Unmarshal(encodedArgs, &input); decodeErr != nil {
		return subprocess.MCPCallResult{}, decodeErr
	}
	result, err := p.Open(ctx, input)
	if err != nil {
		return subprocess.MCPCallResult{}, err
	}
	encoded, err := json.Marshal(result)
	return subprocess.MCPCallResult{Content: encoded}, err
}
func (p *Plugin) HTTPHandle(ctx context.Context, request subprocess.HTTPRequest) (subprocess.HTTPResponse, error) {
	if request.Method != http.MethodPost || (request.Path != ActionPath && request.Path != StatePath) {
		return subprocess.HTTPResponse{}, errors.New("unknown GitHub plugin route")
	}
	var command Command
	decoder := json.NewDecoder(strings.NewReader(string(request.Body)))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&command)
	if err == nil {
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			err = errors.New("expected one JSON command")
		}
	}
	if err == nil && request.Path == StatePath && command.Action != "" {
		err = errors.New("status refresh cannot execute an action")
	}
	if err == nil && request.Path == ActionPath && command.Action == "" {
		err = errors.New("an explicit action is required")
	}
	var result State
	if err == nil {
		result, err = p.Execute(ctx, command)
	}
	status := http.StatusOK
	var body any = result
	if err != nil {
		status = http.StatusConflict
		body = map[string]string{"message": err.Error()}
	}
	encoded, encodeErr := json.Marshal(body)
	return subprocess.HTTPResponse{Status: status, Headers: map[string]string{"Content-Type": "application/json", "Cache-Control": "no-store"}, Body: encoded}, encodeErr
}
