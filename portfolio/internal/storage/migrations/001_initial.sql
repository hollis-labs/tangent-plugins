CREATE TABLE id_reservations (id TEXT PRIMARY KEY, db TEXT NOT NULL);
CREATE TRIGGER reservation_no_delete BEFORE DELETE ON id_reservations BEGIN SELECT RAISE(ABORT, 'portfolio ids cannot be reused'); END;
CREATE TRIGGER reservation_no_change BEFORE UPDATE ON id_reservations BEGIN SELECT RAISE(ABORT, 'portfolio ids cannot be renamed'); END;
CREATE TABLE envelopes (db TEXT PRIMARY KEY, data TEXT NOT NULL CHECK(json_valid(data)));
CREATE TABLE items (
 id TEXT PRIMARY KEY REFERENCES id_reservations(id), db TEXT NOT NULL REFERENCES envelopes(db),
 ordinal INTEGER NOT NULL CHECK(ordinal >= 0), title TEXT NOT NULL, status TEXT,
 rev INTEGER NOT NULL CHECK(rev >= 0), sort_order REAL, created TEXT, updated TEXT, author TEXT,
 data TEXT NOT NULL CHECK(json_valid(data)), UNIQUE(db, ordinal)
);
CREATE TRIGGER item_identity_immutable BEFORE UPDATE OF id,db ON items WHEN OLD.id != NEW.id OR OLD.db != NEW.db BEGIN SELECT RAISE(ABORT, 'portfolio identity is immutable'); END;
CREATE TRIGGER item_no_delete BEFORE DELETE ON items BEGIN SELECT RAISE(ABORT, 'drop portfolio items by status'); END;
CREATE TABLE comments (item_id TEXT NOT NULL REFERENCES items(id), n INTEGER NOT NULL, id TEXT NOT NULL, author TEXT, text TEXT, created TEXT, kind TEXT, data TEXT NOT NULL CHECK(json_valid(data)), PRIMARY KEY(item_id,n), UNIQUE(item_id,id));
CREATE TABLE links (item_id TEXT NOT NULL REFERENCES items(id), n INTEGER NOT NULL, kind TEXT NOT NULL, ref TEXT NOT NULL, label TEXT, data TEXT NOT NULL CHECK(json_valid(data)), PRIMARY KEY(item_id,n));
-- Targets can be external Torque ids or retained dangling references. Never invent nodes.
CREATE TABLE edges (from_id TEXT NOT NULL REFERENCES items(id), to_id TEXT NOT NULL, type TEXT NOT NULL CHECK(type IN ('decision_gates','informs','supersedes','belongs_to','depends_on','related')), field TEXT NOT NULL, PRIMARY KEY(from_id,to_id,type,field));
CREATE TABLE projects (urn TEXT PRIMARY KEY, external_ids TEXT NOT NULL CHECK(json_valid(external_ids)), source_data TEXT NOT NULL CHECK(json_valid(source_data)), overlay TEXT NOT NULL CHECK(json_valid(overlay)), synced_at TEXT);
CREATE TABLE import_receipts (singleton INTEGER PRIMARY KEY DEFAULT 1 CHECK(singleton=1), digest TEXT NOT NULL UNIQUE, imported_at TEXT NOT NULL);
CREATE VIRTUAL TABLE items_fts USING fts5(id UNINDEXED, text, tokenize='trigram');
CREATE TRIGGER item_search_insert AFTER INSERT ON items BEGIN INSERT INTO items_fts(id,text) VALUES(NEW.id,portfolio_search_text(NEW.data)); END;
CREATE TRIGGER item_search_update AFTER UPDATE OF data ON items BEGIN DELETE FROM items_fts WHERE id=OLD.id; INSERT INTO items_fts(id,text) VALUES(NEW.id,portfolio_search_text(NEW.data)); END;
