package tesseract

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	tangentplugin "github.com/hollis-labs/tangent/pkg/plugin"
)

// These tests hold CW-20260910-0054's flows end to end, against a fake
// Tesseract and a fake Tangent tool surface.
//
// The fakes are deliberately at the wire: a real HTTP server for Tesseract and
// a recorded tool-call log for Tangent. What is being checked is the sequence
// of calls this plugin makes — which is the whole of its behavior — and a fake
// that intercepted at a Go interface one layer in would let a wrong request
// body pass. That matters more here than it did for the Torque board, because
// Tesseract's HTTP door rejects unknown fields and its recall filters are keyed
// by Go field name: a body this plugin gets subtly wrong is a 400 at runtime,
// and only a fake that decodes the real bytes will show it.

// ── The fake Tangent tool surface ───────────────────────────────────────────

type toolCall struct {
	Name      string
	Arguments map[string]any
}

type fakeTools struct {
	mu      sync.Mutex
	calls   []toolCall
	answers map[string]tangentplugin.ToolResult
}

func newFakeTools() *fakeTools {
	return &fakeTools{answers: map[string]tangentplugin.ToolResult{}}
}

func (f *fakeTools) answer(name string, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[name] = tangentplugin.ToolResult{Content: json.RawMessage(body)}
}

func (f *fakeTools) CallTool(
	_ context.Context, name string, arguments any,
) (tangentplugin.ToolResult, error) {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return tangentplugin.ToolResult{}, err
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return tangentplugin.ToolResult{}, err
	}
	f.mu.Lock()
	f.calls = append(f.calls, toolCall{Name: name, Arguments: decoded})
	answer, ok := f.answers[name]
	f.mu.Unlock()
	if !ok {
		answer = tangentplugin.ToolResult{Content: json.RawMessage(`{}`)}
	}
	return answer, nil
}

func (f *fakeTools) called(name string) (toolCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.calls {
		if call.Name == name {
			return call, true
		}
	}
	return toolCall{}, false
}

func (f *fakeTools) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, call := range f.calls {
		out = append(out, call.Name)
	}
	return out
}

// fakeHost is a Tangent host: the SDK's base contract (embedded, and refusing
// everything, because this plugin calls none of it) plus the two extensions
// and the tool caller it does use.
type fakeHost struct {
	baseHostOnly
	tools  *fakeTools
	mcp    []tangentplugin.MCPTool
	routes []tangentplugin.HTTPRoute
}

func (h *fakeHost) RegisterMCPTool(tool tangentplugin.MCPTool) error {
	h.mcp = append(h.mcp, tool)
	return nil
}

func (h *fakeHost) RegisterHTTPRoute(route tangentplugin.HTTPRoute) error {
	h.routes = append(h.routes, route)
	return nil
}

func (h *fakeHost) Tools() (tangentplugin.ToolCaller, error) {
	if h.tools == nil {
		return nil, tangentplugin.ErrToolCallerUnavailable
	}
	return h.tools, nil
}

// ── The fake Tesseract ──────────────────────────────────────────────────────

type fakeTesseract struct {
	server *httptest.Server
	mu     sync.Mutex
	// revisions is what recall answers with, minus anything deprecated —
	// mirroring the real store, where a deprecated revision leaves the default
	// recall. That is the feedback a participant gets from a deprecation, so a
	// fake that kept returning it would hide the one thing the flow proves.
	revisions []Revision
	// total is what the manifest reports as results_total. Set higher than the
	// revision count to model a truncated recall.
	total       int
	deprecated  []string
	lastRecall  json.RawMessage
	refuse      map[string]string
	recallCalls int
}

func startFakeTesseract(t *testing.T, revisions []Revision) *fakeTesseract {
	t.Helper()
	fake := &fakeTesseract{revisions: revisions, refuse: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/memory/recall", func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r)
		fake.mu.Lock()
		fake.lastRecall = body
		fake.recallCalls++
		live := make([]Revision, 0, len(fake.revisions))
		for _, revision := range fake.revisions {
			if !contains(fake.deprecated, revision.RevisionID) {
				live = append(live, revision)
			}
		}
		total := fake.total
		fake.mu.Unlock()
		if total == 0 {
			total = len(live)
		}

		results := make([]map[string]any, 0, len(live))
		for _, revision := range live {
			results = append(results, map[string]any{"revision": revision})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": results,
			"manifest": map[string]any{
				"results_total":     total,
				"results_returned":  len(live),
				"bytes_returned":    len(live) * 700,
				"tokens_estimate":   len(live) * 175,
				"truncated":         total > len(live),
				"truncation_reason": truncationReasonFor(total, len(live)),
				"next_cursor":       nil,
			},
		})
	})
	mux.HandleFunc("POST /v1/memory/deprecate", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			RevisionID string `json:"revision_id"`
		}
		body, _ := readAll(r)
		_ = json.Unmarshal(body, &request)

		fake.mu.Lock()
		refusal, refused := fake.refuse[request.RevisionID]
		if !refused {
			fake.deprecated = append(fake.deprecated, request.RevisionID)
		}
		fake.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if refused {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(
				map[string]any{"code": "not_found", "message": refusal})
			return
		}
		_ = json.NewEncoder(w).Encode(
			map[string]any{"status": "deprecated", "revision_id": request.RevisionID})
	})
	mux.HandleFunc("GET /v1/memory/revisions/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		fake.mu.Lock()
		var found *Revision
		for index := range fake.revisions {
			if fake.revisions[index].RevisionID == id {
				copied := fake.revisions[index]
				// Hydration is the ONLY way a deprecated revision comes back;
				// recall drops it. The fake says so too, or the test would pass
				// against a store that never retired anything.
				if contains(fake.deprecated, id) {
					copied.Status = StatusDeprecated
				}
				found = &copied
			}
		}
		fake.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if found == nil {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(
				map[string]any{"code": "not_found", "message": "memory not found: revision_id " + id})
			return
		}
		_ = json.NewEncoder(w).Encode(found)
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func truncationReasonFor(total, returned int) string {
	if total > returned {
		return "limit"
	}
	return ""
}

func readAll(r *http.Request) (json.RawMessage, error) {
	var body json.RawMessage
	err := json.NewDecoder(r.Body).Decode(&body)
	return body, err
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (f *fakeTesseract) retired() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deprecated...)
}

func (f *fakeTesseract) recallBody(t *testing.T) map[string]any {
	t.Helper()
	f.mu.Lock()
	raw := f.lastRecall
	f.mu.Unlock()
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode recall body %q: %v", string(raw), err)
	}
	return decoded
}

// ── Fixtures ────────────────────────────────────────────────────────────────

func revisions() []Revision {
	return []Revision{
		summaryRevision("rev-1", "mem-1", "portfolio_ui_layering", "draft", 0.75),
		summaryRevision("rev-2", "mem-2", "app_plugins_are_self_contained", "reviewed", 0.95),
		summaryRevision("rev-3", "mem-3", "compiled_in_plugin_write_exception", "canonical", 0.9),
	}
}

func summaryRevision(revisionID, memoryID, key, status string, confidence float64) Revision {
	revision := Revision{
		RevisionID: revisionID, MemoryID: memoryID, Domain: "memory",
		Namespace: "user/example/memory/decisions", MemoryKey: key,
		Status: status, CreatedAt: "2026-09-10T20:00:00Z",
		Confidence: confidence, Tags: []string{"decision", "project:tangent"},
	}
	revision.Payload.Summary = "Summary of " + key + "."
	return revision
}

func loadedPlugin(t *testing.T, fake *fakeTesseract) (*Plugin, *fakeHost) {
	t.Helper()
	board := NewWithClient(NewClient(fake.server.URL, ""))
	board.namespaces = []string{"user/example/memory"}
	host := &fakeHost{tools: newFakeTools()}
	if err := board.Load(host); err != nil {
		t.Fatalf("Load: %v", err)
	}
	host.tools.answer("tangent.session_create", `{"roomID":"room-1","url":"http://x/r/room-1"}`)
	return board, host
}

// boardEnvelopeFrom pulls the presented envelope back out of the recorded
// session_advance call.
func boardEnvelopeFrom(t *testing.T, host *fakeHost) map[string]any {
	t.Helper()
	call, ok := host.tools.called("tangent.session_advance")
	if !ok {
		t.Fatalf("no board was presented; calls: %v", host.tools.names())
	}
	envelope, ok := call.Arguments["envelope"].(map[string]any)
	if !ok {
		t.Fatalf("session_advance carried no envelope: %#v", call.Arguments)
	}
	return envelope
}

func boardDataFrom(t *testing.T, host *fakeHost) BoardData {
	t.Helper()
	encoded, err := json.Marshal(boardEnvelopeFrom(t, host)["data"])
	if err != nil {
		t.Fatalf("encode board data: %v", err)
	}
	var data BoardData
	if err := json.Unmarshal(encoded, &data); err != nil {
		t.Fatalf("decode board data: %v", err)
	}
	return data
}

// ── Loading ─────────────────────────────────────────────────────────────────

func TestLoadRegistersBothToolsAndTheSyncRoute(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	_, host := loadedPlugin(t, fake)

	names := make([]string, 0, len(host.mcp))
	for _, tool := range host.mcp {
		names = append(names, tool.Name)
	}
	if len(names) != 2 || names[0] != OpenTool || names[1] != SyncTool {
		t.Errorf("registered tools = %v, want [%s %s]", names, OpenTool, SyncTool)
	}
	if len(host.routes) != 1 || host.routes[0].Path != SyncPath {
		t.Errorf("registered routes = %#v, want one at %s", host.routes, SyncPath)
	}
	// tangentplugin.CapabilityDraft, not submit: a sync is the participant applying what they
	// staged. The pluginhost refuses a capability a participant cannot hold, so
	// this asserts the intent rather than the enforcement.
	if got := string(host.routes[0].Capability); got != "draft" {
		t.Errorf("sync route capability = %q, want draft", got)
	}
}

func TestLoadRefusesAHostWithoutTangentSurfaces(t *testing.T) {
	err := New().Load(baseHostOnly{})
	if err == nil {
		t.Fatal("Load accepted a host with no Tangent registration surfaces")
	}
	if !strings.Contains(err.Error(), "RegisterMCPTool") {
		t.Errorf("Load = %v, want the missing surface named", err)
	}
}

// ── Opening ─────────────────────────────────────────────────────────────────

func TestOpenRecallsThenCreatesARoomThenPresents(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, host := loadedPlugin(t, fake)

	result, err := board.Open(context.Background(), OpenInput{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if result.RoomID != "room-1" || result.Cards != 3 {
		t.Errorf("Open = %+v, want room-1 with 3 cards", result)
	}

	want := []string{"tangent.session_create", "tangent.session_advance"}
	if got := host.tools.names(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("tool calls = %v, want %v", got, want)
	}
}

// TestOpenSendsTesseractsOwnRecallShape is the test that would have caught the
// two route names this task's brief had wrong.
//
// The filters ride in a nested object whose keys are Go FIELD names, because
// the HTTP door decodes them into a struct carrying no tags. `statuses` at the
// top level is a 400 from the real service, and so is `confidence_min` inside
// the object. Asserting the bytes is the only way to hold that.
func TestOpenSendsTesseractsOwnRecallShape(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, _ := loadedPlugin(t, fake)

	if _, err := board.Open(context.Background(), OpenInput{
		Namespaces: []string{"user/example/memory/decisions"},
		Statuses:   []string{"draft", "reviewed"},
		Tags:       []string{"decision"},
	}); err != nil {
		t.Fatalf("Open: %v", err)
	}

	body := fake.recallBody(t)
	if _, leaked := body["statuses"]; leaked {
		t.Error("recall body carries a top-level `statuses`; the HTTP door rejects unknown fields")
	}
	filters, ok := body["filters"].(map[string]any)
	if !ok {
		t.Fatalf("recall body has no filters object: %#v", body)
	}
	if _, ok := filters["Statuses"]; !ok {
		t.Errorf("filters = %#v, want the Go field name `Statuses`", filters)
	}
	if _, ok := filters["Tags"]; !ok {
		t.Errorf("filters = %#v, want the Go field name `Tags`", filters)
	}
	if body["payload_mode"] != PayloadModeSummary {
		t.Errorf("payload_mode = %v, want %q", body["payload_mode"], PayloadModeSummary)
	}
	// activation, not relevance: no query was given.
	if body["ranking"] != RankingActivation {
		t.Errorf("ranking = %v, want %q", body["ranking"], RankingActivation)
	}
}

func TestOpenWithAQuerySwitchesToRelevanceRanking(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, _ := loadedPlugin(t, fake)

	if _, err := board.Open(context.Background(), OpenInput{Query: "plugin boundary"}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := fake.recallBody(t)["ranking"]; got != RankingRelevance {
		t.Errorf("ranking = %v, want %q", got, RankingRelevance)
	}
}

func TestOpenClampsTheCardLimit(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, _ := loadedPlugin(t, fake)

	if _, err := board.Open(context.Background(), OpenInput{Limit: 5000}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := fake.recallBody(t)["limit"]; got != float64(MaximumCards) {
		t.Errorf("limit = %v, want %d", got, MaximumCards)
	}
}

// TestBoardShowsEveryLifecycleColumnEvenWhenEmpty is why an empty Deprecated
// column is correct rather than a defect: recall does not return deprecated
// records by default, and the column exists to be dropped into.
func TestBoardShowsEveryLifecycleColumnEvenWhenEmpty(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, host := loadedPlugin(t, fake)

	if _, err := board.Open(context.Background(), OpenInput{}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	data := boardDataFrom(t, host)
	if len(data.Columns) != len(LifecycleStatuses) {
		t.Fatalf("columns = %d, want %d", len(data.Columns), len(LifecycleStatuses))
	}
	for index, status := range LifecycleStatuses {
		if data.Columns[index].ID != status {
			t.Errorf("column %d = %q, want %q", index, data.Columns[index].ID, status)
		}
		if data.Columns[index].CardIDs == nil {
			t.Errorf("column %q has nil card_ids; the schema requires an array", status)
		}
	}
}

// TestAStatusOutsideTheLifecycleGetsItsOwnColumn: dropping the card would make
// the board quietly disagree with Tesseract about how much needs review.
func TestAStatusOutsideTheLifecycleGetsItsOwnColumn(t *testing.T) {
	odd := summaryRevision("rev-9", "mem-9", "something_new", "quarantined", 0.5)
	fake := startFakeTesseract(t, append(revisions(), odd))
	board, host := loadedPlugin(t, fake)

	if _, err := board.Open(context.Background(), OpenInput{}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	data := boardDataFrom(t, host)
	last := data.Columns[len(data.Columns)-1]
	if last.ID != "quarantined" {
		t.Fatalf("last column = %q, want the unknown status appended", last.ID)
	}
	if len(last.CardIDs) != 1 || last.CardIDs[0] != "rev-9" {
		t.Errorf("quarantined column = %v, want [rev-9]", last.CardIDs)
	}
	if len(data.Cards) != 4 {
		t.Errorf("cards = %d, want every revision to become one", len(data.Cards))
	}
}

// TestAnUnkeyedMemoryStillGetsATitle: memory_key is optional in Tesseract, and
// the kind's schema requires a title of at least one character. A keyless
// record is titled by its id rather than dropped or rendered blank.
func TestAnUnkeyedMemoryStillGetsATitle(t *testing.T) {
	unkeyed := summaryRevision("rev-8", "mem-8", "", "draft", 0.5)
	unkeyed.MemoryKey = ""
	fake := startFakeTesseract(t, []Revision{unkeyed})
	board, host := loadedPlugin(t, fake)

	if _, err := board.Open(context.Background(), OpenInput{}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	data := boardDataFrom(t, host)
	if len(data.Cards) != 1 {
		t.Fatalf("cards = %d, want 1", len(data.Cards))
	}
	if data.Cards[0].Title == "" {
		t.Fatal("an unkeyed memory produced an empty title; the schema requires minLength 1")
	}
	if !strings.Contains(data.Cards[0].Title, "mem-8") {
		t.Errorf("title = %q, want the memory id so the record can be found again",
			data.Cards[0].Title)
	}
}

// TestAnEmptyResultSetIsAScopeNotAnError: a filter that matches nothing is a
// legitimate board, and the scope line has to say so or it reads as a defect.
func TestAnEmptyResultSetIsAScopeNotAnError(t *testing.T) {
	fake := startFakeTesseract(t, nil)
	board, host := loadedPlugin(t, fake)

	result, err := board.Open(context.Background(), OpenInput{Statuses: []string{"draft"}})
	if err != nil {
		t.Fatalf("Open over an empty set: %v", err)
	}
	if result.Cards != 0 {
		t.Errorf("cards = %d, want 0", result.Cards)
	}
	data := boardDataFrom(t, host)
	if data.Sync == nil || data.Sync.Scope == "" {
		t.Fatal("an empty board carries no scope sentence")
	}
	if !strings.Contains(data.Sync.Scope, "0 record(s)") {
		t.Errorf("scope = %q, want it to say the set is empty", data.Sync.Scope)
	}
	if strings.Contains(data.Sync.Scope, "NOT the whole set") {
		t.Errorf("scope = %q, want an untruncated empty set not reported as cut", data.Sync.Scope)
	}
}

// TestATruncatedSetSaysSoInTheScopeLine is the CW-20260910-0043 guard: recall's
// manifest reports the cut exactly, so a cut card set must never read as the
// whole set.
func TestATruncatedSetSaysSoInTheScopeLine(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	fake.total = 1426
	board, host := loadedPlugin(t, fake)

	result, err := board.Open(context.Background(), OpenInput{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !result.Truncated || result.Matching != 1426 {
		t.Errorf("Open = %+v, want truncated with 1426 matching", result)
	}

	scope := boardDataFrom(t, host).Sync.Scope
	for _, want := range []string{"3 of 1426", "NOT the whole set", "the card limit"} {
		if !strings.Contains(scope, want) {
			t.Errorf("scope = %q, want it to contain %q", scope, want)
		}
	}
}

// TestTesseractDownIsItsOwnSentence: an operator restarting a service and an
// operator reading Tangent's logs need different messages.
func TestTesseractDownIsItsOwnSentence(t *testing.T) {
	board := NewWithClient(NewClient("http://127.0.0.1:1", ""))
	host := &fakeHost{tools: newFakeTools()}
	if err := board.Load(host); err != nil {
		t.Fatalf("Load: %v", err)
	}

	_, err := board.Open(context.Background(), OpenInput{Namespaces: []string{"user/example/memory"}})
	if err == nil {
		t.Fatal("Open succeeded against a dead Tesseract")
	}
	if !errors.Is(err, ErrTesseractUnavailable) {
		t.Errorf("Open = %v, want it to wrap ErrTesseractUnavailable", err)
	}
	// No room is created when the recall fails: a board with nothing in it is
	// worse than no board.
	if _, created := host.tools.called("tangent.session_create"); created {
		t.Error("a room was created even though Tesseract never answered")
	}
}

// TestOpenWithNoNamespaceAndNoDefaultIsRefused: with NamespacesEnv unset there
// is no default, and the board is refused before Tesseract is asked anything
// rather than aimed at a namespace nobody chose.
func TestOpenWithNoNamespaceAndNoDefaultIsRefused(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, host := loadedPlugin(t, fake)
	board.namespaces = nil

	_, err := board.Open(context.Background(), OpenInput{})
	if !errors.Is(err, ErrNoNamespaces) {
		t.Fatalf("Open = %v, want ErrNoNamespaces", err)
	}
	if _, created := host.tools.called("tangent.session_create"); created {
		t.Error("a room was created for a board with no namespace")
	}
}

// TestOpenFallsBackToTheConfiguredNamespaces: a caller naming none gets the
// plugin's configured default.
func TestOpenFallsBackToTheConfiguredNamespaces(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, _ := loadedPlugin(t, fake)
	board.namespaces = []string{"user/example/memory", "project/example/memory"}

	if _, err := board.Open(context.Background(), OpenInput{}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, _ := fake.recallBody(t)["namespaces"].([]any)
	if len(got) != 2 || got[0] != "user/example/memory" || got[1] != "project/example/memory" {
		t.Errorf("default recall namespaces = %v", got)
	}
}

func TestParseNamespaces(t *testing.T) {
	got := parseNamespaces(" user/example/memory, ,project/example/memory,")
	if len(got) != 2 || got[0] != "user/example/memory" || got[1] != "project/example/memory" {
		t.Errorf("parseNamespaces = %q", got)
	}
	if got := parseNamespaces(""); len(got) != 0 {
		t.Errorf(`parseNamespaces("") = %q, want none`, got)
	}
}
