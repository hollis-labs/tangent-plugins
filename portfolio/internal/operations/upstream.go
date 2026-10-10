package operations

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf16"
)

var filterWord = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
var filterWords = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}(,[a-z][a-z0-9_-]{0,31}){0,11}$`)
var filterTags = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)
var filterCursor = regexp.MustCompile(`^[A-Za-z0-9_=-]{1,512}$`)
var filterInstant = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}(T\d{2}:\d{2}(:\d{2}(\.\d{1,9})?)?(Z|[+-]\d{2}:\d{2}))?$`)
var facetDimensions = []string{"status", "priority", "tags", "project_id", "epic_id", "sprint_id"}

func upstreamInput(name string, in object) (object, error) {
	input := clone(in).(object)
	bad := func(message string) (object, error) { return nil, failure("bad_request", message, nil) }
	checkID := func(id any, label string) error {
		if !torqueID.MatchString(str(id)) {
			return failure("bad_request", label+" is not a Torque id (expected like CW-20261003-0137): "+jsString(id), nil)
		}
		return nil
	}
	if name == "torque_task" {
		return input, checkID(in["id"], "id")
	}
	if name == "torque_titles" {
		values := in["ids"].([]any)
		seen := map[string]bool{}
		for _, v := range values {
			seen[str(v)] = true
		}
		if len(seen) > 100 {
			return nil, failure("bad_request", fmt.Sprintf("at most 100 ids per call (got %d)", len(seen)), object{"max": 100})
		}
		for _, v := range values {
			if err := checkID(v, "id"); err != nil {
				return nil, err
			}
		}
		return input, nil
	}
	allowed := []string{"project_id", "epic_id", "sprint_id", "status", "priority", "tags", "tags_any", "tags_none", "tag", "q"}
	switch name {
	case "torque_tasks":
		allowed = append(allowed, "sort_by", "sort_dir", "limit", "offset", "cursor", "include_total", "updated_after", "updated_before")
	case "torque_facets":
		allowed = append(allowed, "dimensions")
	case "torque_projects":
		allowed = []string{"status"}
	case "torque_epics":
		allowed = []string{"project_id", "status"}
	case "torque_sprints":
		allowed = []string{"project_id", "epic_id", "status"}
	}
	for key := range in {
		found := false
		for _, v := range allowed {
			found = found || v == key
		}
		if !found {
			return nil, failure("bad_request", "unknown filter: "+key, object{"allowed": allowed})
		}
	}
	has := func(key string) bool { v, ok := in[key]; return ok && v != nil && v != "" }
	for _, key := range []string{"project_id", "epic_id", "sprint_id"} {
		if has(key) {
			if err := checkID(in[key], key); err != nil {
				return nil, err
			}
		}
	}
	if has("status") {
		valid := filterWords.MatchString(str(in["status"]))
		if name == "torque_projects" || name == "torque_epics" || name == "torque_sprints" {
			valid = filterWord.MatchString(str(in["status"]))
		}
		if !valid {
			return bad("status is malformed")
		}
	}
	if has("priority") {
		n, ok := integer(in["priority"])
		if !ok || n < 0 || n > 9 {
			return bad("priority must be an integer 0-9")
		}
	}
	for _, key := range []string{"tags", "tags_any", "tags_none", "tag"} {
		if has(key) && !validTags(str(in[key])) {
			return bad(key + " is malformed")
		}
	}
	if has("q") && len(utf16.Encode([]rune(str(in["q"])))) > 200 {
		return bad("q is too long (max 200)")
	}
	if name == "torque_tasks" {
		for _, key := range []string{"updated_after", "updated_before"} {
			if has(key) {
				s := str(in[key])
				if !filterInstant.MatchString(s) {
					return bad(key + " must be a date or an RFC 3339 time")
				}
				if len(s) == 10 {
					s += "T00:00:00Z"
				}
				if len(s) >= 17 && s[16] != ':' {
					s = s[:16] + ":00" + s[16:]
				}
				if _, ok := parseDate(s); !ok {
					return bad(key + " must be a date or an RFC 3339 time")
				}
				input[key] = s
			}
		}
		if has("sort_by") && !filterWord.MatchString(str(in["sort_by"])) {
			return bad("sort_by is malformed")
		}
		if has("sort_dir") && in["sort_dir"] != "asc" && in["sort_dir"] != "desc" {
			return bad("sort_dir must be asc or desc")
		}
		if has("limit") {
			n, ok := integer(in["limit"])
			if !ok || n < 1 || n > 200 {
				return bad("limit must be 1-200")
			}
		}
		if has("offset") {
			n, ok := integer(in["offset"])
			if !ok || n < 0 || n > 10000000 {
				return bad("offset must be a non-negative integer")
			}
		}
		if has("cursor") && !filterCursor.MatchString(str(in["cursor"])) {
			return bad("cursor is malformed")
		}
	}
	if name == "torque_facets" {
		if dims, has := in["dimensions"].([]any); has {
			if len(dims) == 0 {
				return bad("dimensions must be a non-empty subset of: " + strings.Join(facetDimensions, ", "))
			}
			for _, d := range dims {
				found := false
				for _, key := range facetDimensions {
					found = found || d == key
				}
				if !found {
					return bad("dimensions must be a non-empty subset of: " + strings.Join(facetDimensions, ", "))
				}
			}
		}
	}
	return input, nil
}

func validTags(s string) bool {
	tags := strings.Split(s, ",")
	if len(tags) > 20 {
		return false
	}
	for _, tag := range tags {
		if !filterTags.MatchString(tag) {
			return false
		}
	}
	return true
}
