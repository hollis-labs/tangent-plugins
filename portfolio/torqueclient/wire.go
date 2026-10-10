package torqueclient

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

type object = map[string]any

func pick(in object, keys ...string) object {
	out := object{}
	for _, k := range keys {
		out[k] = in[k]
	}
	return out
}

func compactTask(raw any, full bool) (object, Resource, error) {
	in, ok := raw.(object)
	if !ok || !idPattern.MatchString(text(in["id"])) {
		return nil, Resource{}, Malformed
	}
	for _, k := range []string{"title", "status", "project_id", "epic_id", "sprint_id"} {
		if v := in[k]; v != nil {
			if _, ok := v.(string); !ok {
				return nil, Resource{}, Malformed
			}
		}
	}
	if in["priority"] != nil {
		if _, ok := integer(in["priority"]); !ok {
			return nil, Resource{}, Malformed
		}
	}
	for _, k := range []string{"updated_at", "created_at"} {
		if v := in[k]; v != nil {
			if _, err := time.Parse(time.RFC3339Nano, text(v)); err != nil {
				return nil, Resource{}, Malformed
			}
		}
	}
	out := pick(in, "id", "title", "status", "priority", "project_id", "epic_id", "sprint_id", "updated_at")
	tags := []any{}
	labels := []string{}
	if raw := in["tags"]; raw != nil {
		a, ok := raw.([]any)
		if !ok || len(a) > 1000 {
			return nil, Resource{}, Malformed
		}
		for _, v := range a {
			s, ok := v.(string)
			if !ok {
				m, ok := v.(object)
				if !ok {
					return nil, Resource{}, Malformed
				}
				s = text(m["slug"])
				if s == "" {
					s = text(m["name"])
				}
			}
			if s != "" {
				labels = append(labels, s)
				tags = append(tags, s)
			}
		}
	}
	out["tags"] = tags
	if full {
		if in["description"] != nil {
			if _, ok := in["description"].(string); !ok {
				return nil, Resource{}, Malformed
			}
		}
		out["description"] = in["description"]
		if out["description"] == nil {
			out["description"] = ""
		}
		out["created_at"] = in["created_at"]
	}
	r := Resource{Kind: "task", ID: text(out["id"]), ProjectID: text(out["project_id"]), EpicID: text(out["epic_id"]), SprintID: text(out["sprint_id"]), Tags: labels, Status: text(out["status"])}
	if n, ok := integer(out["priority"]); ok {
		r.Priority = &n
	}
	return out, r, nil
}

func matches(r Resource, q url.Values) bool {
	for k, v := range map[string]string{"project_id": r.ProjectID, "epic_id": r.EpicID, "sprint_id": r.SprintID, "status": r.Status} {
		if wanted := q.Get(k); wanted != "" && !contains(strings.Split(wanted, ","), v) {
			return false
		}
	}
	if q.Get("priority") != "" && (r.Priority == nil || strconv.Itoa(*r.Priority) != q.Get("priority")) {
		return false
	}
	for _, k := range []string{"tags", "tag"} {
		if s := q.Get(k); s != "" {
			for _, tag := range strings.Split(s, ",") {
				if !contains(r.Tags, tag) {
					return false
				}
			}
		}
	}
	if s := q.Get("tags_any"); s != "" {
		found := false
		for _, tag := range strings.Split(s, ",") {
			found = found || contains(r.Tags, tag)
		}
		if !found {
			return false
		}
	}
	if s := q.Get("tags_none"); s != "" {
		for _, tag := range strings.Split(s, ",") {
			if contains(r.Tags, tag) {
				return false
			}
		}
	}
	return true
}

type page struct {
	items  []any
	meta   object
	more   bool
	cursor string
}

// Completeness comes exclusively from a valid has_more/next_cursor envelope,
// never from total or page fullness. Sorted comments require explicit sort echo.
func parsePage(raw any, limit int, sortDir string) (page, error) {
	in, ok := raw.(object)
	if !ok {
		return page{}, Malformed
	}
	items, ok := in["items"].([]any)
	if !ok || len(items) > limit {
		return page{}, Malformed
	}
	m, ok := in["meta"].(object)
	if !ok {
		return page{}, Malformed
	}
	returned, ok := integer(m["returned"])
	if !ok || returned != len(items) {
		return page{}, Malformed
	}
	n, ok := integer(m["limit"])
	if !ok || n != limit {
		return page{}, Malformed
	}
	more, ok := m["has_more"].(bool)
	if !ok {
		return page{}, Malformed
	}
	cursor, has := m["next_cursor"]
	if !has {
		return page{}, Malformed
	}
	next := ""
	if cursor != nil {
		var ok bool
		next, ok = cursor.(string)
		if !ok || !cursorPattern.MatchString(next) {
			return page{}, Malformed
		}
	}
	if more && (len(items) == 0 || next == "") || !more && cursor != nil {
		return page{}, Malformed
	}
	out := pick(m, "returned", "limit", "has_more", "next_cursor")
	if total, has := m["total"]; has {
		n, ok := integer(total)
		if !ok || n < len(items) {
			return page{}, Malformed
		}
		out["total"] = total
	}
	for _, k := range []string{"offset", "next_offset"} {
		if v, has := m[k]; has {
			if v != nil {
				n, ok := integer(v)
				if !ok || n < 0 {
					return page{}, Malformed
				}
			}
			out[k] = v
		}
	}
	if sortDir != "" {
		if m["sort_by"] != "created_at" || m["sort_dir"] != sortDir {
			return page{}, Malformed
		}
		out["sort_by"] = "created_at"
		out["sort_dir"] = sortDir
	}
	return page{items: items, meta: out, more: more, cursor: next}, nil
}

func taskPageProjection(q url.Values) project {
	return func(raw any) (any, []Resource, error) {
		limit, _ := strconv.Atoi(q.Get("limit"))
		p, err := parsePage(raw, limit, "")
		if err != nil {
			return nil, nil, err
		}
		if q.Get("include_total") == "true" && p.meta["total"] == nil {
			return nil, nil, Malformed
		}
		items := []any{}
		resources := []Resource{}
		seen := map[string]bool{}
		for _, raw := range p.items {
			t, r, err := compactTask(raw, false)
			if err != nil {
				return nil, nil, err
			}
			if seen[r.ID] || !matches(r, q) {
				return nil, nil, Malformed
			}
			seen[r.ID] = true
			items = append(items, t)
			resources = append(resources, r)
		}
		return object{"items": items, "meta": p.meta}, resources, nil
	}
}

func structuralProjection(kind string, q url.Values) project {
	return func(raw any) (any, []Resource, error) {
		p, err := parsePage(raw, 200, "")
		if err != nil {
			return nil, nil, err
		}
		items := []any{}
		resources := []Resource{}
		for _, raw := range p.items {
			in, ok := raw.(object)
			if !ok || !idPattern.MatchString(text(in["id"])) {
				return nil, nil, Malformed
			}
			keys := []string{"id", "name", "status"}
			if kind != "project" {
				keys = append(keys, "project_id")
			}
			if kind == "sprint" {
				keys = append(keys, "epic_id", "goal")
			}
			for _, k := range keys {
				if in[k] != nil {
					if _, ok := in[k].(string); !ok {
						return nil, nil, Malformed
					}
				}
			}
			t := pick(in, keys...)
			if kind == "sprint" {
				if goal, ok := t["goal"].(string); ok {
					units := utf16.Encode([]rune(goal))
					if len(units) > 300 {
						t["goal"] = string(utf16.Decode(append(units[:299], uint16('…'))))
					}
				}
			}
			r := Resource{Kind: kind, ID: text(in["id"]), ProjectID: text(in["project_id"]), EpicID: text(in["epic_id"]), Status: text(in["status"])}
			if !matches(r, q) {
				return nil, nil, Malformed
			}
			items = append(items, t)
			resources = append(resources, r)
		}
		return object{"items": items, "meta": p.meta}, resources, nil
	}
}

func facetProjection(q url.Values) project {
	return func(raw any) (any, []Resource, error) {
		in, ok := raw.(object)
		if !ok {
			return nil, nil, Malformed
		}
		n, ok := integer(in["matching_count"])
		if !ok || n < 0 {
			return nil, nil, Malformed
		}
		dims, ok := in["dimensions"].([]any)
		if !ok || len(dims) == 0 || len(dims) > 6 {
			return nil, nil, Malformed
		}
		requested := strings.Split(q.Get("dimensions"), ",")
		seen := map[string]bool{}
		for _, v := range dims {
			s := text(v)
			if !contains(requested, s) || seen[s] {
				return nil, nil, Malformed
			}
			seen[s] = true
		}
		if len(seen) != len(requested) {
			return nil, nil, Malformed
		}
		facets, ok := in["facets"].([]any)
		if !ok || len(facets) != len(dims) {
			return nil, nil, Malformed
		}
		out := []any{}
		resources := []Resource{}
		seen = map[string]bool{}
		for _, v := range facets {
			f, ok := v.(object)
			if !ok {
				return nil, nil, Malformed
			}
			d := text(f["dimension"])
			if !contains(requested, d) || seen[d] {
				return nil, nil, Malformed
			}
			seen[d] = true
			buckets, ok := f["buckets"].([]any)
			if !ok || len(buckets) > 200 {
				return nil, nil, Malformed
			}
			compact := []any{}
			for _, v := range buckets {
				b, bucketOK := v.(object)
				if !bucketOK {
					return nil, nil, Malformed
				}
				count, countOK := integer(b["count"])
				if !countOK || count < 0 {
					return nil, nil, Malformed
				}
				value, exists := b["value"]
				if !exists {
					return nil, nil, Malformed
				}
				if value != nil {
					if _, stringOK := value.(string); !stringOK {
						if _, numberOK := integer(value); !numberOK {
							return nil, nil, Malformed
						}
					}
				}
				compact = append(compact, pick(b, "value", "count"))
				if contains([]string{"project_id", "epic_id", "sprint_id"}, d) && text(value) != "" {
					if !idPattern.MatchString(text(value)) {
						return nil, nil, Malformed
					}
					resources = append(resources, Resource{Kind: strings.TrimSuffix(d, "_id"), ID: text(value)})
				}
			}
			fout := object{"dimension": d, "buckets": compact}
			returned, ok := integer(f["returned"])
			if !ok || returned != len(buckets) {
				return nil, nil, Malformed
			}
			fout["returned"] = f["returned"]
			if v, has := f["total_distinct"]; has {
				n, ok := integer(v)
				if !ok || n < len(buckets) {
					return nil, nil, Malformed
				}
				fout["total_distinct"] = v
			}
			if v, has := f["truncated"]; has {
				if _, ok := v.(bool); !ok {
					return nil, nil, Malformed
				}
				fout["truncated"] = v
			}
			out = append(out, fout)
		}
		return object{"matching_count": in["matching_count"], "dimensions": dims, "facets": out}, resources, nil
	}
}

func encoded(v any) []byte { b, _ := json.Marshal(v); return b }
