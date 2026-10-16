// Package ptrack maps the tracker command surface onto an explicit API client.
// It contains no store, domain service, caller issuer or live installation.
package ptrack

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hollis-labs/tangent-plugins/portfolio/internal/projects"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/textcompat"
)

type object = map[string]any

type arguments struct {
	pos   []string
	flags map[string][]string
	child []string
}

func parse(args []string) (arguments, error) {
	a := arguments{flags: map[string][]string{}}
	for len(args) > 0 {
		token := args[0]
		args = args[1:]
		if token == "-h" {
			a.flags["help"] = []string{"true"}
			a.child = append(a.child, token)
			continue
		}
		if !strings.HasPrefix(token, "--") {
			a.pos = append(a.pos, token)
			a.child = append(a.child, token)
			continue
		}
		key := strings.TrimPrefix(token, "--")
		if key == "help" || key == "text" {
			a.flags[key] = []string{"true"}
			a.child = append(a.child, token)
			continue
		}
		if len(args) == 0 {
			return a, fail("bad_request", "--"+key+" needs a value")
		}
		value := args[0]
		args = args[1:]
		if key == "field" || key == "set" {
			a.flags[key] = append(a.flags[key], value)
		} else {
			a.flags[key] = []string{value}
		}
		if key != "config" && key != "backend" {
			a.child = append(a.child, token, value)
		}
	}
	return a, nil
}

func (a arguments) has(key string) bool { _, ok := a.flags[key]; return ok }
func (a arguments) flag(key string) string {
	values := a.flags[key]
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}
func (a arguments) help() bool { return a.has("help") || len(a.pos) == 0 || a.pos[0] == "help" }

func pairs(values []string) (object, error) {
	out := object{}
	for _, value := range values {
		key, raw, found := strings.Cut(value, "=")
		if !found || key == "" {
			return nil, fail("bad_request", "expected k=v, got: "+value)
		}
		parsed, err := decodeJSON([]byte(raw))
		if err != nil && json.Valid([]byte(raw)) {
			return nil, fail("bad_request", "field JSON must be unambiguous and within nesting bounds")
		}
		if err != nil {
			parsed = raw
		}
		out[key] = parsed
	}
	return out, nil
}

// JSON objects/arrays retain number tokens. Primitive spreading follows the
// legacy object-spread surface; string indices use UTF-16 via display helpers.
func spreadJSON(raw string) (object, error) {
	v, err := decodeJSON([]byte(raw))
	if err != nil {
		return nil, fail("bad_request", "--json is not valid JSON")
	}
	out := object{}
	switch value := v.(type) {
	case object:
		return value, nil
	case []any:
		for i, child := range value {
			out[strconv.Itoa(i)] = child
		}
	case string:
		for i, child := range stringUnits(value) {
			out[strconv.Itoa(i)] = child
		}
	}
	return out, nil
}

func endpoint(raw string) (object, error) {
	db, id, ok := strings.Cut(raw, ":")
	if !ok || db == "" {
		return nil, fail("bad_request", "expected <db:id>, got: "+raw)
	}
	return object{"db": db, "id": id}, nil
}

func merge(into, from object) {
	for key, value := range from {
		into[key] = value
	}
}
func putFlag(in object, key, flag string, a arguments) {
	if a.has(flag) {
		in[key] = a.flag(flag)
	}
}

var decimalNumberFlag = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

func numberFlag(raw string, digitsOnly bool) any {
	if digitsOnly {
		if raw == "" || strings.IndexFunc(raw, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return raw
		}
	}
	// Numeric flags have legacy Number semantics; data-bearing JSON uses raw tokens.
	text := textcompat.Trim(raw)
	if text == "" {
		return json.Number("0")
	}
	if len(text) > 2 && text[0] == '0' {
		base := 0
		switch text[1] {
		case 'x', 'X':
			base = 16
		case 'b', 'B':
			base = 2
		case 'o', 'O':
			base = 8
		}
		if base != 0 {
			digits := text[2:]
			for _, digit := range digits {
				value := strings.IndexRune("0123456789abcdef", digit)
				if digit >= 'A' && digit <= 'F' {
					value = int(digit-'A') + 10
				}
				if value < 0 || value >= base {
					return nil
				}
			}
			if integer, ok := new(big.Int).SetString(digits, base); ok {
				n, _ := new(big.Float).SetInt(integer).Float64()
				if !math.IsInf(n, 0) {
					return json.Number(strconv.FormatFloat(n, 'g', -1, 64))
				}
			}
			return nil
		}
	}
	if !decimalNumberFlag.MatchString(text) {
		return nil
	}
	n, err := strconv.ParseFloat(text, 64)
	if err != nil || strconv.FormatFloat(n, 'g', -1, 64) == "+Inf" || strconv.FormatFloat(n, 'g', -1, 64) == "-Inf" || n != n {
		return nil
	}
	if n == 0 {
		return json.Number("0")
	}
	return json.Number(strconv.FormatFloat(n, 'g', -1, 64))
}

func plan(a arguments) (string, object, error) {
	cmd, args := a.pos[0], a.pos[1:]
	in := object{}
	need := func(n int, usage string) error {
		if len(args) < n {
			return fail("bad_request", "usage: ptrack "+usage)
		}
		return nil
	}
	name := cmd
	switch cmd {
	case "databases", "contract", "migrate":
	case "board":
		if a.has("recent-hours") {
			in["recent_hours"] = numberFlag(a.flag("recent-hours"), false)
		}
		if a.has("limit") {
			in["limit"] = numberFlag(a.flag("limit"), true)
		}
	case "list", "schema", "get", "add", "update", "comment", "reorder":
		minimum := 1
		usage := cmd + " <db>"
		if cmd == "get" || cmd == "update" || cmd == "reorder" {
			minimum = 2
			usage += " <id>"
		}
		if cmd == "comment" {
			minimum = 3
			usage += " <id> \"text\""
		}
		if err := need(minimum, usage); err != nil {
			return "", nil, err
		}
		in["db"] = args[0]
		switch cmd {
		case "get":
			in["id"] = args[1]
		case "list":
			filters, err := pairs(a.flags["field"])
			if err != nil {
				return "", nil, err
			}
			if a.flag("status") != "" {
				if _, overridden := filters["status"]; !overridden {
					filters["status"] = a.flag("status")
				}
			}
			in["filters"] = filters
			putFlag(in, "sort", "sort", a)
		case "add", "update":
			key, repeated := "item", "field"
			if cmd == "update" {
				key, repeated = "patch", "set"
				in["id"] = args[1]
				if a.has("rev") {
					in["rev"] = numberFlag(a.flag("rev"), false)
				}
			} else {
				name = "create"
			}
			fields := object{}
			if cmd == "add" && a.flag("title") != "" {
				fields["title"] = a.flag("title")
			}
			parsed, err := pairs(a.flags[repeated])
			if err != nil {
				return "", nil, err
			}
			merge(fields, parsed)
			if a.has("json") {
				extra, err := spreadJSON(a.flag("json"))
				if err != nil {
					return "", nil, err
				}
				merge(fields, extra)
			}
			in[key] = fields
		case "comment":
			in["id"], in["text"] = args[1], strings.Join(args[2:], " ")
			putFlag(in, "kind", "kind", a)
			putFlag(in, "author", "as", a)
		case "reorder":
			in["ids"] = args[1:]
		}
	case "link", "unlink":
		if err := need(2, cmd+" <db:id> <db:id>"); err != nil {
			return "", nil, err
		}
		from, err := endpoint(args[0])
		if err != nil {
			return "", nil, err
		}
		to, err := endpoint(args[1])
		if err != nil {
			return "", nil, err
		}
		in["from"], in["to"] = from, to
		if a.has("field") {
			in["field"] = a.flags["field"][0]
		}
	case "link-add", "link-remove":
		if err := need(1, cmd+" <db:id>"); err != nil {
			return "", nil, err
		}
		ref, err := endpoint(args[0])
		if err != nil {
			return "", nil, err
		}
		merge(in, ref)
		name = strings.ReplaceAll(cmd, "-", "_")
		if cmd == "link-add" {
			link := object{}
			for _, key := range []string{"kind", "ref", "label"} {
				putFlag(link, key, key, a)
			}
			in["link"] = link
		} else {
			putFlag(in, "kind", "kind", a)
			putFlag(in, "ref", "ref", a)
		}
	case "decide", "defer", "reopen":
		if err := need(1, cmd+" <DEC-id>"); err != nil {
			return "", nil, err
		}
		in["id"] = args[0]
		if cmd == "decide" {
			putFlag(in, "option", "option", a)
			putFlag(in, "comment", "comment", a)
		}
		if cmd == "defer" {
			putFlag(in, "note", "note", a)
		}
		if cmd != "reopen" {
			putFlag(in, "author", "as", a)
		}
	case "search":
		if err := need(1, "search <q>"); err != nil {
			return "", nil, err
		}
		in["q"] = strings.Join(args, " ")
		putFlag(in, "db", "db", a)
	case "edge":
		if err := need(2, "edge list|add|remove|backlinks|decision-gates ..."); err != nil {
			return "", nil, err
		}
		sub := args[0]
		switch sub {
		case "list", "decision-gates":
			ref, err := endpoint(args[1])
			if err != nil {
				return "", nil, err
			}
			merge(in, ref)
			if sub == "list" {
				name = "edge_list"
				putFlag(in, "type", "type", a)
			} else {
				name = "decision_gates"
			}
		case "backlinks":
			name = "edge_backlinks"
			in["id"] = args[1]
			putFlag(in, "type", "type", a)
		case "add", "remove":
			if err := need(3, "edge "+sub+" <db:id> <target-id> --type TYPE"); err != nil {
				return "", nil, err
			}
			ref, err := endpoint(args[1])
			if err != nil {
				return "", nil, err
			}
			name = "edge_" + sub
			in["from"] = ref
			target := object{"id": args[2]}
			putFlag(target, "db", "to-db", a)
			in["to"] = target
			putFlag(in, "type", "type", a)
			if a.has("rev") {
				in["rev"] = numberFlag(a.flag("rev"), false)
			}
		default:
			return "", nil, fail("bad_request", "usage: ptrack edge list|add|remove|backlinks|decision-gates ...")
		}
	case "inbox":
		if err := need(2, "inbox add|promote|dismiss ..."); err != nil {
			return "", nil, err
		}
		sub := args[0]
		name = "inbox_" + sub
		switch sub {
		case "add":
			in["title"] = strings.Join(args[1:], " ")
			for _, key := range []string{"path", "kind", "body"} {
				putFlag(in, key, key, a)
			}
			putFlag(in, "added_by", "as", a)
		case "promote":
			in["id"] = args[1]
			putFlag(in, "to_db", "to", a)
			fields, err := pairs(a.flags["field"])
			if err != nil {
				return "", nil, err
			}
			in["fields"] = fields
		case "dismiss":
			in["id"] = args[1]
			putFlag(in, "note", "note", a)
			putFlag(in, "author", "as", a)
		default:
			return "", nil, fail("bad_request", "usage: ptrack inbox add|promote|dismiss ...")
		}
	case "torque":
		if err := need(1, "torque task|tasks|titles|projects|epics|sprints|facets ..."); err != nil {
			return "", nil, err
		}
		sub := args[0]
		name = "torque_" + sub
		switch sub {
		case "task", "titles":
			if err := need(2, "torque "+sub+" <id>"); err != nil {
				return "", nil, err
			}
			if sub == "task" {
				in["id"] = args[1]
			} else {
				in["ids"] = args[1:]
			}
		case "tasks", "projects", "epics", "sprints", "facets":
			putFlag(in, "status", "status", a)
			if sub != "projects" {
				putFlag(in, "project_id", "project", a)
			}
			if sub == "tasks" || sub == "sprints" || sub == "facets" {
				putFlag(in, "epic_id", "epic", a)
			}
			if sub == "tasks" || sub == "facets" {
				for _, key := range []string{"sprint", "tag", "q"} {
					target := key
					if key == "sprint" {
						target = "sprint_id"
					}
					putFlag(in, target, key, a)
				}
				if a.has("priority") {
					in["priority"] = numberFlag(a.flag("priority"), true)
				}
			}
			if sub == "tasks" {
				for target, flag := range map[string]string{"sort_by": "sort", "sort_dir": "dir", "cursor": "cursor"} {
					putFlag(in, target, flag, a)
				}
				for _, key := range []string{"limit", "offset"} {
					if a.has(key) {
						in[key] = numberFlag(a.flag(key), true)
					}
				}
			}
			if sub == "facets" {
				putFlag(in, "tags", "tags", a)
				if a.flag("dimensions") != "" {
					in["dimensions"] = strings.Split(a.flag("dimensions"), ",")
				}
			}
		default:
			return "", nil, fail("bad_request", "usage: ptrack torque task|tasks|titles|projects|epics|sprints|facets ...")
		}
	default:
		return "", nil, fail("bad_request", fmt.Sprintf("unknown command: %s. Try ptrack --help", cmd))
	}
	scope, err := selectedScope(a, name)
	if err != nil {
		return "", nil, err
	}
	if scope != nil {
		in["scope"] = scope
	}
	return name, in, nil
}

func selectedScope(a arguments, name string) (object, error) {
	projectFlag := "project"
	if strings.HasPrefix(name, "torque_") {
		projectFlag = "scope-project"
	} else if a.has("scope-project") {
		return nil, fail("unsupported", "--scope-project is only supported on Torque queries")
	}
	if a.has(projectFlag) && a.has("workstream") {
		return nil, fail("bad_request", "portfolio scope selectors are mutually exclusive")
	}
	if !a.has(projectFlag) && !a.has("workstream") {
		return nil, nil
	}
	switch name {
	case "list", "search", "board", "torque_tasks", "torque_projects", "torque_epics", "torque_sprints", "torque_facets":
	default:
		return nil, fail("unsupported", "portfolio scope is not supported for this command")
	}
	kind, id := "project", a.flag(projectFlag)
	if a.has("workstream") {
		kind, id = "workstream", a.flag("workstream")
	}
	valid := utf8.RuneCountInString(id) <= 512 && id != "" && utf8.ValidString(id)
	if kind == "project" {
		valid = valid && projects.ValidURN(id)
	} else {
		valid = valid && strings.HasPrefix(id, "WS-") && strings.TrimSpace(id) == id
	}
	if !valid {
		return nil, fail("bad_request", "invalid portfolio scope identity")
	}
	return object{"kind": kind, "id": id}, nil
}
