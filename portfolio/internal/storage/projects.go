package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/hollis-labs/tangent-plugins/portfolio/internal/projects"
)

// RegistryReader supplies only directory reads.
type RegistryReader interface {
	Directory(context.Context) (projects.Directory, error)
	ScopeKey() string
}

// ProjectReader supplies only Torque enrichment reads.
type ProjectReader interface {
	Project(context.Context, string) (projects.TorqueProject, error)
	ScopeKey() string
}

// Overlay is independent, test-owned local metadata with no production carrier.
type Overlay struct {
	Tags         []string                   `json:"tags"`
	Notes        string                     `json:"notes"`
	CustomFields map[string]json.RawMessage `json:"custom_fields"`
}

// ProjectRecord separates canonical source, enrichment and local overlay provenance.
type ProjectRecord struct {
	URN             string
	Source          projects.Project
	Enrichment      map[string]projects.TorqueProject
	Overlay         Overlay
	OverlayRev      int64
	RegistryState   string
	TorqueState     string
	FetchedAt       string
	TorqueFetchedAt string
	MappingState    string
}

func failureState(err error) string {
	switch {
	case errors.Is(err, projects.Denied):
		return "denied"
	case errors.Is(err, projects.Missing), errors.Is(err, projects.Invalid):
		return "missing_integration"
	case errors.Is(err, projects.NotFound):
		return "unresolved"
	default:
		return "stale"
	}
}

// SyncProjects updates only local derived rows, never overlay or copied items.
// A complete active-directory result alone can mark prior rows absent.
func (s *Store) SyncProjects(ctx context.Context, registry RegistryReader, torque ProjectReader) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var dir projects.Directory
	var readErr error
	if registry == nil {
		readErr = projects.Missing
	} else {
		dir, readErr = registry.Directory(ctx)
	}
	if dir.Evidence.Partial {
		dir.Evidence.Complete = false
	}
	enrich := map[string]map[string]projects.TorqueProject{}
	states := map[string]string{}
	seen := map[string]bool{}
	torqueInvalid := ""
	enrichmentReads := 0
	for _, p := range dir.Projects {
		if !projects.ValidURN(p.URN) || p.Kind != "project" || seen[p.URN] {
			return projects.Malformed
		}
		seen[p.URN] = true
		enrich[p.URN] = map[string]projects.TorqueProject{}
		states[p.URN] = "fresh"
		ids := p.TorqueIDs()
		if len(ids) == 0 {
			states[p.URN] = "unresolved"
		}
		for _, id := range ids {
			var ep projects.TorqueProject
			var err error
			if torqueInvalid != "" {
				states[p.URN] = torqueInvalid
				break
			} else if enrichmentReads >= 200 {
				states[p.URN] = "partial"
				continue
			} else if torque == nil {
				err = projects.Missing
			} else {
				enrichmentReads++
				ep, err = torque.Project(ctx, id)
			}
			if err != nil {
				states[p.URN] = combineState(states[p.URN], failureState(err))
				if errors.Is(err, projects.Denied) || errors.Is(err, projects.Missing) || errors.Is(err, projects.Invalid) {
					torqueInvalid = failureState(err)
					enrich[p.URN] = map[string]projects.TorqueProject{}
					break
				}
				continue
			}
			if ep.ID != id {
				return projects.Malformed
			}
			enrich[p.URN][id] = ep
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	registryKey, torqueKey := "", ""
	if registry != nil {
		registryKey = registry.ScopeKey()
	}
	if torque != nil {
		torqueKey = torque.ScopeKey()
	}
	if err = invalidatePartition(ctx, tx, "registry", registryKey); err != nil {
		return err
	}
	if err = invalidatePartition(ctx, tx, "torque", torqueKey); err != nil {
		return err
	}
	if torqueInvalid != "" {
		if _, err = tx.ExecContext(ctx, "UPDATE projects SET enrichment='{}',torque_state=?", torqueInvalid); err != nil {
			return err
		}
		for urn := range states {
			states[urn] = torqueInvalid
			enrich[urn] = map[string]projects.TorqueProject{}
		}
	}
	state := "partial"
	if dir.Evidence.Complete && readErr == nil {
		state = "absent"
	}
	if readErr != nil {
		state = failureState(readErr)
		dir.Evidence.Complete = false
		dir.Evidence.Partial = true
		dir.Evidence.Code = state
	}
	if state == "denied" || state == "missing_integration" {
		_, err = tx.ExecContext(ctx, "UPDATE projects SET source_data='{}',enrichment='{}',registry_state=?,torque_state='invalidated'", state)
		dir.Projects = nil
	} else {
		_, err = tx.ExecContext(ctx, "UPDATE projects SET registry_state=?", state)
	}
	if err != nil {
		return err
	}
	for _, p := range dir.Projects {
		data := enrich[p.URN]
		torqueTime := now
		if states[p.URN] == "stale" || states[p.URN] == "partial" {
			var old, priorTime string
			scanErr := tx.QueryRowContext(ctx, "SELECT enrichment,COALESCE(torque_fetched_at,'') FROM projects WHERE urn=?", p.URN).Scan(&old, &priorTime)
			if scanErr != nil && !errors.Is(scanErr, sql.ErrNoRows) {
				return scanErr
			}
			prior := map[string]projects.TorqueProject{}
			if old != "" {
				if err = json.Unmarshal([]byte(old), &prior); err != nil {
					return err
				}
			}
			for _, id := range p.TorqueIDs() {
				if _, ok := data[id]; !ok {
					if v, exists := prior[id]; exists {
						data[id] = v
					}
				}
			}
			torqueTime = priorTime
		}
		registryState := "partial"
		if dir.Evidence.Complete {
			registryState = "fresh"
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO projects(urn,source_data,enrichment,registry_state,torque_state,fetched_at,torque_fetched_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(urn) DO UPDATE SET source_data=excluded.source_data,enrichment=excluded.enrichment,registry_state=excluded.registry_state,torque_state=excluded.torque_state,fetched_at=excluded.fetched_at,torque_fetched_at=excluded.torque_fetched_at`, p.URN, encode(p), encode(data), registryState, states[p.URN], now, torqueTime)
		if err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO project_sync_state(source,evidence,fetched_at) VALUES('registry',?,?) ON CONFLICT(source) DO UPDATE SET evidence=excluded.evidence,fetched_at=excluded.fetched_at`, encode(dir.Evidence), now); err != nil {
		return err
	}
	return tx.Commit()
}

func invalidatePartition(ctx context.Context, tx *sql.Tx, source, key string) error {
	var old string
	err := tx.QueryRowContext(ctx, "SELECT partition_key FROM project_source_partitions WHERE source=?", source).Scan(&old)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if old != key {
		query := "UPDATE projects SET enrichment='{}',torque_state='invalidated'"
		if source == "registry" {
			query = "UPDATE projects SET source_data='{}',enrichment='{}',registry_state='invalidated',torque_state='invalidated'"
		}
		if _, err = tx.ExecContext(ctx, query); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO project_source_partitions(source,partition_key) VALUES(?,?) ON CONFLICT(source) DO UPDATE SET partition_key=excluded.partition_key`, source, key)
	return err
}

// Projects includes stale/absent/invalidated identities without inventing items.
func (s *Store) Projects(ctx context.Context) ([]ProjectRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.urn,p.source_data,p.enrichment,p.registry_state,p.torque_state,COALESCE(p.fetched_at,''),COALESCE(p.torque_fetched_at,''),COALESCE(o.data,'{}'),COALESCE(o.rev,0) FROM projects p LEFT JOIN project_overlays o ON o.urn=p.urn ORDER BY p.urn`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ProjectRecord{}
	for rows.Next() {
		var p ProjectRecord
		var source, enrichment, overlay string
		if err = rows.Scan(&p.URN, &source, &enrichment, &p.RegistryState, &p.TorqueState, &p.FetchedAt, &p.TorqueFetchedAt, &overlay, &p.OverlayRev); err != nil {
			return nil, err
		}
		for _, v := range []struct {
			data   string
			target any
		}{{source, &p.Source}, {enrichment, &p.Enrichment}, {overlay, &p.Overlay}} {
			if err = json.Unmarshal([]byte(v.data), v.target); err != nil {
				return nil, err
			}
		}
		out = append(out, p)
		out[len(out)-1].MappingState = p.Source.MappingState()
	}
	return out, rows.Err()
}

func combineState(prior, next string) string {
	rank := map[string]int{"fresh": 0, "unresolved": 1, "partial": 2, "stale": 3, "missing_integration": 4, "denied": 5}
	if rank[next] > rank[prior] {
		return next
	}
	return prior
}

// ProjectSyncEvidence returns the last directory completeness/failure provenance.
// Upstream read failures are recorded by SyncProjects; its error is a local
// persistence/validation failure, not an upstream error body.
func (s *Store) ProjectSyncEvidence(ctx context.Context) (projects.Evidence, string, error) {
	var body, at string
	err := s.db.QueryRowContext(ctx, "SELECT evidence,fetched_at FROM project_sync_state WHERE source='registry'").Scan(&body, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return projects.Evidence{Partial: true, Code: "not_synced"}, "", nil
	}
	if err != nil {
		return projects.Evidence{}, "", err
	}
	var e projects.Evidence
	if err = json.Unmarshal([]byte(body), &e); err != nil {
		return e, "", err
	}
	return e, at, nil
}

// SetProjectOverlay is shadow-only CAS. No API/CLI/MCP exposes this mutation.
func (s *Store) SetProjectOverlay(ctx context.Context, urn string, wantRev int64, value Overlay) (int64, error) {
	if !projects.ValidURN(urn) || wantRev < 0 {
		return 0, projects.Invalid
	}
	body, err := json.Marshal(value)
	if err != nil || len(body) > 64<<10 {
		return 0, projects.Invalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	old := "{}"
	var rev int64
	err = tx.QueryRowContext(ctx, "SELECT data,rev FROM project_overlays WHERE urn=?", urn).Scan(&old, &rev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if rev != wantRev {
		return rev, errors.New("project overlay revision conflict")
	}
	var prior Overlay
	if err = json.Unmarshal([]byte(old), &prior); err != nil {
		return 0, err
	}
	normalized, err := json.Marshal(prior)
	if err != nil {
		return 0, err
	}
	if string(normalized) == string(body) {
		return rev, tx.Commit()
	}
	if rev == math.MaxInt64 {
		return rev, errors.New("project overlay revision exhausted")
	}
	rev++
	if _, err = tx.ExecContext(ctx, `INSERT INTO project_overlays(urn,data,rev) VALUES(?,?,?) ON CONFLICT(urn) DO UPDATE SET data=excluded.data,rev=excluded.rev`, urn, string(body), rev); err != nil {
		return 0, err
	}
	return rev, tx.Commit()
}
