package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tether "github.com/hollis-labs/go-tether-client"
	pluginhost "github.com/hollis-labs/libs/plugin-mcp/plugin-host"
	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/subprocess"
	"github.com/hollis-labs/tangent-plugins/messaging/internal/messaging"
	"github.com/hollis-labs/tangent-plugins/messaging/internal/replyprojection"
)

// This proxy forwards to the real host. Only the first committed enqueue
// receipt is withheld: the child crashes with immutable preparation retained
// before its local sink-receipt/cursor transaction can commit.
func heldReceiptProxy(t *testing.T, h *host, heldTool string) (*httptest.Server, <-chan struct{}, *[][]byte) {
	t.Helper()
	committed := make(chan struct{})
	var first atomic.Bool
	var mu sync.Mutex
	requests := &[][]byte{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, e := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		if e != nil {
			t.Error(e)
			return
		}
		var call struct {
			Method string
			Params struct {
				Name      string
				Arguments json.RawMessage
			}
		}
		_ = json.Unmarshal(raw, &call)
		enqueue := call.Method == "tools/call" && call.Params.Name == "tangent.turns_enqueue"
		if enqueue {
			mu.Lock()
			*requests = append(*requests, bytes.Clone(call.Params.Arguments))
			mu.Unlock()
		}
		request, e := http.NewRequestWithContext(r.Context(), r.Method, h.url+"/mcp", bytes.NewReader(raw)) // #nosec G704 -- destination is only this test's kernel-assigned loopback host; request cannot select a destination.
		if e != nil {
			t.Error(e)
			return
		}
		request.Header = r.Header.Clone()
		request.Header.Set("Origin", h.url)
		response, e := http.DefaultClient.Do(request) // #nosec G704 -- forwarding only to the private loopback host above, with a fixed MCP path.
		if e != nil {
			return
		}
		defer func() { _ = response.Body.Close() }()
		data, e := io.ReadAll(io.LimitReader(response.Body, 8<<20))
		if e != nil {
			t.Error(e)
			return
		}
		if call.Method == "tools/call" && call.Params.Name == heldTool && !first.Swap(true) {
			if response.StatusCode != 200 {
				t.Error("host did not earn receipt", response.StatusCode, string(data))
			}
			close(committed)
			<-r.Context().Done()
			return
		}
		for key, values := range response.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(data)
	}))
	t.Cleanup(server.Close)
	// Access requests only after both child processes have joined their callers.
	return server, committed, requests
}
func canonical(t *testing.T, raw []byte) []byte {
	t.Helper()
	var v any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if e := decoder.Decode(&v); e != nil {
		t.Fatal(e)
	}
	out, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return out
}

func TestStage1CrashReplayPreservesSinkItemAndPreparedBytes(t *testing.T) {
	h := startHost(t)
	u := newUpstream(t, []tether.ChannelMessage{publication(1, "ordinary", "Exact 世界 original", "")})
	proxy, committed, requests := heldReceiptProxy(t, h, "tangent.turns_enqueue")
	data := privateDir(t)
	child := startPlugin(t, u, proxy.URL+"/mcp", data, 1)
	select {
	case <-committed:
	case <-time.After(8 * time.Second):
		t.Fatal("real host commit not reached", child.Diagnostics())
	}
	first := h.items(t)["ordinary"]
	if first.ItemID == "" || first.Content != "Exact 世界 original" || first.Summary != "Fixture summary" || first.Replyable || first.SessionID != "" || first.TurnID != "" {
		t.Fatal("ordinary public host item", first)
	}
	if e := child.Kill(); e != nil {
		t.Fatal(e)
	}
	select {
	case <-child.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("test-owned crash not reaped")
	}
	source := messaging.Source{EndpointRef: "stage1-fixture", Channel: channel}
	ledger, e := messaging.OpenLedger(context.Background(), data)
	if e != nil {
		t.Fatal(e)
	}
	saved, e := ledger.Publication(context.Background(), source, "ordinary")
	if e != nil || saved.Settled || len(saved.Prepared) == 0 {
		t.Fatal("crash lost pending preparation", saved, e)
	}
	before := bytes.Clone(saved.Prepared)
	if cursor, found, cursorErr := ledger.Cursor(context.Background(), source); cursorErr != nil || found || cursor != 0 {
		t.Fatal("receipt-less cursor advanced", cursor, found, cursorErr)
	}
	if e = ledger.Close(); e != nil {
		t.Fatal(e)
	}
	replacement := startPlugin(t, u, proxy.URL+"/mcp", data, 2)
	await(t, func() bool { u.mu.Lock(); defer u.mu.Unlock(); return u.subscribed > 0 }, "replayed receipt settled before subscription")
	live := publication(2, "live", "Live synthetic publication", "")
	u.mu.Lock()
	u.messages = append(u.messages, live)
	u.mu.Unlock()
	u.live <- live
	await(t, func() bool { u.mu.Lock(); defer u.mu.Unlock(); return u.subscribed >= 2 }, "live SSE publication settled before cursor-backed reconnect")
	stopPlugin(t, replacement)
	after := h.items(t)["ordinary"]
	if after.ItemID != first.ItemID {
		t.Fatal("sink duplicate on replay", first, after)
	}
	ledger, e = messaging.OpenLedger(context.Background(), data)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = ledger.Close() }()
	saved, e = ledger.Publication(context.Background(), source, "ordinary")
	if e != nil || !saved.Settled || saved.ItemID != first.ItemID || !bytes.Equal(before, saved.Prepared) {
		t.Fatal("immutable local mapping", saved, e)
	}
	if cursor, found, cursorErr := ledger.Cursor(context.Background(), source); cursorErr != nil || !found || cursor != 2 {
		t.Fatal("cursor not receipt-backed", cursor, found, cursorErr)
	}
	if len(*requests) != 3 || !bytes.Equal(canonical(t, (*requests)[0]), canonical(t, (*requests)[1])) || !bytes.Equal(canonical(t, before), canonical(t, (*requests)[1])) {
		t.Fatal("regenerated enqueue request")
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.ai["Exact 世界 original"] != 1 || u.ai["Live synthetic publication"] != 1 || u.subscribed != u.joined {
		t.Fatal("stage rerun or unjoined reader", u.ai, u.subscribed, u.joined)
	}
	t.Log("real sink commit -> plugin crash -> same prepared request/item, one cached stage, earned cursor, natural replacement reap")
}

func TestStage1IdentityPurgeFailOpenAndAdmissionPending(t *testing.T) {
	for _, invalid := range []string{"malformed", "oversized"} {
		t.Run(invalid, func(t *testing.T) {
			rows := []tether.ChannelMessage{publication(1, "ordinary", "ordinary", ""), publication(2, "output-a", "same original", "question"), publication(3, "output-b", "same original", "final"), publication(4, "purged", "discarded", ""), publication(5, "refusal", "refusal", ""), publication(6, "timeout", "timeout", ""), publication(7, "tool", "tool", ""), publication(8, "blocked", "invalid", ""), publication(9, "later", "must stay pending", "")}
			purgedAt := time.Now().UTC()
			rows[3].Purged = true
			rows[3].PurgedAt = &purgedAt
			rows[3].Payload = nil
			rows[3].Metadata = nil
			if invalid == "malformed" {
				rows[7].Payload = json.RawMessage(`{"text":null}`)
			} else {
				raw, _ := json.Marshal(strings.Repeat("x", 65537))
				rows[7].Payload = raw
			}
			h := startHost(t)
			u := newUpstream(t, rows)
			data := privateDir(t)
			child := startPlugin(t, u, h.url+"/mcp", data, 1)
			await(t, func() bool {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				health, e := child.Client().Health(ctx)
				return e == nil && strings.Contains(health.Message, "admission_refused")
			}, "invalid publication refused after prior settlements")
			// Health changes before its asynchronous refusal transaction. Wait
			// for that owned database receipt rather than canceling the write
			// merely because a status string has become visible.
			refusalURL := url.URL{Scheme: "file", Path: filepath.Join(data, "messaging.sqlite3"), RawQuery: "mode=ro"}
			probeDB, openErr := sql.Open("sqlite", refusalURL.String())
			if openErr != nil {
				t.Fatal(openErr)
			}
			await(t, func() bool {
				var seq int64
				return probeDB.QueryRow("SELECT sequence FROM admission_refusals WHERE message_id=?", "blocked").Scan(&seq) == nil && seq == 8
			}, "actual bounded refusal transaction persisted")
			if closeErr := probeDB.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			stopPlugin(t, child)
			items := h.items(t)
			if items["ordinary"].Replyable || items["ordinary"].Kind != "checkpoint" {
				t.Fatal("ordinary acquired reply authority")
			}
			a, b := items["output-a"], items["output-b"]
			if a.ItemID == "" || b.ItemID == "" || a.ItemID == b.ItemID || a.TurnID != b.TurnID || a.Source.OutputID == b.Source.OutputID || a.Content != "same original" || b.Content != "same original" {
				t.Fatal("outputs collapsed by body/turn", a, b)
			}
			if items["purged"].ItemID != "" || items["blocked"].ItemID != "" || items["later"].ItemID != "" {
				t.Fatal("purge/refusal/later emitted")
			}
			for id, want := range map[string]string{"refusal": "stage_refused", "timeout": "stage_timeout", "tool": "stage_error"} {
				item := items[id]
				if item.Content != id || item.Summary != "" || len(item.Trace) != 1 || item.Trace[0].FailureCode != want {
					t.Fatal("fail-open original/marker", id, item)
				}
			}
			ledger, e := messaging.OpenLedger(context.Background(), data)
			if e != nil {
				t.Fatal(e)
			}
			defer func() { _ = ledger.Close() }()
			source := messaging.Source{EndpointRef: "stage1-fixture", Channel: channel}
			if cursor, found, cursorErr := ledger.Cursor(context.Background(), source); cursorErr != nil || !found || cursor != 7 {
				t.Fatal("purge/blocked cursor", cursor, found, cursorErr)
			}
			purged, e := ledger.Publication(context.Background(), source, "purged")
			if e != nil || !purged.Purged || !purged.Settled || purged.ItemID != "" || len(purged.Prepared) != 0 {
				t.Fatal("purge recreated content", purged, e)
			}
			_, e = ledger.Publication(context.Background(), source, "blocked")
			if !errors.Is(e, sql.ErrNoRows) {
				t.Fatal("inadmissible body was admitted", e)
			}
			// Read only this stopped, private fixture database. Refusals keep
			// identity/digest, not rejected body bytes in the admitted ledger.
			dbURL := url.URL{Scheme: "file", Path: filepath.Join(data, "messaging.sqlite3"), RawQuery: "mode=ro"}
			privateDB, e := sql.Open("sqlite", dbURL.String())
			if e != nil {
				t.Fatal(e)
			}
			defer func() { _ = privateDB.Close() }()
			sourceRaw, _ := json.Marshal(source)
			var seq, payloadBytes int64
			var digest string
			e = privateDB.QueryRow("SELECT sequence,payload_digest,payload_bytes FROM admission_refusals WHERE source=? AND message_id=?", string(sourceRaw), "blocked").Scan(&seq, &digest, &payloadBytes)
			expected := sha256.Sum256(rows[7].Payload)
			if e != nil || seq != 8 || digest != hex.EncodeToString(expected[:]) || payloadBytes != int64(len(rows[7].Payload)) {
				t.Fatal("bounded refusal not retained", seq, digest, payloadBytes, e)
			}
			u.mu.Lock()
			defer u.mu.Unlock()
			if u.ai["same original"] != 2 || u.ai["discarded"] != 0 || u.ai["invalid"] != 0 || u.ai["must stay pending"] != 0 {
				t.Fatal("wrong billable admission/dedupe", u.ai)
			}
			t.Log("actual public sink: multi-output identity; ordinary nonreplyable; purge; stage refusal/timeout/tool rejection fail-open; invalid source pending")
		})
	}
}

func delivery(t *testing.T, p *pluginhost.Process, id string) replyprojection.View {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	response, e := p.Client().HTTPHandle(ctx, subprocess.HTTPRequest{Method: http.MethodGet, Path: replyprojection.DeliveryPath, Query: map[string]string{"item_id": id}})
	if e != nil || response.Status != 200 {
		t.Fatal("delivery callback", response, e)
	}
	var view replyprojection.View
	if e = json.Unmarshal(response.Body, &view); e != nil {
		t.Fatal(e)
	}
	return view
}
func TestStage1ExplicitReplyAcceptanceDeliveryAckAndInterruptRefusal(t *testing.T) {
	h := startHost(t)
	u := newUpstream(t, []tether.ChannelMessage{publication(1, "ordinary", "ordinary", ""), publication(2, "answer", "question", "question"), publication(3, "interrupt", "interrupt question", "question")})
	data := privateDir(t)
	child := startPlugin(t, u, h.url+"/mcp", data, 1)
	await(t, func() bool { u.mu.Lock(); defer u.mu.Unlock(); return u.subscribed > 0 }, "all publication mappings settled")
	items := h.items(t)
	ordinary := items["ordinary"]
	if view := delivery(t, child, ordinary.ItemID); view.State != "nonreplyable" || view.ReplySupported {
		t.Fatal("ordinary projection borrowed sender authority", view)
	}
	if status := h.request(t, http.MethodPost, "/api/turns/items/"+ordinary.ItemID+"/reply", map[string]any{"expected_revision": ordinary.Revision, "action": "respond", "response_text": "Operator fixture answer"}, nil); status < 400 {
		t.Fatal("ordinary accepted resolution", status)
	}
	for _, id := range []string{"answer", "interrupt"} {
		row := items[id]
		status := h.request(t, http.MethodPost, "/api/turns/items/"+row.ItemID+"/reply", map[string]any{"expected_revision": row.Revision, "action": "respond", "response_text": "Operator fixture answer", "interrupt": id == "interrupt"}, nil)
		if status != 200 {
			t.Fatal("actual operator resolution", id, status)
		}
	}
	await(t, func() bool { return delivery(t, child, items["answer"].ItemID).State == "queued" }, "real reply accepted but not delivered")
	queued := delivery(t, child, items["answer"].ItemID)
	if !queued.Accepted || queued.Acknowledged || h.inspect(t, items["answer"].ItemID).DeliveryState == "acknowledged" {
		t.Fatal("acceptance falsely acknowledged", queued)
	}
	await(t, func() bool {
		return delivery(t, child, items["interrupt"].ItemID).FailureCode == tether.CodeInterruptUnsupported
	}, "capability-gated interrupt refused")
	refused := delivery(t, child, items["interrupt"].ItemID)
	if refused.Accepted || refused.Acknowledged || !refused.InterruptRequested {
		t.Fatal("interrupt refusal lost no-effect distinction", refused)
	}
	u.mu.Lock()
	u.delivered = true
	u.mu.Unlock()
	await(t, func() bool { return delivery(t, child, items["answer"].ItemID).Acknowledged }, "delivered receipt persisted then host acknowledged")
	delivered := delivery(t, child, items["answer"].ItemID)
	if delivered.State != "delivered" || !delivered.Accepted || delivered.DeliveredToSessionID != "fixture-session" {
		t.Fatal("delivery attribution", delivered)
	}
	stopPlugin(t, child)
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.sends["answer"] != 1 || u.sends["interrupt"] != 0 || u.sends["ordinary"] != 0 || u.subscribed != u.joined {
		t.Fatal("repeat send or forbidden effects/reader leak", u.sends, u.subscribed, u.joined)
	}
	ledger, e := messaging.OpenLedger(context.Background(), data)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = ledger.Close() }()
	store, e := messaging.NewReplyStore(context.Background(), ledger, messaging.Source{EndpointRef: "stage1-fixture", Channel: channel})
	if e != nil {
		t.Fatal(e)
	}
	saved, e := store.Latest(context.Background(), items["answer"].ItemID)
	if e != nil || !saved.Acknowledged || saved.Receipt == nil || saved.Delivery == nil || saved.Delivery.State != tether.ReplyDelivered {
		t.Fatal("reply durable sequencing", saved, e)
	}
	failed, e := store.Latest(context.Background(), items["interrupt"].ItemID)
	if e != nil || failed.Acknowledged || failed.Receipt != nil || failed.FailureCode != tether.CodeInterruptUnsupported {
		t.Fatal("refused reply not retained", failed, e)
	}
	t.Log("actual operator resolution, one dedicated reply; queued != delivered/acknowledged; synthetic delivery then ack; unsupported interrupt no send")
}

func TestStage1AckCrashResumesSavedDeliveryWithoutAnotherSend(t *testing.T) {
	h := startHost(t)
	u := newUpstream(t, []tether.ChannelMessage{publication(1, "ack-boundary", "question", "question")})
	u.delivered = true
	proxy, committed, _ := heldReceiptProxy(t, h, "tangent.turn_ack")
	data := privateDir(t)
	child := startPlugin(t, u, proxy.URL+"/mcp", data, 1)
	await(t, func() bool { u.mu.Lock(); defer u.mu.Unlock(); return u.subscribed > 0 }, "publication settlement before operator resolution")
	row := h.items(t)["ack-boundary"]
	if status := h.request(t, http.MethodPost, "/api/turns/items/"+row.ItemID+"/reply", map[string]any{"expected_revision": row.Revision, "action": "respond", "response_text": "Operator fixture answer"}, nil); status != 200 {
		t.Fatal("operator resolution", status)
	}
	select {
	case <-committed:
	case <-time.After(8 * time.Second):
		t.Fatal("actual host ack not committed", child.Diagnostics())
	}
	if h.inspect(t, row.ItemID).DeliveryState != "acknowledged" {
		t.Fatal("host receipt did not acknowledge")
	}
	if e := child.Kill(); e != nil {
		t.Fatal(e)
	}
	select {
	case <-child.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("crashed child not reaped")
	}
	ledger, e := messaging.OpenLedger(context.Background(), data)
	if e != nil {
		t.Fatal(e)
	}
	source := messaging.Source{EndpointRef: "stage1-fixture", Channel: channel}
	store, e := messaging.NewReplyStore(context.Background(), ledger, source)
	if e != nil {
		t.Fatal(e)
	}
	before, e := store.Latest(context.Background(), row.ItemID)
	if e != nil || before.Acknowledged || before.Delivery == nil || before.Delivery.State != tether.ReplyDelivered || before.Receipt == nil {
		t.Fatal("delivery-before-ack custody", before, e)
	}
	if e = ledger.Close(); e != nil {
		t.Fatal(e)
	}
	u.mu.Lock()
	readsBefore := u.deliveryReads
	u.mu.Unlock()
	replacement := startPlugin(t, u, proxy.URL+"/mcp", data, 2)
	await(t, func() bool { return delivery(t, replacement, row.ItemID).Acknowledged }, "saved delivery acknowledged without Await item")
	stopPlugin(t, replacement)
	ledger, e = messaging.OpenLedger(context.Background(), data)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = ledger.Close() }()
	store, e = messaging.NewReplyStore(context.Background(), ledger, source)
	if e != nil {
		t.Fatal(e)
	}
	after, e := store.Latest(context.Background(), row.ItemID)
	if e != nil || !after.Acknowledged || after.Prepared.Key != before.Prepared.Key || after.Prepared.Resolution != before.Prepared.Resolution || after.Receipt.ReplyID != before.Receipt.ReplyID {
		t.Fatal("resume mutated original action", after, e)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.sends["ack-boundary"] != 1 || u.deliveryReads != readsBefore {
		t.Fatal("saved delivery spent another send/poll", u.sends, u.deliveryReads, readsBefore)
	}
	t.Log("real host ack committed before plugin crash; saved delivery resumed after host Await omitted item, no second send/poll")
}
