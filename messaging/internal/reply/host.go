package reply

import (
	"context"
	"encoding/json"
	"unicode/utf8"

	"github.com/hollis-labs/tangent/pkg/plugin"
)

// ToolHost uses only the published public hostclient/ToolCaller seam. Await
// looks once; the loaded owner controls polling/backoff and session inventory.
type ToolHost struct{ Caller plugin.ToolCaller }

func (h ToolHost) Await(ctx context.Context, sessionID string) ([]ResolvedItem, error) {
	if h.Caller == nil || !validID(sessionID) {
		return nil, ErrRefused
	}
	result, err := h.Caller.CallTool(ctx, "tangent.turn_await", map[string]any{"session_id": sessionID, "wait_ms": 0})
	if err != nil {
		return nil, closedError(err)
	}
	if result.IsError || !utf8.Valid(result.Content) || len(result.Content) > 8*1024*1024 {
		return nil, ErrRefused
	}
	var response struct {
		SessionID  string         `json:"session_id"`
		WaitStatus string         `json:"wait_status"`
		Replies    []ResolvedItem `json:"replies"`
	}
	if json.Unmarshal(result.Content, &response) != nil || response.SessionID != sessionID || len(response.Replies) > 128 ||
		(response.WaitStatus != "replies" && response.WaitStatus != "timeout") || (response.WaitStatus == "timeout" && len(response.Replies) != 0) {
		return nil, ErrRefused
	}
	for _, item := range response.Replies {
		if item.SessionID != sessionID || !validID(item.ItemID) || item.Resolution == nil || !validID(item.Resolution.ID) {
			return nil, ErrRefused
		}
	}
	return response.Replies, nil
}

func (h ToolHost) Ack(ctx context.Context, itemID, resolutionID string) error {
	if h.Caller == nil || !validID(itemID) || !validID(resolutionID) {
		return ErrRefused
	}
	result, err := h.Caller.CallTool(ctx, "tangent.turn_ack", map[string]any{"item_id": itemID, "reply_id": resolutionID})
	if err != nil {
		return closedError(err)
	}
	if result.IsError || len(result.Content) > 4096 || !utf8.Valid(result.Content) {
		return ErrRefused
	}
	var response struct {
		ItemID string `json:"item_id"`
		Status string `json:"status"`
	}
	if json.Unmarshal(result.Content, &response) != nil || response.ItemID != itemID || response.Status != "acknowledged" {
		return ErrRefused
	}
	return nil
}
