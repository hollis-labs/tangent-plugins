-- The 001 projects placeholder was never part of the copied-item projection.
-- Retain any placeholder data as historical evidence, never promote it to source truth.
ALTER TABLE projects RENAME TO legacy_projects;
CREATE TABLE projects (
 urn TEXT PRIMARY KEY,
 source_data TEXT NOT NULL CHECK(json_valid(source_data)),
 enrichment TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(enrichment)),
 registry_state TEXT NOT NULL,
 torque_state TEXT NOT NULL,
 fetched_at TEXT,
 torque_fetched_at TEXT
);
CREATE TABLE project_overlays (
 urn TEXT PRIMARY KEY,
 data TEXT NOT NULL CHECK(json_valid(data)),
 rev INTEGER NOT NULL CHECK(rev>=0)
);
CREATE TABLE project_sync_state (
 source TEXT PRIMARY KEY,
 evidence TEXT NOT NULL CHECK(json_valid(evidence)),
 fetched_at TEXT NOT NULL
);
-- Opaque endpoint/reference-epoch digests, never raw config or credentials.
CREATE TABLE project_source_partitions (source TEXT PRIMARY KEY, partition_key TEXT NOT NULL);
