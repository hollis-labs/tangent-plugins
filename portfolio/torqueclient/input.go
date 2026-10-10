package torqueclient

import (
	"encoding/json"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

var idPattern = regexp.MustCompile(`^[A-Z]{2,6}-\d{8}-\d{3,6}$`)
var wordPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
var tagPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)
var cursorPattern = regexp.MustCompile(`^[A-Za-z0-9_=-]{1,512}$`)
var instantPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}(T\d{2}:\d{2}(:\d{2}(\.\d{1,9})?)?(Z|[+-]\d{2}:\d{2}))?$`)
var dimensions = []string{"status", "priority", "tags", "project_id", "epic_id", "sprint_id"}

func text(v any) string { s, _ := v.(string); return s }
func integer(v any) (int, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	if err != nil {
		f, e := n.Float64()
		if e != nil || math.IsNaN(f) || math.IsInf(f, 0) || math.Trunc(f) != f || f > 9007199254740991 || f < -9007199254740991 {
			return 0, false
		}
		i = int64(f)
	}
	if i > 9007199254740991 || i < -9007199254740991 || int64(int(i)) != i {
		return 0, false
	}
	return int(i), true
}
func contains(values []string, v string) bool {
	for _, s := range values {
		if s == v {
			return true
		}
	}
	return false
}
func words(v string, maximum int, pattern *regexp.Regexp) bool {
	parts := strings.Split(v, ",")
	if len(parts) > maximum {
		return false
	}
	for _, s := range parts {
		if !pattern.MatchString(s) {
			return false
		}
	}
	return true
}

type plan struct {
	path      string
	query     url.Values
	ids       []string
	input     map[string]any
	requested []Resource
}

func prepare(name string, input map[string]any) (plan, error) {
	b, err := json.Marshal(input)
	if err != nil || len(b) > 32<<10 {
		return plan{}, Invalid
	}
	v, err := decode(b)
	if err != nil {
		return plan{}, Invalid
	}
	in, ok := v.(map[string]any)
	if !ok {
		return plan{}, Invalid
	}
	p := plan{query: url.Values{}, input: in}
	allowed := []string{"project_id", "epic_id", "sprint_id", "status", "priority", "tags", "tags_any", "tags_none", "tag", "q"}
	switch name {
	case "torque_task":
		allowed = []string{"id"}
		p.path = "/tasks/" + text(in["id"])
	case "torque_titles":
		allowed = []string{"ids"}
		p.path = "/tasks"
	case "torque_tasks":
		p.path = "/tasks"
		allowed = append(allowed, "sort_by", "sort_dir", "limit", "offset", "cursor", "include_total", "updated_after", "updated_before")
	case "torque_facets":
		p.path = "/tasks/facets"
		allowed = append(allowed, "dimensions")
	case "torque_projects":
		p.path = "/projects"
		allowed = []string{"status"}
	case "torque_epics":
		p.path = "/epics"
		allowed = []string{"project_id", "status"}
	case "torque_sprints":
		p.path = "/sprints"
		allowed = []string{"project_id", "epic_id", "status"}
	default:
		return plan{}, Invalid
	}
	for k := range in {
		if !contains(allowed, k) {
			return plan{}, Invalid
		}
	}
	if name == "torque_task" {
		if !idPattern.MatchString(text(in["id"])) {
			return plan{}, Invalid
		}
		p.ids = []string{text(in["id"])}
		p.requested = []Resource{{Kind: "task", ID: p.ids[0]}}
		return p, nil
	}
	if name == "torque_titles" {
		ids, err := identifiers(in["ids"], 100)
		if err != nil {
			return plan{}, err
		}
		p.ids = ids
		b, _ := json.Marshal(ids)
		p.query.Set("ids", string(b))
		for _, id := range ids {
			p.requested = append(p.requested, Resource{Kind: "task", ID: id})
		}
		return p, nil
	}
	for k, v := range in {
		if v == nil || v == "" {
			continue
		}
		s, ok := v.(string)
		switch k {
		case "project_id", "epic_id", "sprint_id":
			if !ok || !idPattern.MatchString(s) {
				return plan{}, Invalid
			}
			p.query.Set(k, s)
		case "status":
			maximum := 1
			if name == "torque_tasks" || name == "torque_facets" {
				maximum = 12
			}
			if !ok || !words(s, maximum, wordPattern) {
				return plan{}, Invalid
			}
			p.query.Set(k, s)
		case "tags", "tags_any", "tags_none", "tag":
			if !ok || !words(s, 20, tagPattern) {
				return plan{}, Invalid
			}
			p.query.Set(k, s)
		case "q":
			if !ok || len(utf16.Encode([]rune(s))) > 200 {
				return plan{}, Invalid
			}
			p.query.Set("search", s)
		case "priority":
			n, valid := integer(v)
			if !valid || n < 0 || n > 9 {
				return plan{}, Invalid
			}
			p.query.Set(k, strconv.Itoa(n))
		case "sort_by":
			if !ok || !contains([]string{"priority", "status", "updated_at", "created_at", "title"}, s) {
				return plan{}, Invalid
			}
			p.query.Set(k, s)
		case "sort_dir":
			if s != "asc" && s != "desc" {
				return plan{}, Invalid
			}
			p.query.Set(k, s)
		case "limit", "offset":
			n, valid := integer(v)
			if !valid || k == "limit" && (n < 1 || n > 200) || k == "offset" && (n < 0 || n > 10000000) {
				return plan{}, Invalid
			}
			p.query.Set(k, strconv.Itoa(n))
		case "cursor":
			if !ok || !cursorPattern.MatchString(s) {
				return plan{}, Invalid
			}
			p.query.Set(k, s)
		case "include_total":
			flag, valid := v.(bool)
			if !valid {
				return plan{}, Invalid
			}
			if flag {
				p.query.Set(k, "true")
			}
		case "updated_after", "updated_before":
			if !ok || !instantPattern.MatchString(s) {
				return plan{}, Invalid
			}
			if len(s) == 10 {
				s += "T00:00:00Z"
			} else if len(s) > 16 && s[16] != ':' {
				s = s[:16] + ":00" + s[16:]
			}
			if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
				return plan{}, Invalid
			}
			p.query.Set(k, s)
		case "dimensions": // handled below
		}
	}
	switch name {
	case "torque_tasks":
		if p.query.Get("limit") == "" {
			p.query.Set("limit", "50")
		}
		if p.query.Get("cursor") != "" && p.query.Has("offset") {
			return plan{}, Invalid
		}
		p.query.Set("fields", "summary")
	case "torque_facets":
		dims := dimensions
		if raw, has := in["dimensions"]; has {
			a, ok := raw.([]any)
			if !ok || len(a) == 0 || len(a) > 6 {
				return plan{}, Invalid
			}
			dims = []string{}
			for _, v := range a {
				s, ok := v.(string)
				if !ok || !contains(dimensions, s) {
					return plan{}, Invalid
				}
				if !contains(dims, s) {
					dims = append(dims, s)
				}
			}
		}
		p.query.Set("dimensions", strings.Join(dims, ","))
	default:
		p.query.Set("limit", "200")
	}
	return p, nil
}

func identifiers(raw any, maximum int) ([]string, error) {
	a, ok := raw.([]any)
	if !ok || len(a) > 1000 {
		return nil, Invalid
	}
	out := []string{}
	seen := map[string]bool{}
	for _, v := range a {
		s, ok := v.(string)
		if !ok || !idPattern.MatchString(s) {
			return nil, Invalid
		}
		if !seen[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	if len(out) > maximum {
		return nil, Invalid
	}
	return out, nil
}
