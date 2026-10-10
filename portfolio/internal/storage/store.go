// Package storage owns the copied-snapshot database. It does not open the live
// Node data directory or provide authenticated portfolio write operations.
package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store serializes immediate transactions on a persistent, explicitly chosen file.
type Store struct{ db *sql.DB }

func init() {
	sqlite.MustRegisterDeterministicScalarFunction("portfolio_search_text", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		text, ok := args[0].(string)
		if !ok {
			return nil, errors.New("item JSON must be text")
		}
		obj, err := decodeObject([]byte(text))
		if err != nil {
			return nil, err
		}
		return searchText(obj), nil
	})
}

// Open refuses missing/nonabsolute persistence paths. Every connection enables
// foreign keys, FULL durability, a bounded busy timeout and immediate writes.
func Open(ctx context.Context, path string) (*Store, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("an absolute persistent database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	// Create privately before SQLite opens it; refuse nonregular existing state.
	// #nosec G304 -- explicit canonical shadow database path, never a caller-controlled live fallback.
	f, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if createErr == nil {
		if err := f.Close(); err != nil {
			return nil, err
		}
	} else if !errors.Is(createErr, os.ErrExist) {
		return nil, createErr
	}
	info, statErr := os.Lstat(path)
	if statErr != nil {
		return nil, statErr
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("database must be a regular file")
	}
	if err := os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("_txlock", "immediate")
	for _, p := range []string{"foreign_keys(1)", "busy_timeout(5000)", "synchronous(FULL)"} {
		q.Add("_pragma", p)
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err = s.initialize(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) initialize(ctx context.Context) error {
	var mode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return err
	}
	if mode != "wal" {
		return errors.New("WAL is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, digest TEXT NOT NULL)"); err != nil {
		return err
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		body, readErr := migrations.ReadFile("migrations/" + entry.Name())
		if readErr != nil {
			return readErr
		}
		version, parseErr := strconv.Atoi(strings.SplitN(entry.Name(), "_", 2)[0])
		if parseErr != nil {
			return parseErr
		}
		digest := hashBytes(body)
		var stored string
		err = tx.QueryRowContext(ctx, "SELECT digest FROM schema_migrations WHERE version=?", version).Scan(&stored)
		if err == nil {
			if stored != digest {
				return fmt.Errorf("migration %d changed", version)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err = tx.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("migration %d: %w", version, err)
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations VALUES (?,?)", version, digest); err != nil {
			return err
		}
	}
	var latest int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&latest); err != nil {
		return err
	}
	if latest != len(entries) {
		return errors.New("unsupported database migration version")
	}
	return tx.Commit()
}

// Close releases the store without deleting persistent state.
func (s *Store) Close() error { return s.db.Close() }
