package storage

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/hollis-labs/tangent-plugins/portfolio/internal/projects"
)

type directoryFake struct {
	result projects.Directory
	err    error
}

func (f directoryFake) Directory(context.Context) (projects.Directory, error) { return f.result, f.err }

func (f directoryFake) ScopeKey() string { return "fixture-registry" }

type projectFake struct {
	name string
	err  error
}

func (f projectFake) ScopeKey() string { return "fixture-torque" }

func (f projectFake) Project(_ context.Context, id string) (projects.TorqueProject, error) {
	return projects.TorqueProject{ID: id, Name: f.name, Status: "active"}, f.err
}
func directory() directoryFake {
	return directoryFake{result: projects.Directory{Projects: []projects.Project{{URN: "msg://project/example/a", Kind: "project", DisplayName: "Registry title", Status: "active", ExternalIDs: []projects.ExternalID{{Substrate: "torque", ExternalID: "PRJ-20261009-0001"}}}}, Evidence: projects.Evidence{Complete: true, Pages: 1}}}
}
func getProject(t *testing.T, s *Store) ProjectRecord {
	t.Helper()
	rows, err := s.Projects(t.Context())
	if err != nil || len(rows) != 1 {
		t.Fatalf("projects: %v %v", rows, err)
	}
	return rows[0]
}
func TestProjectSyncOverlayAndLosslessBoundary(t *testing.T) {
	s := openTest(t)
	snap := fixture(t)
	if _, err := s.Import(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	source := directory()
	overlay := Overlay{Tags: []string{"local"}, Notes: "Owned note", CustomFields: map[string]json.RawMessage{"large": json.RawMessage(`9007199254740993`)}}
	rev, err := s.SetProjectOverlay(t.Context(), source.result.Projects[0].URN, 0, overlay)
	if err != nil || rev != 1 {
		t.Fatal(rev, err)
	}
	for range 2 {
		if err = s.SyncProjects(t.Context(), source, projectFake{name: "Torque name"}); err != nil {
			t.Fatal(err)
		}
	}
	p := getProject(t, s)
	if p.OverlayRev != 1 || !reflect.DeepEqual(p.Overlay, overlay) || p.Source.DisplayName != "Registry title" || p.Enrichment["PRJ-20261009-0001"].Name != "Torque name" {
		t.Fatalf("lost source/overlay: %+v", p)
	}
	if rev, err = s.SetProjectOverlay(t.Context(), p.URN, 1, overlay); err != nil || rev != 1 {
		t.Fatal("non-idempotent overlay")
	}
	if _, err = s.SetProjectOverlay(t.Context(), p.URN, 0, Overlay{}); err == nil {
		t.Fatal("CAS accepted")
	}
	if got := currentSnapshot(t, s); got.digest != snap.digest {
		t.Fatal("sync changed copied source")
	}
	if applied, replayErr := s.Import(t.Context(), snap); replayErr != nil || applied {
		t.Fatal("derived projects broke replay", err)
	}
	source.result.Projects[0].DisplayName = "Changed registry"
	if err = s.SyncProjects(t.Context(), source, projectFake{name: "Updated Torque"}); err != nil {
		t.Fatal(err)
	}
	p = getProject(t, s)
	if p.Source.DisplayName != "Changed registry" || p.OverlayRev != 1 || !reflect.DeepEqual(p.Overlay, overlay) {
		t.Fatal("resync moved overlay")
	}
}
func TestSyncPartialOutageAbsenceAndDenial(t *testing.T) {
	s := openTest(t)
	source := directory()
	urn := source.result.Projects[0].URN
	if _, err := s.SetProjectOverlay(t.Context(), urn, 0, Overlay{Notes: "keep"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncProjects(t.Context(), source, projectFake{name: "safe"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncProjects(t.Context(), directoryFake{err: projects.Unavailable}, projectFake{}); err != nil {
		t.Fatal(err)
	}
	p := getProject(t, s)
	if p.RegistryState != "stale" || p.Source.DisplayName == "" || p.Overlay.Notes != "keep" {
		t.Fatal("outage erased snapshot")
	}
	if err := s.SyncProjects(t.Context(), directoryFake{result: projects.Directory{Evidence: projects.Evidence{Partial: true}}}, projectFake{}); err != nil {
		t.Fatal(err)
	}
	if getProject(t, s).RegistryState != "partial" {
		t.Fatal("partial marked absence")
	}
	if err := s.SyncProjects(t.Context(), source, projectFake{err: projects.Unavailable}); err != nil {
		t.Fatal(err)
	}
	p = getProject(t, s)
	if p.TorqueState != "stale" || p.Enrichment["PRJ-20261009-0001"].Name != "safe" {
		t.Fatal("enrichment outage lost safe data")
	}
	if err := s.SyncProjects(t.Context(), source, projectFake{err: projects.Denied}); err != nil {
		t.Fatal(err)
	}
	p = getProject(t, s)
	if len(p.Enrichment) != 0 || p.TorqueState != "denied" {
		t.Fatal("denial served protected enrichment")
	}
	if err := s.SyncProjects(t.Context(), directoryFake{err: projects.Denied}, nil); err != nil {
		t.Fatal(err)
	}
	p = getProject(t, s)
	if p.Source.DisplayName != "" || len(p.Enrichment) != 0 || p.Overlay.Notes != "keep" || p.URN != urn {
		t.Fatal("registry denial retained protected metadata or erased overlay")
	}
	if err := s.SyncProjects(t.Context(), directoryFake{result: projects.Directory{Evidence: projects.Evidence{Complete: true}}}, nil); err != nil {
		t.Fatal(err)
	}
	if getProject(t, s).RegistryState != "absent" {
		t.Fatal("complete empty did not mark directory absence")
	}
}
func TestSyncTransactionRollbackAndConcurrentOverlay(t *testing.T) {
	s := openTest(t)
	source := directory()
	if _, err := s.db.Exec(`CREATE TRIGGER fail_project BEFORE INSERT ON projects BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncProjects(t.Context(), source, nil); err == nil {
		t.Fatal("fault not returned")
	}
	rows, err := s.Projects(t.Context())
	if err != nil || len(rows) != 0 {
		t.Fatal("partial commit")
	}
	if _, err = s.db.Exec("DROP TRIGGER fail_project"); err != nil {
		t.Fatal(err)
	}
	urn := source.result.Projects[0].URN
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, note := range []string{"one", "two"} {
		wg.Go(func() {
			_, writeErr := s.SetProjectOverlay(context.Background(), urn, 0, Overlay{Notes: note})
			results <- writeErr
		})
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatal("CAS did not serialize")
	}
	if err = s.SyncProjects(t.Context(), source, nil); err != nil {
		t.Fatal(err)
	}
	if getProject(t, s).OverlayRev != 1 {
		t.Fatal("sync changed concurrent overlay")
	}
}

type tasksFake struct{ calls [][2]string }

func (f *tasksFake) Tasks(_ context.Context, id, tag string) (projects.TaskPage, error) {
	f.calls = append(f.calls, [2]string{id, tag})
	return projects.TaskPage{Tasks: []projects.Task{{ID: "CW-20261009-0001", ProjectID: "PRJ-20261009-0001", Title: "Read-only task"}}, Evidence: projects.Evidence{Complete: true}}, nil
}
func TestProjectViewManyToOneAmbiguityAndScopeBeforeCap(t *testing.T) {
	s := openTest(t)
	base := fixture(t)
	files := map[string][]byte{}
	for name, env := range base.envelopes {
		files[name] = []byte(encode(env))
	}
	files["workstreams"] = []byte(`{"schema":"portfolio/workstreams@1","items":[{"id":"WS-first","title":"One","torque_project_ids":["PRJ-20261009-0001"],"torque_tag":"explicit-tag"},{"id":"WS-second","title":"Two","torque_project_ids":["PRJ-20261009-0001"]},{"id":"WS-missing","title":"Missing","torque_project_ids":["PRJ-20261009-0099"]}]}`)
	files["ideas"] = []byte(`{"schema":"portfolio/ideas@1","items":[{"id":"ID-unscoped","title":"No membership"},{"id":"ID-member","title":"Member","workstream_ids":["WS-first"]},{"id":"ID-external","title":"External","project_ids":["msg://project/external/a"]}]}`)
	snap, err := ParseSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Import(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	source := directory()
	other := source.result.Projects[0]
	other.URN = "msg://project/example/b"
	source.result.Projects = append(source.result.Projects, other)
	if err = s.SyncProjects(t.Context(), source, projectFake{}); err != nil {
		t.Fatal(err)
	}
	fake := &tasksFake{}
	view, err := s.ViewProject(t.Context(), "msg://project/example/a", 1, fake)
	if err != nil {
		t.Fatal(err)
	}
	if view.LocalTotal != 3 || len(view.Items) != 1 || !view.LocalTruncated || !view.Items[0].Membership.Ambiguous {
		t.Fatalf("scope after cap or lost ambiguity: %+v", view)
	}
	if len(view.Tasks) != 1 || !view.Tasks[0].Membership.Ambiguous || len(view.Tasks[0].Membership.Projects) != 2 {
		t.Fatal("task mapping collapsed")
	}
	if len(fake.calls) != 2 {
		t.Fatalf("explicit selectors: %v", fake.calls)
	}
	external, err := s.ViewProject(t.Context(), "msg://project/external/a", 20, nil)
	if err != nil || external.LocalTotal != 0 {
		t.Fatal("unsynced external URN confirmed canonical membership", err)
	}
	found := false
	for _, item := range external.Unscoped {
		if item.ID == "ID-external" && len(item.Membership.Unresolved) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("unsynced external reference lost diagnostic evidence")
	}
	if got := currentSnapshot(t, s); got.digest != snap.digest {
		t.Fatal("view changed source")
	}
	if _, err = s.ViewProject(t.Context(), "label", 20, nil); !errors.Is(err, projects.Invalid) {
		t.Fatal("display-label scope accepted")
	}
}

type scopedDirectoryFake struct {
	directoryFake
	key string
}

func (f scopedDirectoryFake) ScopeKey() string { return f.key }
func TestSourceChangeAndGlobalDenialInvalidatesProtectedRows(t *testing.T) {
	s := openTest(t)
	source := directory()
	p := source.result.Projects[0]
	p.URN = "msg://project/example/b"
	source.result.Projects = append(source.result.Projects, p)
	if err := s.SyncProjects(t.Context(), source, projectFake{name: "protected"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncProjects(t.Context(), scopedDirectoryFake{directoryFake{err: projects.Unavailable}, "new-reference-epoch"}, projectFake{}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Projects(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Source.DisplayName != "" || len(r.Enrichment) != 0 {
			t.Fatal("source change reused old protected cache")
		}
	}
	if err = s.SyncProjects(t.Context(), source, projectFake{name: "new"}); err != nil {
		t.Fatal(err)
	}
	// A denial affecting one currently returned row also invalidates cached rows
	// omitted by this partial response.
	source.result.Projects = source.result.Projects[:1]
	source.result.Evidence = projects.Evidence{Partial: true}
	if err = s.SyncProjects(t.Context(), source, projectFake{err: projects.Denied}); err != nil {
		t.Fatal(err)
	}
	rows, err = s.Projects(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if len(r.Enrichment) != 0 || r.TorqueState != "denied" {
			t.Fatal("denied source left omitted protected row")
		}
	}
}

type deniedTasksFake struct{ calls int }

func (f *deniedTasksFake) Tasks(_ context.Context, id, tag string) (projects.TaskPage, error) {
	f.calls++
	if f.calls > 1 {
		return projects.TaskPage{Evidence: projects.Evidence{Total: new(int)}}, projects.Denied
	}
	return projects.TaskPage{Tasks: []projects.Task{{ID: "CW-20261009-0001", ProjectID: id}}, Evidence: projects.Evidence{Complete: true, Total: new(int)}}, nil
}
func TestProjectTaskDenialDropsEarlierRowsAndTotals(t *testing.T) {
	s := openTest(t)
	base := fixture(t)
	files := map[string][]byte{}
	for name, env := range base.envelopes {
		files[name] = []byte(encode(env))
	}
	files["workstreams"] = []byte(`{"schema":"portfolio/workstreams@1","items":[{"id":"WS-a","title":"Scoped","project_ids":["msg://project/example/a"],"torque_tag":"z-tag"}]}`)
	snap, err := ParseSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Import(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	if err = s.SyncProjects(t.Context(), directory(), projectFake{name: "protected"}); err != nil {
		t.Fatal(err)
	}
	fake := &deniedTasksFake{}
	view, err := s.ViewProject(t.Context(), "msg://project/example/a", 20, fake)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Tasks) != 0 || view.TaskTotal != 0 || fake.calls != 2 {
		t.Fatal("denial kept authenticated fallback")
	}
	for _, e := range view.TaskEvidence {
		if e.Evidence.Total != nil || e.Evidence.Code != "denied" {
			t.Fatal("denial retained protected total")
		}
	}
	if p := getProject(t, s); len(p.Enrichment) != 0 || p.TorqueState != "denied" {
		t.Fatal("view denial did not invalidate protected metadata")
	}
}
func TestProjectsMigrationPreservesCopiedProjectionAndHistoricalPlaceholder(t *testing.T) {
	s := openTest(t)
	snap := fixture(t)
	if _, err := s.Import(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	// Build a private version002 state with the original historical placeholder.
	for _, q := range []string{"DROP TABLE projects", "DROP TABLE project_overlays", "DROP TABLE project_sync_state", "DROP TABLE project_source_partitions", "ALTER TABLE legacy_projects RENAME TO projects", "DELETE FROM schema_migrations WHERE version=3", `INSERT INTO projects VALUES('old-urn','[]','{"historical":true}','{"notes":"historic"}','then')`} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	var old string
	if err := s.db.QueryRow("SELECT overlay FROM legacy_projects WHERE urn='old-urn'").Scan(&old); err != nil || old != `{"notes":"historic"}` {
		t.Fatal("historical placeholder lost", err)
	}
	rows, err := s.Projects(t.Context())
	if err != nil || len(rows) != 0 {
		t.Fatal("untrusted placeholder promoted")
	}
	if currentSnapshot(t, s).digest != snap.digest {
		t.Fatal("migration changed copied source")
	}
	evidence, _, err := s.ProjectSyncEvidence(t.Context())
	if err != nil || evidence.Code != "not_synced" {
		t.Fatal(err)
	}
	if err = s.SyncProjects(t.Context(), directoryFake{err: projects.Unavailable}, nil); err != nil {
		t.Fatal(err)
	}
	evidence, at, err := s.ProjectSyncEvidence(t.Context())
	if err != nil || evidence.Complete || !evidence.Partial || at == "" {
		t.Fatal("sync failure provenance unavailable", err)
	}
}
