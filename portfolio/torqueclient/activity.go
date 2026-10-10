package torqueclient

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"time"
)

// Activity contains only proven complete creation-time metadata. Any paging,
// admission or validation failure returns an empty result and an error; callers
// must treat every doing task as active when that signal is unavailable.
type Activity struct {
	LatestByID map[string]time.Time
	Complete   bool
}

// LatestCommentActivity is a finite metadata reader, not a registry operation.
// The 200-ID/5-page/1000-row caps are client policy, not server cardinality caps.
func (c *Client) LatestCommentActivity(ctx context.Context, proof any, ids []string) (Activity, error) {
	return c.activity(ctx, proof, ids, nil)
}

func (c *Client) activity(ctx context.Context, proof any, ids []string, parent ParentGuard) (Activity, error) {
	if len(ids) > 200 {
		return Activity{}, Invalid
	}
	values := make([]any, len(ids))
	for i, id := range ids {
		values[i] = id
	}
	validated, err := identifiers(values, 200)
	if err != nil {
		return Activity{}, err
	}
	o, cancel, err := c.begin(ctx, proof, "torque_comment_activity")
	if err != nil {
		return Activity{}, err
	}
	defer cancel()
	o.parent = parent
	encodedIDs, _ := json.Marshal(validated)
	q := url.Values{"entity_type": {"task"}, "entity_ids": {string(encodedIDs)}, "limit": {"200"}, "sort_by": {"created_at"}, "sort_dir": {"desc"}}
	requested := []Resource{}
	for _, id := range validated {
		requested = append(requested, Resource{Kind: "task_comment", ID: id})
	}
	r := Request{Operation: o.name, Path: "/comments", Query: q.Encode(), Phase: "query", Resources: requested}
	if _, err = o.check(r); err != nil {
		return Activity{}, err
	}
	// Metadata activity is intentionally uncached. Every request proves current
	// complete activity independently; no comment content is retained.
	latest := map[string]time.Time{}
	resources := requested
	if len(validated) > 0 {
		v, got, commentsErr := o.comments("/comments", q, validated, false)
		if commentsErr != nil {
			return Activity{}, commentsErr
		}
		for id, t := range v.(map[string]time.Time) {
			latest[id] = t
		}
		resources = append(resources, got...)
	}
	r.Phase = "disclosure"
	r.Resources = resources
	if _, err = o.check(r); err != nil {
		return Activity{}, err
	}
	return Activity{LatestByID: latest, Complete: true}, nil
}

func (o *operation) comments(path string, q url.Values, ids []string, full bool) (any, []Resource, error) {
	allowed := map[string]bool{}
	requested := []Resource{}
	for _, id := range ids {
		allowed[id] = true
		requested = append(requested, Resource{Kind: "task_comment", ID: id})
	}
	latest := map[string]time.Time{}
	out := []any{}
	resources := []Resource{}
	cursors := map[string]bool{}
	seen := map[int64]bool{}
	var previous time.Time
	var previousID int64
	var total any
	for range o.client.maxPages {
		var pageResources []Resource
		v, err := o.get(path, q, requested, func(raw any) (any, []Resource, error) {
			p, err := parsePage(raw, 200, q.Get("sort_dir"))
			if err != nil {
				return nil, nil, err
			}
			if t := p.meta["total"]; t != nil {
				if total != nil && total != t {
					return nil, nil, Malformed
				}
				total = t
			}
			for _, raw := range p.items {
				in, ok := raw.(object)
				if !ok || in["entity_type"] != "task" || !allowed[text(in["entity_id"])] {
					return nil, nil, Malformed
				}
				n, ok := in["id"].(json.Number)
				if !ok {
					return nil, nil, Malformed
				}
				id, err := n.Int64()
				if err != nil || id <= 0 || seen[id] {
					return nil, nil, Malformed
				}
				created, err := time.Parse(time.RFC3339Nano, text(in["created_at"]))
				if err != nil || created.IsZero() {
					return nil, nil, Malformed
				}
				if _, err := time.Parse(time.RFC3339Nano, text(in["updated_at"])); err != nil {
					return nil, nil, Malformed
				}
				if !previous.IsZero() && ((!full && created.After(previous) || full && created.Before(previous)) || created.Equal(previous) && id <= previousID) {
					return nil, nil, Malformed
				}
				previous = created
				previousID = id
				seen[id] = true
				entity := text(in["entity_id"])
				if created.After(latest[entity]) {
					latest[entity] = created
				}
				pageResources = append(pageResources, Resource{Kind: "task_comment", ID: entity, CommentID: strconv.FormatInt(id, 10)})
				if full {
					if _, ok := in["author"].(string); !ok {
						return nil, nil, Malformed
					}
					if _, ok := in["content"].(string); !ok {
						return nil, nil, Malformed
					}
					out = append(out, pick(in, "id", "author", "content", "created_at", "updated_at"))
				}
			}
			return object{"meta": p.meta}, pageResources, nil
		})
		if err != nil {
			return nil, nil, err
		}
		resources = append(resources, pageResources...)
		if full && int64(len(encoded(out))) > o.client.maxBody {
			return nil, nil, TooLarge
		}
		meta := v.(object)["meta"].(object)
		if meta["has_more"] == false {
			if full {
				return out, resources, nil
			}
			return latest, resources, nil
		}
		cursor := text(meta["next_cursor"])
		if cursors[cursor] {
			return nil, nil, Malformed
		}
		cursors[cursor] = true
		q.Set("cursor", cursor)
	}
	return nil, nil, Incomplete
}
