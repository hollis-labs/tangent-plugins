package torqueclient

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"sync"
)

// Read supports exactly the seven read-only tracker operations. Opaque proof
// is passed only to the trusted verifier, never sent upstream or cached.
func (c *Client) Read(ctx context.Context, proof any, name string, input map[string]any) (any, error) {
	return c.read(ctx, proof, name, input, nil)
}

func (c *Client) read(ctx context.Context, proof any, name string, input map[string]any, parent ParentGuard) (any, error) {
	p, err := prepare(name, input)
	if err != nil {
		return nil, err
	}
	o, cancel, err := c.begin(ctx, proof, name)
	if err != nil {
		return nil, err
	}
	defer cancel()
	o.parent = parent
	r := Request{Operation: name, Path: p.path, Query: p.query.Encode(), Phase: "query", Resources: p.requested}
	g, err := o.check(r)
	if err != nil {
		return nil, err
	}
	cache := name != "torque_task"
	key := c.key(g, r)
	if cache {
		if hit, ok := c.cached(key); ok {
			if _, err = o.check(r); err != nil {
				return nil, err
			}
			r.Phase = "disclosure"
			if json.Unmarshal(hit.resources, &r.Resources) != nil {
				return nil, Malformed
			}
			v, decodeErr := decode(hit.value)
			if decodeErr != nil {
				return nil, decodeErr
			}
			if _, err = o.check(r); err != nil {
				return nil, err
			}
			return v, nil
		}
	}
	var result any
	var resources []Resource
	switch name {
	case "torque_task":
		var taskResource Resource
		task, taskErr := o.get(p.path, url.Values{}, p.requested, func(raw any) (any, []Resource, error) {
			t, r, projectionErr := compactTask(raw, true)
			if projectionErr != nil {
				return nil, nil, projectionErr
			}
			if r.ID != p.ids[0] {
				return nil, nil, Malformed
			}
			taskResource = r
			return t, []Resource{r}, nil
		})
		if taskErr != nil {
			return nil, taskErr
		}
		comments, commentResources, commentsErr := o.comments(p.path+"/comments", url.Values{"limit": {"200"}, "sort_by": {"created_at"}, "sort_dir": {"asc"}}, p.ids, true)
		if commentsErr != nil {
			return nil, commentsErr
		}
		resources = append([]Resource{taskResource}, commentResources...)
		result = object{"task": task, "comments": comments}
	case "torque_titles":
		result, resources, err = o.titles(p.ids)
	case "torque_tasks":
		result, err = o.get(p.path, p.query, nil, func(raw any) (any, []Resource, error) {
			v, r, projectionErr := taskPageProjection(p.query)(raw)
			resources = r
			return v, r, projectionErr
		})
	case "torque_facets":
		result, err = o.get(p.path, p.query, nil, func(raw any) (any, []Resource, error) {
			v, r, projectionErr := facetProjection(p.query)(raw)
			resources = r
			return v, r, projectionErr
		})
	default:
		result, resources, err = o.structures(p)
	}
	if err != nil {
		return nil, err
	}
	compact := encoded(result)
	if int64(len(compact)) > c.maxBody {
		return nil, TooLarge
	}
	v, err := decode(compact)
	if err != nil {
		return nil, err
	}
	r.Phase = "disclosure"
	r.Resources = resources
	if _, err = o.check(r); err != nil {
		return nil, err
	}
	if cache {
		complete := true
		if name == "torque_facets" {
			for _, f := range v.(object)["facets"].([]any) {
				if f.(object)["truncated"] == true {
					complete = false
				}
			}
		}
		if complete {
			c.remember(key, cacheEntry{value: compact, resources: encoded(resources)})
			// Cache insertion can wait on a mutex. Ordinary parent and exact child
			// authority must both still hold after that wait, before disclosure.
			if _, err = o.check(r); err != nil {
				c.forget(key)
				return nil, err
			}
		}
	}
	// Checks are current at disclosure, not an atomic revocation/return barrier.
	// Same detached JSON representation on a miss and a hit.
	return v, nil
}

func (o *operation) titles(ids []string) (any, []Resource, error) {
	out := object{}
	resources := []Resource{}
	var mu sync.Mutex
	ctx, cancel := context.WithCancel(o.ctx)
	defer cancel()
	jobs := make(chan string)
	var wg sync.WaitGroup
	var failure error
	for range min(6, len(ids)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := *o
			local.ctx = ctx
			for id := range jobs {
				requested := []Resource{{Kind: "task", ID: id}}
				var admitted []Resource
				v, err := local.get("/tasks/"+id, url.Values{}, requested, func(raw any) (any, []Resource, error) {
					t, r, err := compactTask(raw, false)
					if err != nil {
						return nil, nil, err
					}
					if r.ID != id {
						return nil, nil, Malformed
					}
					admitted = []Resource{r}
					title := t["title"]
					if title == nil {
						title = ""
					}
					return object{"title": title, "status": t["status"], "priority": t["priority"]}, admitted, nil
				})
				mu.Lock()
				if err == nil {
					out[id] = v
					resources = append(resources, admitted...)
				} else if isFailure(err, NotFound) {
					out[id] = nil
					resources = append(resources, requested...)
				} else if failure == nil {
					failure = err
					cancel()
				}
				mu.Unlock()
			}
		}()
	}
send:
	for _, id := range ids {
		select {
		case jobs <- id:
		case <-ctx.Done():
			break send
		}
	}
	close(jobs)
	wg.Wait()
	if failure != nil {
		return nil, nil, failure
	}
	if ctx.Err() != nil {
		return nil, nil, Unavailable
	}
	return out, resources, nil
}

func (o *operation) structures(p plan) (any, []Resource, error) {
	q := url.Values{}
	for k, v := range p.query {
		q[k] = append([]string(nil), v...)
	}
	kind := strings.TrimSuffix(strings.TrimPrefix(p.path, "/"), "s")
	projection := structuralProjection(kind, q)
	out := []any{}
	resources := []Resource{}
	seen := map[string]bool{}
	cursors := map[string]bool{}
	var total any
	for range o.client.maxPages {
		var pageResources []Resource
		v, err := o.get(p.path, q, nil, func(raw any) (any, []Resource, error) { v, r, e := projection(raw); pageResources = r; return v, r, e })
		if err != nil {
			return nil, nil, err
		}
		page := v.(object)
		meta := page["meta"].(object)
		if t := meta["total"]; t != nil {
			if total != nil && total != t {
				return nil, nil, Malformed
			}
			total = t
		}
		for _, item := range page["items"].([]any) {
			id := text(item.(object)["id"])
			if seen[id] {
				return nil, nil, Malformed
			}
			seen[id] = true
			out = append(out, item)
		}
		resources = append(resources, pageResources...)
		if int64(len(encoded(out))) > o.client.maxBody {
			return nil, nil, TooLarge
		}
		if meta["has_more"] == false {
			return out, resources, nil
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
