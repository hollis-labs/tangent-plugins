package tesseract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// This file is the plugin's Tesseract dependency, and it is the only file in
// this repository that knows Tesseract exists.
//
// That placement is a condition recorded on CW-20260910-0054, not a layout
// preference. `torque/internal/torque/torque.go` is the only file that
// knows Torque exists, and the ADR 0007 §6 amendment that widened what a
// *plugin* may do widened nothing about what Tangent core learns. A future
// reader checking "does Tangent know about Tesseract" should get one file back.
//
// Tesseract's HTTP API is the surface this uses rather than its MCP one, for
// the reason torque.go gives: a plain request/response with no session to hold
// and no second protocol stack inside a process that already runs one.
//
// # The request shapes here are the HTTP door's, and they differ from MCP's
//
// Verified against the running service on 2026-09-10 rather than read off the
// tool descriptions, because the two doors do not take the same JSON:
//
//   - Over HTTP the fields are real JSON values, not the JSON-encoded strings
//     the MCP surface takes.
//   - The recall filters ride in a nested `filters` object rather than as
//     top-level `statuses`/`tags` arguments — and that object is decoded into a
//     Go struct carrying no tags, so its keys are Go FIELD NAMES. See
//     RecallFilters below, which is why its tags look wrong and are not.
//   - The route rejects unknown top-level fields, so a name copied across from
//     the MCP shape is a 400 rather than a silently ignored key. That is a
//     feature here: it is why the shapes in this file are known to be right.
//
// **If Tesseract is down, this reports it and nothing else in Tangent
// notices.** Every call returns an error a tool result can carry rather than
// panicking, retrying forever, or caching a stale answer.

// DefaultBaseURL is where tesseract-api-service listens on this machine. It is
// Cerberus-managed (launchd job
// com.fragments-engine.cerberus.tesseract.tesseract-api-service) and answers on
// loopback with no auth header.
const DefaultBaseURL = "http://127.0.0.1:8089"

// BaseURLEnv overrides DefaultBaseURL. The plugin reads it itself rather than
// asking the host: GetConfig is deliberately unimplemented (plugin
// configuration has no owner in Tangent yet), and a plugin reading its own
// environment is userland doing userland's job.
const BaseURLEnv = "TANGENT_TESSERACT_API_URL"

// TokenEnv optionally supplies a Tesseract capability token. It is sent as a
// bearer only when set.
//
// Absent is the normal case and is not a fallback to something weaker: the
// local service runs with auth off, and sending an empty Authorization header
// would be a header a proxy or a future auth mode has to interpret. A plugin
// that has no credential says nothing rather than saying nothing loudly.
//
// Note what this does NOT establish. Tangent holds no Tesseract credential and
// issues no identity; a token here is one the operator put in the environment,
// and any write it authorizes is authorized by Tesseract's policy rather than
// by anything this host vouched for (CW-20260910-0045).
const TokenEnv = "TANGENT_TESSERACT_TOKEN" // #nosec G101 -- the NAME of an environment variable, not a credential

// requestTimeout bounds one Tesseract call. A board is opened by a person
// waiting for it, so a hung dependency has to become a visible refusal quickly
// rather than a spinner.
const requestTimeout = 10 * time.Second

// maximumResponseBytes bounds a Tesseract answer. A board's recall is capped
// well inside it (see MaximumCards); a response that is not is a symptom.
const maximumResponseBytes = 8 << 20

// ErrTesseractUnavailable reports that Tesseract could not be reached or
// answered unusably. It is distinguishable so a tool result can say "Tesseract
// is down" rather than "something failed" — the difference between an operator
// restarting a service and an operator reading Tangent's logs.
var ErrTesseractUnavailable = errors.New("tesseract: tesseract is unavailable")

// Revision is the subset of a Tesseract revision this board renders.
//
// Deliberately not every field the store returns: a field nothing displays is a
// field that would silently become part of this plugin's contract with
// Tesseract. `payload.body` is absent on purpose and is not an oversight — see
// PayloadModeSummary for the measurement that decided it.
type Revision struct {
	RevisionID string   `json:"revision_id"`
	MemoryID   string   `json:"memory_id"`
	Domain     string   `json:"domain"`
	Namespace  string   `json:"namespace"`
	MemoryKey  string   `json:"memory_key"`
	Status     string   `json:"status"`
	CreatedAt  string   `json:"created_at"`
	Confidence float64  `json:"confidence"`
	Tags       []string `json:"tags"`
	Payload    struct {
		Summary string `json:"summary"`
	} `json:"payload"`
}

// Manifest is recall's own account of what it did and did not return.
//
// It is modeled in full because reporting a cut card set honestly is a
// condition on this plugin (CW-20260910-0043 is the open bug for the Torque
// board failing to do it). Every field is always present: `truncated: false`
// means everything matched was returned, and completeness is never inferred
// from the length of the results array.
type Manifest struct {
	ResultsTotal     int    `json:"results_total"`
	ResultsReturned  int    `json:"results_returned"`
	BytesReturned    int    `json:"bytes_returned"`
	TokensEstimate   int    `json:"tokens_estimate"`
	Truncated        bool   `json:"truncated"`
	TruncationReason string `json:"truncation_reason"`
	NextCursor       string `json:"next_cursor"`
}

// RecallFilters are the facets recall narrows a candidate set by.
//
// **The JSON tags are Go field names, and that is correct.** The HTTP route
// decodes this object into `memory.RecallFilters`, a struct that carries no
// struct tags at all, so its wire keys are its Go field names. encoding/json
// matches case-insensitively but NOT across underscores, so `confidence_min`
// would not decode into `ConfidenceMin` — it would be dropped, or under this
// route's unknown-field checking, rejected. Spelling them exactly is the only
// shape that works, and writing it out here is cheaper than rediscovering it.
//
// This is the one object on Tesseract's HTTP surface that is not snake_case.
type RecallFilters struct {
	Statuses      []string `json:"Statuses,omitempty"`
	Tags          []string `json:"Tags,omitempty"`
	ConfidenceMin float64  `json:"ConfidenceMin,omitempty"`
	// Since and Until are RFC3339 bounds. They are strings here and decode
	// into *time.Time server-side; modeling them as time.Time would mean
	// this plugin parsing a bound it only ever passes through.
	Since string `json:"Since,omitempty"`
	Until string `json:"Until,omitempty"`
}

// Ranking modes. This is a closed vocabulary and it is the board's sort:
// CW-20260910-0054 settled that recall's ranking IS the sort for the MVP,
// rather than adding client-side sort to app-board.
const (
	// RankingActivation is "what is hottest" — recency x reinforcement x
	// confidence. The default when no query is given.
	RankingActivation = "activation"
	// RankingChronological is "what is newest". Carries no score.
	RankingChronological = "chronological"
	// RankingRelevance answers a query. The default when a query is given.
	RankingRelevance = "relevance"
)

// PayloadModeSummary is the only projection this board reads at, and the
// measurement that decided it is worth keeping:
//
//	summary: ~700 bytes/record   — 200 records = 136 KB
//	full:    ~5.8 KB/record      — 50 records  = 292 KB
//
// The app-board manifest sets inline_payload_limit_bytes to 262144, so `full`
// is already over the limit at fifty cards. A board of fifty records is not a
// review surface, so `full` is not a mode this plugin offers at a smaller cap
// either — it is a mode this board cannot have. The full body is what the AGENT
// hydrates by revision_id when it acts on a request, which is the same
// recall → choose → hydrate loop Tesseract's own guidance describes.
const PayloadModeSummary = "summary"

// RecallInput is one recall, as this plugin issues it.
type RecallInput struct {
	Namespaces []string      `json:"namespaces"`
	Ranking    string        `json:"ranking,omitempty"`
	Query      string        `json:"query,omitempty"`
	Filters    RecallFilters `json:"filters,omitempty"`
	Limit      int           `json:"limit,omitempty"`
	// PayloadMode is always PayloadModeSummary. It is a field rather than a
	// constant in the request builder so the board's own recall and a test's
	// recall cannot differ on it.
	PayloadMode string `json:"payload_mode,omitempty"`
}

// RecallResult is what a recall returned, and what it did not.
type RecallResult struct {
	Revisions []Revision
	Manifest  Manifest
}

// Client is a Tesseract HTTP client.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClient builds a client against baseURL and token, falling back to the
// local default for the URL. An empty token means no Authorization header.
func NewClient(baseURL, token string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: requestTimeout},
	}
}

// BaseURL is where this client points. Reported in the board's `source` block
// so a reader can tell which Tesseract they are looking at.
func (c *Client) BaseURL() string { return c.baseURL }

// Recall returns the revisions matching input, and recall's own manifest.
//
// The manifest is returned alongside rather than folded into the slice length
// because they answer different questions: the slice is what arrived, and the
// manifest is whether that was everything.
func (c *Client) Recall(ctx context.Context, input RecallInput) (RecallResult, error) {
	if input.PayloadMode == "" {
		input.PayloadMode = PayloadModeSummary
	}
	var response struct {
		Results []struct {
			Revision Revision `json:"revision"`
		} `json:"results"`
		Manifest Manifest `json:"manifest"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/memory/recall", input, &response); err != nil {
		return RecallResult{}, err
	}
	revisions := make([]Revision, 0, len(response.Results))
	for _, result := range response.Results {
		revisions = append(revisions, result.Revision)
	}
	return RecallResult{Revisions: revisions, Manifest: response.Manifest}, nil
}

// Deprecate retires one revision.
//
// It is the ONLY write this plugin makes to Tesseract, and that is the whole
// design rather than a stage of it. A memory revision is immutable except for
// this one transition: `internal/memory/types.go` says "the only field that may
// be mutated after write is Status, and only via the deprecation code path",
// and the memory domain exposes no status route to promote one. So
// draft → reviewed → canonical is not a field this plugin can flip — it is a
// new revision with `supersedes`, carrying the whole payload forward, which is
// authoring. Chrispian's call on 2026-09-10: the agent does that write, and
// this plugin hands it back as a request. See sync.go.
//
// Deprecation is idempotent in the store: deprecating an already-deprecated
// revision is a no-op rather than an error.
func (c *Client) Deprecate(ctx context.Context, revisionID string) error {
	if revisionID == "" {
		return fmt.Errorf("tesseract: deprecate needs a revision_id")
	}
	return c.do(ctx, http.MethodPost, "/v1/memory/deprecate",
		map[string]any{"revision_id": revisionID}, nil)
}

// GetRevision hydrates one revision by id, including a deprecated one.
//
// Recall does not return deprecated records, which is right for a review board
// and wrong for the one case that needs it: a record the participant retired
// AND asked to have rewritten. That request has to stay visible until the
// replacement exists, and a request with no card is a request the participant
// cannot see — which is how the first one was lost.
//
// The route is a prefix route (`/v1/memory/revisions/{id}`), not a query.
func (c *Client) GetRevision(ctx context.Context, revisionID string) (Revision, error) {
	if revisionID == "" {
		return Revision{}, fmt.Errorf("tesseract: get revision needs a revision_id")
	}
	var revision Revision
	if err := c.do(ctx, http.MethodGet,
		"/v1/memory/revisions/"+url.PathEscape(revisionID), nil, &revision); err != nil {
		return Revision{}, err
	}
	return revision, nil
}

// Health reports whether Tesseract is answering at all.
//
// /v1/health/readiness is the public route and needs no credential, so this
// distinguishes "the service is down" from "this caller is not authorized" —
// which a probe against an authenticated route could not.
func (c *Client) Health(ctx context.Context) error {
	var ignored json.RawMessage
	return c.do(ctx, http.MethodGet, "/v1/health/readiness", nil, &ignored)
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("tesseract: encode request: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}
	// #nosec G704 -- the URL is `c.baseURL` (operator configuration, from
	// TANGENT_TESSERACT_API_URL or the loopback default) joined to a `path`
	// literal from this file. gosec's taint analysis flags it because the base
	// reaches here from os.Getenv, which is true and is the point: pointing at
	// a named Tesseract is what the environment override is for, exactly as
	// torque's TANGENT_TORQUE_API_URL is. No request-derived or
	// participant-supplied value reaches this call — every caller-supplied
	// value in this package rides in the JSON body, never in the URL.
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, payload)
	if err != nil {
		return fmt.Errorf("tesseract: build request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}

	response, err := c.http.Do(request) // #nosec G704 -- see the request above
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrTesseractUnavailable, c.baseURL, err)
	}
	defer func() { _ = response.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseBytes))
	if err != nil {
		return fmt.Errorf("%w: reading %s: %w", ErrTesseractUnavailable, path, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Tesseract's own message is carried through rather than replaced. Its
		// validation errors name the offending field and even list the accepted
		// ones, and a message that said only "400" would throw that away.
		return fmt.Errorf("tesseract refused %s %s: %s",
			method, path, tesseractMessage(response.StatusCode, raw))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: decoding %s: %w", ErrTesseractUnavailable, path, err)
	}
	return nil
}

// tesseractMessage extracts Tesseract's error text, falling back to the status
// line. Its errors are {code, message, details}.
func tesseractMessage(status int, raw []byte) string {
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &body); err == nil && body.Message != "" {
		if body.Code != "" {
			return body.Code + ": " + body.Message
		}
		return body.Message
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
