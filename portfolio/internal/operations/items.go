package operations

import (
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/hollis-labs/tangent-plugins/portfolio/internal/textcompat"
)

var dbNames = []string{"priorities", "workstreams", "roadmap", "ideas", "decisions", "risks", "inbox"}
var prefixes = map[string]string{"priorities": "PR-", "workstreams": "WS-", "roadmap": "RM-", "ideas": "ID-", "decisions": "DEC-", "risks": "RK-", "inbox": "IN-"}
var defaults = map[string]object{
	"priorities": {"status": "queued"}, "workstreams": {"status": "planned"}, "roadmap": {"status": "idea"}, "ideas": {"status": "new"}, "decisions": {"status": "needs-decision"}, "risks": {"status": "open"}, "inbox": {"status": "new", "kind": "topic"},
}

func knownDB(db string) error {
	if _, ok := prefixes[db]; !ok {
		return failure("not_found", "unknown database: "+db, object{"databases": dbNames})
	}
	return nil
}
func (x *execution) items(db string) []object {
	out := []object{}
	for _, v := range x.state.Envelopes[db]["items"].([]any) {
		item := v.(object)
		if x.scope == nil || x.scope.matches(x.state.Membership.Resolve(str(item["id"]))) {
			out = append(out, item)
		}
	}
	return out
}
func (x *execution) find(db, id string) (object, error) {
	if err := knownDB(db); err != nil {
		return nil, err
	}
	for _, item := range x.items(db) {
		if item["id"] == id {
			return item, nil
		}
	}
	return nil, failure("not_found", "no item "+id, object{"id": id})
}
func (x *execution) touch(db string) { x.state.Envelopes[db]["updated"] = x.day() }
func (x *execution) bump(item object) error {
	rev, ok := integer(item["rev"])
	if !ok {
		rev = 0
	}
	if rev >= 9007199254740991 {
		return failure("invalid", "item revision exhausted", nil)
	}
	item["rev"] = json.Number(strconv.FormatInt(rev+1, 10))
	item["updated"] = x.day()
	return nil
}
func ordered(items []object) []object {
	out := append([]object{}, items...)
	sort.SliceStable(out, func(i, j int) bool {
		a, hasA := number(out[i]["order"])
		b, hasB := number(out[j]["order"])
		if hasA != hasB {
			return hasA
		}
		return hasA && a < b
	})
	return out
}
func databases(x *execution, _ object) (any, error) {
	out := []any{}
	for _, db := range dbNames {
		env := x.state.Envelopes[db]
		r := object{"name": db, "prefix": prefixes[db], "schema": env["schema"], "count": len(x.items(db))}
		if v, has := env["updated"]; has && x.scope == nil {
			r["updated"] = v
		}
		out = append(out, r)
	}
	if x.scope != nil {
		return object{"items": out, "scope": x.scope, "partial": false}, nil
	}
	return out, nil
}
func get(x *execution, in object) (any, error) { return x.find(str(in["db"]), str(in["id"])) }
func list(x *execution, in object) (any, error) {
	db := str(in["db"])
	if err := knownDB(db); err != nil {
		return nil, err
	}
	items := ordered(x.items(db))
	filters, _ := in["filters"].(object)
	out := []object{}
	for _, item := range items {
		match := true
		for k, want := range filters {
			v, has := item[k]
			if values, ok := v.([]any); ok {
				found := false
				for _, v := range values {
					if jsString(v) == jsString(want) {
						found = true
					}
				}
				match = match && found
			} else {
				match = match && has && jsString(v) == jsString(want)
			}
		}
		if match {
			out = append(out, item)
		}
	}
	if field := str(in["sort"]); field != "" {
		desc := strings.HasPrefix(field, "-")
		key := strings.TrimPrefix(field, "-")
		sort.SliceStable(out, func(i, j int) bool {
			a, b := out[i], out[j]
			if desc {
				a, b = b, a
			}
			av, ah := a[key]
			bv, bh := b[key]
			if !ah {
				return false
			}
			if !bh {
				return true
			}
			return jsLess(av, bv)
		})
	}
	return out, nil
}
func jsLess(a, b any) bool {
	if as, ok := a.(string); ok {
		if bs, ok := b.(string); ok {
			return textcompat.Less(as, bs)
		}
	}
	af, aok := jsNumeric(a)
	bf, bok := jsNumeric(b)
	return aok && bok && af < bf
}
func jsNumeric(v any) (float64, bool) {
	if v == nil {
		return 0, true
	}
	if b, ok := v.(bool); ok {
		if b {
			return 1, true
		}
		return 0, true
	}
	if s, ok := v.(string); ok {
		if textcompat.Trim(s) == "" {
			return 0, true
		}
		f, e := strconv.ParseFloat(textcompat.Trim(s), 64)
		return f, e == nil
	}
	return number(v)
}
func jsonText(v any) string { b, _ := json.Marshal(v); return string(b) }
func search(x *execution, in object) (any, error) {
	terms := textcompat.Terms(str(in["q"]))
	if len(terms) == 0 {
		return nil, failure("bad_request", "q is empty", nil)
	}
	// Search reads the Store's FTS projection; keep the database/file ordering and
	// compact result shape from the lossless state, not alphabetical id order.
	hits := x.state.SearchIDs(str(in["q"]))
	names := dbNames
	if db := str(in["db"]); db != "" {
		if err := knownDB(db); err != nil {
			return nil, err
		}
		names = []string{db}
	}
	out := []any{}
	for _, db := range names {
		for _, item := range x.items(db) {
			if hits[str(item["id"])] {
				r := object{"db": db, "id": item["id"], "title": item["title"]}
				if v, has := item["status"]; has {
					r["status"] = v
				}
				out = append(out, r)
			}
		}
	}
	return out, nil
}

var slugReplace = regexp.MustCompile(`[^a-z0-9]+`)

func (x *execution) allocate(db, title string) (string, error) {
	prefix := prefixes[db]
	if db == "workstreams" || db == "roadmap" || db == "ideas" {
		slug := strings.Trim(slugReplace.ReplaceAllString(textcompat.Lower(title), "-"), "-")
		if len(slug) > 48 {
			slug = strings.TrimRight(slug[:48], "-")
		}
		if slug == "" {
			slug = "item"
		}
		id := prefix + slug
		for n := 2; x.state.Reserved[id] != ""; n++ {
			id = prefix + slug + "-" + strconv.Itoa(n)
		}
		return id, nil
	}
	width, maxID := 3, int64(0)
	if db == "priorities" {
		width = 2
	}
	for id, reservedDB := range x.state.Reserved {
		if reservedDB != db || !strings.HasPrefix(id, prefix) {
			continue
		}
		tail := strings.TrimPrefix(id, prefix)
		if tail == "" {
			continue
		}
		digits := true
		for _, c := range tail {
			digits = digits && c >= '0' && c <= '9'
		}
		if !digits {
			continue
		}
		n, e := strconv.ParseInt(tail, 10, 64)
		if e != nil || n >= 9007199254740991 {
			return "", failure("invalid", "item id allocator exhausted", nil)
		}
		if n > maxID {
			maxID = n
		}
		if len(tail) > width {
			width = len(tail)
		}
	}
	tail := strconv.FormatInt(maxID+1, 10)
	if len(tail) < width {
		tail = strings.Repeat("0", width-len(tail)) + tail
	}
	id := prefix + tail
	if _, taken := x.state.Reserved[id]; taken {
		return "", failure("conflict", "id already exists: "+id, object{"id": id})
	}
	return id, nil
}
func truthy(v any) bool { return textcompat.Truthy(v) }
func (x *execution) build(db string, input, extra object) (object, error) {
	item := clone(input).(object)
	for k, v := range defaults[db] {
		if _, has := item[k]; !has {
			item[k] = v
		}
	}
	for k, v := range extra {
		if _, has := input[k]; !has {
			item[k] = v
		}
	}
	delete(item, "comments")
	id := str(item["id"])
	if truthy(item["id"]) {
		if _, has := x.state.Reserved[id]; has {
			return nil, failure("conflict", "id already exists: "+id, object{"id": id})
		}
	} else {
		if !truthy(item["title"]) {
			return nil, failure("invalid", "title is required", object{"errors": []problem{{"title", "is required"}}})
		}
		var err error
		id, err = x.allocate(db, jsString(item["title"]))
		if err != nil {
			return nil, err
		}
	}
	if !truthy(input["id"]) {
		item["id"] = id
	}
	item["rev"] = json.Number("1")
	if !truthy(item["created"]) {
		item["created"] = x.day()
	}
	item["updated"] = x.day()
	// Every newly supplied principal-valued field is owned by the verifier.
	for _, key := range []string{"author", "added_by", "decided_by"} {
		if _, has := item[key]; has {
			item[key] = x.who
		}
	}
	if db == "inbox" {
		item["added_by"] = x.who
	} else {
		item["author"] = x.who
	}
	if _, has := number(item["order"]); !has {
		maxOrder := math.Inf(-1)
		for _, i := range x.items(db) {
			if n, has := number(i["order"]); has && n > maxOrder {
				maxOrder = n
			}
		}
		if !math.IsInf(maxOrder, -1) {
			item["order"] = json.Number(strconv.FormatFloat(maxOrder+10, 'g', -1, 64))
		}
	}
	if err := x.check(db, item); err != nil {
		return nil, err
	}
	return item, nil
}
func (x *execution) appendItem(db string, item object) {
	env := x.state.Envelopes[db]
	env["items"] = append(env["items"].([]any), item)
	x.state.Reserved[str(item["id"])] = db
	x.touch(db)
}
func create(x *execution, in object) (any, error) {
	db := str(in["db"])
	if err := knownDB(db); err != nil {
		return nil, err
	}
	item, err := x.build(db, in["item"].(object), nil)
	if err != nil {
		return nil, err
	}
	x.appendItem(db, item)
	return item, nil
}
func update(x *execution, in object) (any, error) {
	db := str(in["db"])
	item, err := x.find(db, str(in["id"]))
	if err != nil {
		return nil, err
	}
	patch := in["patch"].(object)
	bad := []string{}
	for _, key := range []string{"id", "created", "comments", "rev"} {
		if _, has := patch[key]; has {
			bad = append(bad, key)
		}
	}
	if len(bad) > 0 {
		return nil, failure("bad_request", "cannot patch: "+strings.Join(bad, ", "), object{"fields": bad})
	}
	rev, hasRev := in["rev"]
	current, ok := integer(item["rev"])
	if !ok {
		current = 0
	}
	wanted, _ := integer(rev)
	if hasRev && wanted != current {
		return nil, failure("conflict", "rev mismatch: you sent "+jsString(rev)+", current is "+strconv.FormatInt(current, 10), object{"current": clone(item)})
	}
	for k, v := range patch {
		if k == "author" || k == "added_by" || k == "decided_by" {
			item[k] = x.who
		} else if v == nil {
			delete(item, k)
		} else {
			item[k] = v
		}
	}
	if err = x.bump(item); err != nil {
		return nil, err
	}
	if err = x.check(db, item); err != nil {
		return nil, err
	}
	x.touch(db)
	return item, nil
}

var commentID = regexp.MustCompile(`^c-(\d+)$`)

func (x *execution) addComment(item object, text, kind string) error {
	comments, _ := item["comments"].([]any)
	if comments == nil {
		comments = []any{}
	}
	n := int64(0)
	for _, v := range comments {
		c, _ := v.(object)
		match := commentID.FindStringSubmatch(str(c["id"]))
		if len(match) > 0 {
			i, e := strconv.ParseInt(match[1], 10, 64)
			if e != nil || i >= 9007199254740991 {
				return failure("invalid", "comment id allocator exhausted", nil)
			}
			if i > n {
				n = i
			}
		}
	}
	c := object{"id": "c-" + strconv.FormatInt(n+1, 10), "author": x.who, "text": text, "created": x.now.UTC().Format("2006-01-02T15:04:05.000Z")}
	if kind != "" {
		c["kind"] = kind
	}
	item["comments"] = append(comments, c)
	return nil
}
func comment(x *execution, in object) (any, error) {
	if textcompat.Trim(str(in["text"])) == "" {
		return nil, failure("invalid", "text is required", object{"errors": []problem{{"text", "is required"}}})
	}
	db := str(in["db"])
	item, err := x.find(db, str(in["id"]))
	if err != nil {
		return nil, err
	}
	if err = x.addComment(item, str(in["text"]), str(in["kind"])); err != nil {
		return nil, err
	}
	if err = x.bump(item); err != nil {
		return nil, err
	}
	x.touch(db)
	return item, nil
}
