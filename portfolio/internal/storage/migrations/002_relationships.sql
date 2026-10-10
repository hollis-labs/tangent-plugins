-- Preserve the original receipt as provenance, but mark intentional mutations.
ALTER TABLE import_receipts ADD COLUMN mutated_at TEXT;
-- Upgrade existing copied stores without rewriting their lossless JSON/links.
INSERT OR IGNORE INTO edges(from_id,to_id,type,field)
 SELECT item_id,ref,'belongs_to','links.workstream' FROM links WHERE kind='workstream';
