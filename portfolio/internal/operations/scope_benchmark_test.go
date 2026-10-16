package operations

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/projects"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/storage"
	"path/filepath"
	"testing"
	"time"
)

type generatedDirectory struct{ rows []projects.Project }

func (g generatedDirectory) ScopeKey() string { return "benchmark-owned" }
func (g generatedDirectory) Directory(context.Context) (projects.Directory, error) {
	return projects.Directory{Projects: g.rows, Evidence: projects.Evidence{Complete: true}}, nil
}

// Synthetic assumptions, not an operator census: scale1 = 1000 content rows,
// 100 workstreams, 20 canonical projects, 3000 item belongs_to edges.
// 80% ideas/20% roadmap; 5% ambiguous, 5% dangling item paths; 1% workstreams
// have a cycle. Scale10 multiplies each population; same fixed distribution.
func benchmarkService(b *testing.B, scale int) *Service {
	b.Helper()
	n, ws, pr := 1000*scale, 100*scale, 20*scale
	envelopes := map[string]object{}
	for _, db := range dbNames {
		envelopes[db] = object{"schema": "portfolio/" + db + "@1", "items": []any{}}
	}
	dir := generatedDirectory{}
	for p := 0; p < pr; p++ {
		dir.rows = append(dir.rows, projects.Project{URN: fmt.Sprintf("msg://project/generated/p%d", p), Kind: "project", ExternalIDs: []projects.ExternalID{{Substrate: "torque", ExternalID: fmt.Sprintf("PRJ-20261009-%04d", p+1)}}})
	}
	for w := 0; w < ws; w++ {
		item := object{"id": fmt.Sprintf("WS-%d", w), "title": "Synthetic workstream", "status": "active", "torque_project_ids": []any{fmt.Sprintf("PRJ-20261009-%04d", w%pr+1)}}
		if w%100 == 0 {
			item["workstream_ids"] = []any{item["id"]}
		}
		envelopes["workstreams"]["items"] = append(envelopes["workstreams"]["items"].([]any), item)
	}
	for j := 0; j < n; j++ {
		db, prefix, status := "ideas", "ID-", "new"
		if j%5 == 0 {
			db, prefix, status = "roadmap", "RM-", "in-progress"
		}
		p := j % pr
		targets := []any{fmt.Sprintf("WS-%d", p), fmt.Sprintf("WS-%d", p+pr), fmt.Sprintf("WS-%d", p+2*pr)}
		if j%20 == 1 {
			targets[2] = fmt.Sprintf("WS-%d", (p+1)%pr)
		}
		if j%20 == 2 {
			targets[2] = "WS-dangling"
		}
		item := object{"id": fmt.Sprintf("%s%d", prefix, j), "title": fmt.Sprintf("Synthetic row %d", j), "status": status, "kind": "idea", "workstream_ids": targets, "updated": "2026-10-08", "area": "hub", "horizon": "now"}
		envelopes[db]["items"] = append(envelopes[db]["items"].([]any), item)
	}
	files := map[string][]byte{}
	for db, env := range envelopes {
		raw, err := json.Marshal(env)
		if err != nil {
			b.Fatal(err)
		}
		files[db] = raw
	}
	store, err := storage.Open(b.Context(), filepath.Join(b.TempDir(), "generated.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	snap, err := storage.ParseSnapshot(files)
	if err != nil {
		b.Fatal(err)
	}
	if _, err = store.Import(b.Context(), snap); err != nil {
		b.Fatal(err)
	}
	if err = store.SyncProjects(b.Context(), dir, nil); err != nil {
		b.Fatal(err)
	}
	service := &Service{Store: store, Now: func() time.Time { return time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC) }}
	service.Torque = scopeUpstreamFunc(func(context.Context, string, object) (any, error) { return taskPage(), nil })
	b.Logf("generated content=%d workstreams=%d projects=%d item_belongs_to_edges=%d ambiguous_items=%d dangling_items=%d cyclic_workstreams=%d; no live data/upstream; selection=p1", n, ws, pr, n*3, n/20, n/20, ws/100)
	return service
}

func BenchmarkScopeGenerated(b *testing.B) {
	for _, scale := range []int{1, 10} {
		b.Run(fmt.Sprintf("scale%d", scale), func(b *testing.B) {
			service := benchmarkService(b, scale)
			state, err := service.Store.ReadState(b.Context())
			if err != nil {
				b.Fatal(err)
			}
			b.Run("resolver", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					m := state.Membership.Resolve("ID-1")
					if len(m.Projects) == 0 {
						b.Fatal(m)
					}
				}
			})
			for _, name := range []string{"list", "search", "board"} {
				for _, scoped := range []bool{false, true} {
					// Aggregate large list/search hits the intentionally fixed output bound;
					// bound its displayed rows with existing local paging for a fair comparison.
					b.Run(fmt.Sprintf("%s/scoped_%t", name, scoped), func(b *testing.B) {
						input := object{}
						if scoped {
							input["scope"] = object{"kind": "project", "id": "msg://project/generated/p1"}
						}
						if name == "list" {
							input["db"] = "ideas"
							input["page"] = object{"limit": 50}
						}
						if name == "search" {
							input["q"] = "synthetic"
							input["page"] = object{"limit": 50}
						}
						if name == "board" {
							input["limit"] = 30
						}
						b.ReportAllocs()
						for b.Loop() {
							if _, err := service.Call(b.Context(), Caller{}, name, input); err != nil {
								b.Fatal(err)
							}
						}
					})
				}
			}
		})
	}
}
