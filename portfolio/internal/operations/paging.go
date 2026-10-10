package operations

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/hollis-labs/tangent-plugins/portfolio/internal/storage"
)

// pageResult pages only a complete local snapshot after domain filtering. The
// caller verifier must admit the entire requested cohort before any totals.
// It does not implement project/workstream membership or upstream paging.
func pageResult(name string, input object, state *storage.State, result any) (any, error) {
	page, requested := input["page"].(object)
	if !requested {
		return result, nil
	}
	query := clone(input).(object)
	delete(query, "page")
	// Hash only the complete admitted result cohort. Unrelated or unadmitted
	// database changes must not be observable through continuation tokens.
	versions := object{}
	if name == "search" {
		for _, value := range result.([]any) {
			row := value.(object)
			db, id := str(row["db"]), str(row["id"])
			for _, value := range state.Envelopes[db]["items"].([]any) {
				item := value.(object)
				if item["id"] == id {
					versions[id] = item["rev"]
					break
				}
			}
		}
	}
	raw, err := json.Marshal(object{"operation": name, "query": query, "cohort": result, "revisions": versions})
	if err != nil {
		return nil, failure("unavailable", "snapshot unavailable", nil)
	}
	digest := sha256.Sum256(raw)
	fingerprint := hex.EncodeToString(digest[:])
	limit, offset := int64(50), int64(0)
	if v, exists := page["limit"]; exists {
		limit, _ = integer(v)
	}
	if v, exists := page["offset"]; exists {
		offset, _ = integer(v)
	}
	if offset > 0 && str(page["snapshot"]) == "" {
		return nil, failure("bad_request", "continuation requires snapshot", nil)
	}
	if snapshot := str(page["snapshot"]); snapshot != "" && snapshot != fingerprint {
		return nil, failure("conflict", "snapshot or query changed; restart pagination", nil)
	}
	var items []any
	switch rows := result.(type) {
	case []object:
		for _, row := range rows {
			items = append(items, row)
		}
	case []any:
		items = rows
	}
	total := int64(len(items))
	start := min(offset, total)
	end := min(start+limit, total)
	selected := append([]any{}, items[start:end]...)
	var next any
	if end < total && end <= 100000 {
		next = end
	}
	return object{"items": selected, "total": total, "returned": len(selected), "has_more": end < total,
		"next_offset": next, "snapshot": fingerprint, "partial": false}, nil
}
