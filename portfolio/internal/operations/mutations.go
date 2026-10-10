package operations

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/hollis-labs/tangent-plugins/portfolio/internal/textcompat"
)

var refField = regexp.MustCompile(`^(related_ids|depends_on|[a-z][a-z_]*_ids)$`)

func itemLink(x *execution, in object, add bool) (any, error) {
	field := "related_ids"
	if v, has := in["field"]; has {
		field = str(v)
	}
	if !refField.MatchString(field) {
		return nil, failure("bad_request", "field must be related_ids, depends_on or *_ids: "+field, nil)
	}
	from, to := in["from"].(object), in["to"].(object)
	db := str(from["db"])
	src, err := x.find(db, str(from["id"]))
	if err != nil {
		return nil, err
	}
	if _, err = x.find(str(to["db"]), str(to["id"])); err != nil {
		return nil, err
	}
	values, _ := src[field].([]any)
	if values == nil {
		values = []any{}
	}
	has := false
	for _, v := range values {
		has = has || v == to["id"]
	}
	if has == add {
		return src, nil
	}
	if add {
		src[field] = append(values, to["id"])
	} else {
		kept := []any{}
		for _, v := range values {
			if v != to["id"] {
				kept = append(kept, v)
			}
		}
		src[field] = kept
	}
	if err = x.bump(src); err != nil {
		return nil, err
	}
	if err = x.check(db, src); err != nil {
		return nil, err
	}
	x.touch(db)
	return src, nil
}

var torqueID = regexp.MustCompile(`^[A-Z]{2,6}-\d{8}-\d{3,6}$`)
var httpURL = regexp.MustCompile(`(?i)^https?://[^\s]+$`)

func cleanLink(db string, v object) (object, error) {
	errors := []problem{}
	bad := func(path, message string) { errors = append(errors, problem{"link." + path, message}) }
	defs := itemSchemas[db]["$defs"].(object)
	kinds := defs["link"].(object)["properties"].(object)["kind"].(object)["enum"].([]any)
	valid := false
	labels := []string{}
	for _, kind := range kinds {
		valid = valid || kind == v["kind"]
		labels = append(labels, str(kind))
	}
	if !valid {
		bad("kind", "must be one of "+strings.Join(labels, ", "))
	}
	ref := textcompat.Trim(str(v["ref"]))
	if ref == "" {
		bad("ref", "is required (a non-empty string)")
	} else if v["kind"] == "url" && (!httpURL.MatchString(ref) || strings.ContainsFunc(ref, textcompat.Space)) {
		bad("ref", "must be an http or https URL")
	} else if v["kind"] == "torque" && !torqueID.MatchString(ref) {
		bad("ref", "must be a Torque task id such as CW-20261003-0137")
	}
	if label, has := v["label"]; has {
		if _, ok := label.(string); !ok {
			bad("label", "must be string")
		}
	}
	for k := range v {
		if k != "kind" && k != "ref" && k != "label" {
			bad(k, "unknown field")
			break
		}
	}
	if len(errors) > 0 {
		return nil, failure("invalid", "link failed validation", object{"errors": errors})
	}
	out := object{"kind": v["kind"], "ref": ref}
	if label := textcompat.Trim(str(v["label"])); label != "" {
		out["label"] = label
	}
	return out, nil
}
func externalLink(x *execution, in object, add bool) (any, error) {
	db := str(in["db"])
	item, err := x.find(db, str(in["id"]))
	if err != nil {
		return nil, err
	}
	links, _ := item["links"].([]any)
	if links == nil {
		links = []any{}
	}
	if add {
		clean, cleanErr := cleanLink(db, in["link"].(object))
		if cleanErr != nil {
			return nil, cleanErr
		}
		for _, v := range links {
			l, _ := v.(object)
			if l["kind"] == clean["kind"] && l["ref"] == clean["ref"] {
				return nil, failure("conflict", str(item["id"])+" already has a "+str(clean["kind"])+" link to "+str(clean["ref"]), object{"current": clone(item)})
			}
		}
		item["links"] = append(links, clean)
	} else {
		kept := []any{}
		for _, v := range links {
			l, _ := v.(object)
			if l["kind"] != in["kind"] || l["ref"] != in["ref"] {
				kept = append(kept, v)
			}
		}
		if len(kept) == len(links) {
			return nil, failure("not_found", str(item["id"])+" has no "+str(in["kind"])+" link to "+str(in["ref"]), object{"kind": in["kind"], "ref": in["ref"]})
		}
		item["links"] = kept
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
func reorder(x *execution, in object) (any, error) {
	db := str(in["db"])
	if err := knownDB(db); err != nil {
		return nil, err
	}
	ids := in["ids"].([]any)
	seen := map[string]bool{}
	for _, v := range ids {
		id := str(v)
		if seen[id] {
			return nil, failure("bad_request", "ids contains duplicates", nil)
		}
		seen[id] = true
	}
	sequence := []object{}
	missing := []string{}
	for _, v := range ids {
		item, err := x.find(db, str(v))
		if err != nil {
			missing = append(missing, str(v))
		} else {
			sequence = append(sequence, item)
		}
	}
	if len(missing) > 0 {
		return nil, failure("not_found", "no such items: "+strings.Join(missing, ", "), object{"missing": missing})
	}
	rest := []object{}
	for _, item := range x.items(db) {
		if _, has := number(item["order"]); has && !seen[str(item["id"])] {
			rest = append(rest, item)
		}
	}
	sequence = append(sequence, ordered(rest)...)
	out := []any{}
	for n, item := range sequence {
		want := float64((n + 1) * 10)
		old, has := number(item["order"])
		if !has || old != want {
			item["order"] = json.Number(strconv.Itoa((n + 1) * 10))
			if err := x.bump(item); err != nil {
				return nil, err
			}
		}
		r := object{"id": item["id"], "order": item["order"]}
		if v, has := item["rev"]; has {
			r["rev"] = v
		}
		out = append(out, r)
	}
	x.touch(db)
	return out, nil
}
func decision(x *execution, in object, name string) (any, error) {
	item, err := x.find("decisions", str(in["id"]))
	if err != nil {
		return nil, err
	}
	switch name {
	case "decide":
		var chosen object
		options, _ := item["options"].([]any)
		ids := []any{}
		for _, v := range options {
			o, _ := v.(object)
			ids = append(ids, o["id"])
			if o["id"] == in["option"] {
				chosen = o
			}
		}
		if chosen == nil {
			return nil, failure("invalid", "option "+str(in["option"])+" does not exist on "+str(in["id"]), object{"options": ids})
		}
		item["selected_option"] = in["option"]
		item["status"] = "decided"
		item["decided_at"] = x.day()
		item["decided_by"] = x.who
		delete(item, "defer_note")
		if !truthy(item["decision"]) {
			item["decision"] = chosen["label"]
		}
		if !truthy(item["date"]) {
			item["date"] = x.day()
		}
		if text := str(in["comment"]); text != "" {
			if err = x.addComment(item, text, "decision"); err != nil {
				return nil, err
			}
		}
	case "defer":
		item["status"] = "deferred"
		if note := str(in["note"]); note != "" {
			item["defer_note"] = note
			if err = x.addComment(item, note, "comment"); err != nil {
				return nil, err
			}
		}
	case "reopen":
		item["status"] = "needs-decision"
		for _, key := range []string{"selected_option", "decided_at", "defer_note"} {
			delete(item, key)
		}
		item["decided_by"] = ""
	}
	if err = x.bump(item); err != nil {
		return nil, err
	}
	if err = x.check("decisions", item); err != nil {
		return nil, err
	}
	x.touch("decisions")
	return item, nil
}
func inboxAdd(x *execution, in object) (any, error) {
	item, err := x.build("inbox", in, nil)
	if err != nil {
		return nil, err
	}
	x.appendItem("inbox", item)
	return item, nil
}
func inboxPromote(x *execution, in object) (any, error) {
	db := str(in["to_db"])
	if err := knownDB(db); err != nil {
		return nil, err
	}
	if db == "inbox" {
		return nil, failure("bad_request", "cannot promote into the inbox", nil)
	}
	inbox, err := x.find("inbox", str(in["id"]))
	if err != nil {
		return nil, err
	}
	if inbox["status"] == "promoted" || inbox["status"] == "dismissed" {
		return nil, failure("conflict", str(in["id"])+" is already "+str(inbox["status"]), object{"current": clone(inbox)})
	}
	base := object{"title": inbox["title"], "related_ids": []any{in["id"]}}
	if truthy(inbox["body"]) {
		base["notes"] = inbox["body"]
	}
	if truthy(inbox["path"]) {
		base["links"] = []any{object{"kind": "file", "ref": inbox["path"]}}
	}
	if fields, has := in["fields"].(object); has {
		for k, v := range fields {
			base[k] = v
		}
	}
	extra := object{}
	switch db {
	case "ideas":
		extra["kind"] = "idea"
	case "risks":
		extra["kind"] = "risk"
		extra["severity"] = "medium"
	case "roadmap":
		extra["area"] = "process"
		extra["horizon"] = "someday"
	case "priorities":
		maxRank := float64(0)
		for _, item := range x.items(db) {
			if n, has := jsNumeric(item["rank"]); has && n > maxRank {
				maxRank = n
			}
		}
		extra["rank"] = json.Number(strconv.FormatFloat(maxRank+1, 'g', -1, 64))
	}
	item, err := x.build(db, base, extra)
	if err != nil {
		return nil, err
	}
	x.appendItem(db, item)
	inbox["status"] = "promoted"
	inbox["promoted_to"] = object{"db": db, "id": item["id"]}
	values, _ := inbox["related_ids"].([]any)
	out := []any{}
	seen := map[string]bool{}
	for _, v := range append(values, item["id"]) {
		key := jsonText(v)
		if !seen[key] {
			out = append(out, v)
			seen[key] = true
		}
	}
	inbox["related_ids"] = out
	if err = x.bump(inbox); err != nil {
		return nil, err
	}
	if err = x.check("inbox", inbox); err != nil {
		return nil, err
	}
	x.touch("inbox")
	return object{"inbox": inbox, "item": item}, nil
}
func inboxDismiss(x *execution, in object) (any, error) {
	item, err := x.find("inbox", str(in["id"]))
	if err != nil {
		return nil, err
	}
	item["status"] = "dismissed"
	if note := str(in["note"]); note != "" {
		if err = x.addComment(item, note, ""); err != nil {
			return nil, err
		}
	}
	if err = x.bump(item); err != nil {
		return nil, err
	}
	x.touch("inbox")
	return item, nil
}
func migrate(x *execution, _ object) (any, error) {
	changed := []string{}
	seeded := []string{}
	marked := map[string]bool{}
	mark := func(db string) {
		if !marked[db] {
			changed = append(changed, db)
			marked[db] = true
		}
	}
	for _, db := range dbNames {
		for _, item := range x.items(db) {
			if _, ok := integer(item["rev"]); !ok {
				item["rev"] = json.Number("1")
				mark(db)
			}
			if db == "decisions" && item["status"] == "active" {
				item["status"] = "decided"
				mark(db)
			}
		}
	}
	// A complete copied snapshot includes inbox. An empty existing envelope is
	// not an absent source file and must never receive historical inbox seeds.
	for _, v := range legacySeeds["decisions"] {
		seed := clone(v).(object)
		id := str(seed["id"])
		if db, exists := x.state.Reserved[id]; exists {
			if db != "decisions" {
				return nil, failure("conflict", "id already exists: "+id, object{"id": id})
			}
			continue
		}
		seed["rev"] = json.Number("1")
		seed["created"] = x.day()
		seed["updated"] = x.day()
		seed["date"] = x.day()
		seed["author"] = x.who
		if err := x.check("decisions", seed); err != nil {
			return nil, err
		}
		x.appendItem("decisions", seed)
		mark("decisions")
		seeded = append(seeded, id)
	}
	for _, item := range x.items("ideas") {
		if item["id"] != "ID-cliproxy-review" {
			continue
		}
		values, _ := item["related_ids"].([]any)
		has := false
		for _, v := range values {
			has = has || v == "DEC-017"
		}
		if truthy(item["options"]) || !has {
			delete(item, "options")
			if !has {
				item["related_ids"] = append(values, "DEC-017")
			}
			if err := x.bump(item); err != nil {
				return nil, err
			}
			mark("ideas")
		}
	}
	for _, db := range changed {
		x.touch(db)
	}
	return object{"changed": changed, "seeded": seeded}, nil
}
