package replyprojection

import (
	"context"
	"encoding/json"
	tether "github.com/hollis-labs/go-tether-client"
	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/subprocess"
	"github.com/hollis-labs/tangent-plugins/messaging/internal/reply"
	"github.com/hollis-labs/tangent/pkg/plugin"
	"net/http"
	"strings"
	"testing"
	"time"
)

type mappingFake struct{ binding reply.Binding }

func (f mappingFake) Binding(context.Context, string) (reply.Binding, error) { return f.binding, nil }

type storeFake struct {
	record  reply.Record
	err     error
	effects int
}

func (f *storeFake) Latest(context.Context, string) (reply.Record, error) { return f.record, f.err }
func (f *storeFake) Prepare(context.Context, reply.Prepared, *reply.Reference) (reply.Record, error) {
	f.effects++
	return reply.Record{}, reply.ErrRefused
}
func (f *storeFake) Update(context.Context, reply.Reference, reply.Record) (reply.Record, error) {
	f.effects++
	return reply.Record{}, reply.ErrRefused
}

type dispatcherFake struct{ effects int }

func (f *dispatcherFake) RoutingCapabilities(_ context.Context, id string) (tether.RoutingCapabilitiesResponse, error) {
	return tether.RoutingCapabilitiesResponse{SessionID: id, RouteSupported: true, ReplyToSender: true, Interrupt: true}, nil
}
func (f *dispatcherFake) Reply(context.Context, string, string, tether.ReplyOptions) (tether.ReplyReceipt, error) {
	f.effects++
	return tether.ReplyReceipt{}, reply.ErrRefused
}
func (f *dispatcherFake) ReplyDelivery(context.Context, string) (tether.ReplyDelivery, error) {
	f.effects++
	return tether.ReplyDelivery{}, reply.ErrRefused
}
func TestProjectionBodyFreeReadOnlyAndParticipantCapabilities(t *testing.T) {
	b := reply.Binding{ItemID: "i", SessionID: "s", Source: plugin.TurnSourceMessage{Origin: "routed"}}
	s := &storeFake{record: reply.Record{Version: 2, Prepared: reply.Prepared{Binding: b, Body: "PRIVATE-BODY", Key: "PRIVATE-KEY"}, Receipt: &tether.ReplyReceipt{ReplyID: "r"}, Delivery: &tether.ReplyDelivery{State: tether.ReplyUndeliverable, Reason: tether.ReplyReasonInterruptUnconfirmed, Detail: "PRIVATE-DETAIL"}}}
	d := &dispatcherFake{}
	h := Handler{Reader: Reader{Mappings: mappingFake{b}, Store: s, Dispatcher: d, Timeout: time.Second}}
	routes := h.Routes()
	if routes[0].Capability != plugin.CapabilityView || routes[1].Capability != plugin.CapabilityDraft {
		t.Fatal("wrong guard")
	}
	// Host participant guard owns identity; forwarded session/cookies are absent.
	result, err := h.HTTPHandle(context.Background(), subprocess.HTTPRequest{Method: http.MethodGet, Path: DeliveryPath, Query: map[string]string{"item_id": "i"}})
	if err != nil || result.Status != 200 || strings.Contains(string(result.Body), "PRIVATE-") || s.effects != 0 || d.effects != 0 {
		t.Fatalf("projection %s %v", result.Body, err)
	}
	var v View
	if json.Unmarshal(result.Body, &v) != nil || !v.Accepted || !v.RetryAllowed || v.Acknowledged {
		t.Fatalf("receipt conflated %+v", v)
	}
	s.record.Delivery.State = tether.ReplyState("future")
	s.record.Delivery.Reason = "private-reason"
	v, err = h.Reader.Read(context.Background(), "i")
	if err != nil || v.State != "unknown" || v.Reason != "unknown_reason" || v.RetryAllowed {
		t.Fatalf("unknown %+v %v", v, err)
	}
	s.err = reply.ErrNotFound
	v, err = h.Reader.Read(context.Background(), "i")
	if err != nil || v.State != "not_submitted" || v.Accepted {
		t.Fatal("invented receipt")
	}
	b.Source.Origin = "publication"
	h.Reader.Mappings = mappingFake{b}
	v, err = h.Reader.Read(context.Background(), "i")
	if err != nil || v.State != "nonreplyable" || v.InterruptSupported {
		t.Fatal("publication enabled")
	}
}
func TestRetryActionStrictBooleanAndSingleObject(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		valid bool
	}{
		{`{"item_id":"i","expected_version":1,"action_id":"a","interrupt":true}`, true},
		{`{"item_id":"i","expected_version":1,"action_id":"a"}`, true},
		{`{"item_id":"i","expected_version":1,"action_id":"a","interrupt":null}`, false},
		{`{"item_id":"i","expected_version":1,"action_id":"a","interrupt":"true"}`, false},
		{`{"item_id":"i","expected_version":1,"action_id":"a","interrupt":true,"interrupt":false}`, false},
		{`{"item_id":"i","expected_version":1,"action_id":"a","extra":1}`, false},
		{`{"item_id":"i","expected_version":1,"action_id":"a"} {}`, false},
	} {
		var target struct {
			ItemID          string `json:"item_id"`
			ExpectedVersion int64  `json:"expected_version"`
			ActionID        string `json:"action_id"`
			Interrupt       bool   `json:"interrupt"`
		}
		if decodeAction([]byte(tc.raw), &target) != tc.valid {
			t.Fatalf("decode %s", tc.raw)
		}
	}
}
