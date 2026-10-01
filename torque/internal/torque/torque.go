package torque

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// This file is the plugin's Torque dependency, and it is the only file in this
// repository that knows Torque exists.
//
// That placement is the decision, not an accident of layout (Tesseract
// `agents_drive_tangent_apps_are_called`, 2026-09-09): Tangent core stays
// domain-free, Torque stays an engine that is called and returns, and the
// dependency lives in userland — a plugin — because that is the one place a
// dependency is a feature rather than a leak. Chrispian, on plugins: "Plugins
// that create dependencies is userland concerns."
//
// Torque's HTTP API is the surface this uses rather than its MCP one. Both
// exist; the plugin owns the choice, and HTTP wins because it is a plain
// request/response with no session to hold and no second protocol stack inside
// a process that already runs one.
//
// **If Torque is down, this reports it and nothing else in Tangent notices.**
// That is confirmed expected behavior, and it is why every call here returns
// an error a tool result can carry rather than panicking, retrying forever, or
// caching a stale answer that would be indistinguishable from a fresh one.

// DefaultBaseURL is where torque-api-service listens on this machine.
const DefaultBaseURL = "http://127.0.0.1:8990"

// BaseURLEnv overrides DefaultBaseURL. The plugin reads it itself rather than
// asking the host: GetConfig is deliberately unimplemented (plugin
// configuration has no owner in Tangent yet), and a plugin reading its own
// environment is userland doing userland's job.
const BaseURLEnv = "TANGENT_TORQUE_API_URL"

// requestTimeout bounds one Torque call. A board is opened by a person waiting
// for it, so a hung dependency has to become a visible refusal quickly rather
// than a spinner.
const requestTimeout = 10 * time.Second

// ErrTorqueUnavailable reports that Torque could not be reached or answered
// unusably. It is distinguishable so a tool result can say "Torque is down"
// rather than "something failed", which is the difference between an operator
// restarting a service and an operator reading Tangent's logs.
var ErrTorqueUnavailable = errors.New("torque: torque is unavailable")

// Task is the subset of a Torque task this board renders. It is deliberately
// not every column Torque returns: a field nothing displays is a field that
// would silently become part of this plugin's contract with Torque.
type Task struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	Priority    int      `json:"priority"`
	Kind        string   `json:"kind"`
	Executor    string   `json:"executor"`
	AgentFile   string   `json:"agent_file"`
	ProjectID   string   `json:"project_id"`
	SprintID    string   `json:"sprint_id"`
	EpicID      string   `json:"epic_id"`
	Tags        []Tag    `json:"tags"`
	DependsOn   []string `json:"depends_on"`
	UpdatedAt   string   `json:"updated_at"`
	CreatedAt   string   `json:"created_at"`
}

// Tag is one Torque tag as the list endpoint returns it.
type Tag struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// TagSlugs flattens the tags for badge and filter construction.
func (t Task) TagSlugs() []string {
	slugs := make([]string, 0, len(t.Tags))
	for _, tag := range t.Tags {
		if tag.Slug != "" {
			slugs = append(slugs, tag.Slug)
		}
	}
	return slugs
}

// ListFilters are the facets torque_task_list already takes, narrowed to the
// ones a board scopes by. They are passed through to Torque verbatim; this
// plugin invents no filter vocabulary of its own.
type ListFilters struct {
	Statuses  []string `json:"statuses,omitempty"`
	ProjectID string   `json:"project_id,omitempty"`
	SprintID  string   `json:"sprint_id,omitempty"`
	EpicID    string   `json:"epic_id,omitempty"`
	Kind      string   `json:"kind,omitempty"`
	Executor  string   `json:"executor,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	Search    string   `json:"search,omitempty"`
	Limit     int      `json:"limit,omitempty"`
}

// Client is a Torque HTTP client.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient builds a client against baseURL, falling back to the environment
// and then to the local default.
func NewClient(baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		http:    &http.Client{Timeout: requestTimeout},
	}
}

// BaseURL is where this client points. Reported in the board's `source` block
// so a reader can tell which Torque they are looking at.
func (c *Client) BaseURL() string { return c.baseURL }

// TaskPage is one bounded page of Torque tasks, and whether Torque held more.
//
// It is a struct rather than a slice for one field: `More`. A board that sent
// a cut set and reported only a count reads as the whole set, which is
// CW-20260910-0043 — the participant filters, finds nothing, and cannot tell
// whether the task is absent from Torque or was simply never sent.
type TaskPage struct {
	// Tasks is the page, already trimmed to the requested limit.
	Tasks []Task
	// More says Torque held at least one further match. It is exact, not a
	// guess from `len(Tasks) == limit`.
	//
	// The client needs rows and a continuation signal, not a match count.
	// It accepts both legacy {tasks,...} and current {items,meta} responses
	// during the Torque envelope rollout without requesting include_total.
	More bool
}

// ListTasks returns the tasks matching filters, bounded by the card limit.
//
// Statuses are joined with commas because that is the shape Torque's own
// handler splits on; everything else is one parameter per facet.
//
// The limit sent to Torque is one higher than the caller's. That extra row is
// never rendered; it supplies the truncation signal, and it is Torque's own
// idiom — `torque_task_list` determines `has_more` the same way ("fetch one
// extra row beyond limit"). It costs one record on a request already in
// flight, where a count would cost a second round trip.
func (c *Client) ListTasks(ctx context.Context, filters ListFilters) (TaskPage, error) {
	limit := clampCards(filters.Limit)
	query := url.Values{}
	if len(filters.Statuses) > 0 {
		query.Set("status", strings.Join(filters.Statuses, ","))
	}
	setIfPresent(query, "project_id", filters.ProjectID)
	setIfPresent(query, "sprint_id", filters.SprintID)
	setIfPresent(query, "epic_id", filters.EpicID)
	setIfPresent(query, "kind", filters.Kind)
	setIfPresent(query, "executor", filters.Executor)
	setIfPresent(query, "search", filters.Search)
	if len(filters.Tags) > 0 {
		query.Set("tags", strings.Join(filters.Tags, ","))
	}
	query.Set("limit", strconv.Itoa(limit+1))

	var response struct {
		Tasks   json.RawMessage `json:"tasks"`
		Items   json.RawMessage `json:"items"`
		Meta    json.RawMessage `json:"meta"`
		HasMore *bool           `json:"has_more"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/tasks?"+query.Encode(), nil, &response); err != nil {
		return TaskPage{}, err
	}
	rows := response.Tasks
	more := response.HasMore != nil && *response.HasMore
	if len(response.Items) > 0 {
		if len(response.Tasks) > 0 || bytes.Equal(bytes.TrimSpace(response.Items), []byte("null")) {
			return TaskPage{}, fmt.Errorf("%w: invalid task-list envelope", ErrTorqueUnavailable)
		}
		var meta struct {
			HasMore *bool `json:"has_more"`
		}
		if err := json.Unmarshal(response.Meta, &meta); err != nil {
			return TaskPage{}, fmt.Errorf("%w: decoding task-list meta: %w", ErrTorqueUnavailable, err)
		}
		if meta.HasMore == nil {
			return TaskPage{}, fmt.Errorf("%w: task-list meta is missing has_more", ErrTorqueUnavailable)
		}
		rows, more = response.Items, *meta.HasMore
	}
	// A missing list is an unusable answer, not an empty board. Legacy null
	// tasks remain valid empty pages, as the older API can emit a nil slice.
	if len(rows) == 0 {
		return TaskPage{}, fmt.Errorf("%w: task-list response is missing tasks or items", ErrTorqueUnavailable)
	}
	var tasks []Task
	if err := json.Unmarshal(rows, &tasks); err != nil {
		return TaskPage{}, fmt.Errorf("%w: decoding task-list rows: %w", ErrTorqueUnavailable, err)
	}
	if len(tasks) > limit {
		return TaskPage{Tasks: tasks[:limit], More: true}, nil
	}
	return TaskPage{Tasks: tasks, More: more}, nil
}

// Transition moves one task to a new status.
//
// force is passed for a task leaving a terminal status: Torque guards `done`
// and `archived` behind it deliberately, "so a finished task is not silently
// reopened by a stray call". A board drag out of the done column is not a
// stray call — it is a person doing it on purpose — so the caller decides,
// and this reports Torque's own refusal message when it says no.
func (c *Client) Transition(ctx context.Context, taskID, status string, force bool) error {
	body := map[string]any{"status": status, "force": force}
	return c.do(ctx, http.MethodPost,
		"/api/v1/tasks/"+url.PathEscape(taskID)+"/transition", body, nil)
}

// Health reports whether Torque is answering at all. It is what the board's
// `source` block and a refusal message are built from.
func (c *Client) Health(ctx context.Context) error {
	var ignored json.RawMessage
	return c.do(ctx, http.MethodGet, "/api/v1/tasks?limit=1", nil, &ignored)
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("torque: encode request: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}
	// #nosec G704 -- the URL is `c.baseURL` (operator configuration, from
	// TANGENT_TORQUE_API_URL or the loopback default) joined to a `path`
	// literal from this file. gosec's taint analysis flags it because the base
	// reaches here from os.Getenv, which is true and is the point: pointing at
	// a named Torque is what the environment override is for. The filters a
	// caller supplies ride in `path` only as url.Values.Encode() output, which
	// percent-encodes them into the query of a fixed route — no
	// participant-supplied value can move the host, the scheme or the path.
	// The same annotation, with the same reasoning, is on the Tesseract
	// plugin's client; this one went unflagged only until a test in this
	// package built a client the way production does.
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, payload)
	if err != nil {
		return fmt.Errorf("torque: build request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.http.Do(request) // #nosec G704 -- see the request above
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrTorqueUnavailable, c.baseURL, err)
	}
	defer func() { _ = response.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseBytes))
	if err != nil {
		return fmt.Errorf("%w: reading %s: %w", ErrTorqueUnavailable, path, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Torque's own message is carried through rather than replaced. Its
		// transition refusals name the remedy ("done is terminal — pass
		// force=true to reopen this task"), and a message that says only
		// "422" would throw that away.
		return fmt.Errorf("torque refused %s %s: %s", method, path, torqueMessage(response.StatusCode, raw))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: decoding %s: %w", ErrTorqueUnavailable, path, err)
	}
	return nil
}

// maximumResponseBytes bounds a Torque answer. A board of a few hundred cards
// is well inside it; a response that is not is a symptom, not a payload.
const maximumResponseBytes = 8 << 20

// torqueMessage extracts Torque's error text, falling back to the status line.
func torqueMessage(status int, raw []byte) string {
	var body struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &body); err == nil {
		if body.Error != "" {
			return body.Error
		}
		if body.Message != "" {
			return body.Message
		}
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return http.StatusText(status)
	}
	if len(text) > 300 {
		text = text[:300] + "…"
	}
	return text
}

func setIfPresent(query url.Values, key, value string) {
	if value != "" {
		query.Set(key, value)
	}
}
