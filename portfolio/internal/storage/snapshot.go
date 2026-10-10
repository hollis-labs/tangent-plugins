package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var databaseNames = []string{"priorities", "workstreams", "roadmap", "ideas", "decisions", "risks", "inbox"}

// Snapshot is a validated, detached copy of all seven envelopes. Its internals
// are immutable to callers, so digest and projections cannot diverge.
type Snapshot struct {
	envelopes   map[string]map[string]any
	digest      string
	projections map[string][][]any
}

func hashBytes(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func encode(v any) string       { b, _ := json.Marshal(v); return string(b) } // Only validated JSON values enter the codec.

// ReadSnapshot reads an explicitly selected owned-copy directory; no default
// directory or environment fallback is provided. It never writes its input.
func ReadSnapshot(dir string) (*Snapshot, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("an absolute owned-copy directory is required")
	}
	files := make(map[string][]byte, len(databaseNames))
	for _, name := range databaseNames {
		path := filepath.Join(dir, name+".json")
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s must be a regular copy file", name)
		}
		// #nosec G304 -- fixed database basename below explicitly selected owned-copy directory.
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		body, readErr := io.ReadAll(io.LimitReader(f, 64*1024*1024+1))
		closeErr := f.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if len(body) > 64*1024*1024 {
			return nil, errors.New("snapshot file exceeds 64 MiB")
		}
		files[name] = body
	}
	return ParseSnapshot(files)
}

// ParseSnapshot validates the complete import before starting any write.
// Unknown item/envelope fields, absent/null values and original ordering survive.
func ParseSnapshot(files map[string][]byte) (*Snapshot, error) {
	if len(files) != len(databaseNames) {
		return nil, errors.New("all seven database files are required")
	}
	s := &Snapshot{envelopes: map[string]map[string]any{}, projections: map[string][][]any{}}
	taken := map[string]bool{}
	for _, db := range databaseNames {
		envelope, err := decodeObject(files[db])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", db, err)
		}
		if envelope["schema"] != "portfolio/"+db+"@1" {
			return nil, fmt.Errorf("%s: unsupported schema", db)
		}
		items, ok := envelope["items"].([]any)
		if !ok {
			return nil, fmt.Errorf("%s: items must be an array", db)
		}
		s.envelopes[db] = envelope
		meta := map[string]any{}
		for key, value := range envelope {
			if key != "items" {
				meta[key] = value
			}
		}
		s.projections["envelopes"] = append(s.projections["envelopes"], []any{db, encode(meta)})
		for ordinal, v := range items {
			item, ok := v.(map[string]any)
			if !ok {
				return nil, errors.New("item must be an object")
			}
			id, ok := item["id"].(string)
			if !ok || id == "" || taken[id] {
				return nil, fmt.Errorf("%s: missing or duplicate global id", db)
			}
			taken[id] = true
			title, ok := item["title"].(string)
			if !ok || title == "" {
				return nil, fmt.Errorf("%s: title required", id)
			}
			rev := int64(0)
			if v, exists := item["rev"]; exists {
				n, ok := v.(json.Number)
				if !ok {
					return nil, errors.New("rev must be a nonnegative integer")
				}
				rev, err = n.Int64()
				if err != nil || rev < 0 {
					return nil, errors.New("rev must be a nonnegative integer")
				}
			}
			var order any
			if v, exists := item["order"]; exists && v != nil {
				n, ok := v.(json.Number)
				if !ok {
					return nil, errors.New("order must be numeric")
				}
				order, err = n.Float64()
				if err != nil {
					return nil, err
				}
			}
			s.projections["id_reservations"] = append(s.projections["id_reservations"], []any{id, db})
			s.projections["items"] = append(s.projections["items"], []any{id, db, int64(ordinal), title, stringOrNil(item["status"]), rev, order, stringOrNil(item["created"]), stringOrNil(item["updated"]), stringOrNil(item["author"]), encode(item)})
			if err = s.children(id, item); err != nil {
				return nil, fmt.Errorf("%s: %w", id, err)
			}
			s.projections["items_fts"] = append(s.projections["items_fts"], []any{id, searchText(item)})
		}
	}
	for _, table := range projectionTables {
		rows := s.projections[table]
		sort.Slice(rows, func(i, j int) bool { return encode(rows[i]) < encode(rows[j]) })
		s.projections[table] = rows
	}
	s.digest = hashBytes([]byte(encode(s.envelopes)))
	return s, nil
}
func stringOrNil(v any) any {
	if s, ok := v.(string); ok {
		return s
	}
	return nil
}
func (s *Snapshot) children(id string, item map[string]any) error {
	edges := map[string]bool{}
	addEdge := func(target, typ, field string) {
		key := encode([]string{target, typ, field})
		if !edges[key] {
			edges[key] = true
			s.projections["edges"] = append(s.projections["edges"], []any{id, target, typ, field})
		}
	}
	for _, kind := range []string{"comments", "links"} {
		v, exists := item[kind]
		if !exists || v == nil {
			continue
		}
		list, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%s must be an array", kind)
		}
		seen := map[string]bool{}
		for n, v := range list {
			obj, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("%s entry must be an object", kind)
			}
			if kind == "comments" {
				cid, ok := obj["id"].(string)
				if !ok || cid == "" || seen[cid] {
					return errors.New("missing or duplicate comment id")
				}
				seen[cid] = true
				s.projections[kind] = append(s.projections[kind], []any{id, int64(n), cid, stringOrNil(obj["author"]), stringOrNil(obj["text"]), stringOrNil(obj["created"]), stringOrNil(obj["kind"]), encode(obj)})
			} else {
				k, ok := obj["kind"].(string)
				if !ok || k == "" {
					return errors.New("link kind required")
				}
				ref, ok := obj["ref"].(string)
				if !ok || ref == "" {
					return errors.New("link ref required")
				}
				s.projections[kind] = append(s.projections[kind], []any{id, int64(n), k, ref, stringOrNil(obj["label"]), encode(obj)})
				if k == "workstream" {
					addEdge(ref, "belongs_to", "links.workstream")
				}
			}
		}
	}
	for field, v := range item {
		if field != "depends_on" && field != "supersedes" && !strings.HasSuffix(field, "_ids") {
			continue
		}
		if v == nil {
			continue
		}
		targets, ok := v.([]any)
		if field == "supersedes" {
			if _, scalar := v.(string); scalar {
				targets = []any{v}
			} else {
				return errors.New("supersedes must be a string")
			}
		} else if !ok {
			return fmt.Errorf("%s must be an array", field)
		}
		seen := map[string]bool{}
		typ := legacyEdgeType(field)
		for _, v := range targets {
			target, ok := v.(string)
			if !ok || target == "" {
				return errors.New("edge target must be a nonempty string")
			}
			if seen[target] {
				continue
			}
			seen[target] = true
			addEdge(target, typ, field)
		}
	}
	metadata, err := relationshipMetadata(item)
	if err != nil {
		return err
	}
	if metadata != nil {
		for _, entry := range metadata["edges"].([]any) {
			edge := entry.(map[string]any)
			addEdge(edge["target"].(string), edge["type"].(string), relationshipField)
		}
	}
	return nil
}
func searchText(item map[string]any) string {
	keys := make([]string, 0, len(item))
	for k := range item {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{}
	for _, key := range keys {
		switch v := item[key].(type) {
		case string:
			parts = append(parts, v)
		case []any:
			for _, v := range v {
				switch x := v.(type) {
				case string:
					parts = append(parts, x)
				case map[string]any:
					for _, k := range []string{"text", "label"} {
						if str, ok := x[k].(string); ok {
							parts = append(parts, str)
						}
					}
				}
			}
		}
	}
	return strings.ToLower(strings.Join(parts, " "))
}

// Decode with number preservation and duplicate-key rejection instead of
// silently replacing data during migration.
func decodeObject(body []byte) (map[string]any, error) {
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	value, err := decodeValue(d)
	if err != nil {
		return nil, err
	}
	if _, err = d.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing JSON")
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("JSON object required")
	}
	return obj, nil
}
func decodeValue(d *json.Decoder) (any, error) {
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delim {
	case '{':
		obj := map[string]any{}
		for d.More() {
			token, err = d.Token()
			if err != nil {
				return nil, err
			}
			key, ok := token.(string)
			if !ok {
				return nil, errors.New("invalid object key")
			}
			if _, exists := obj[key]; exists {
				return nil, errors.New("duplicate JSON key")
			}
			v, readErr := decodeValue(d)
			if readErr != nil {
				return nil, readErr
			}
			obj[key] = v
		}
		if _, err = d.Token(); err != nil {
			return nil, err
		}
		return obj, nil
	case '[':
		list := []any{}
		for d.More() {
			v, readErr := decodeValue(d)
			if readErr != nil {
				return nil, readErr
			}
			list = append(list, v)
		}
		if _, err = d.Token(); err != nil {
			return nil, err
		}
		return list, nil
	}
	return nil, errors.New("unexpected JSON delimiter")
}
