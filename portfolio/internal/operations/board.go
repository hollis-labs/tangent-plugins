package operations

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

var boardSections = []string{"in_flight", "landed", "pre_flight", "holds"}
var boardStatuses = []string{"doing", "review", "queued", "done"}

type boardRow struct {
	value   object
	instant time.Time
	edge    time.Time
}

func parseDate(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02"} {
		t, e := time.Parse(layout, s)
		if e == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
func iso(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
func workstream(item object, torque bool) string {
	if torque {
		tags, _ := item["tags"].([]any)
		for _, v := range tags {
			tag := str(v)
			if m, ok := v.(object); ok {
				tag = str(m["slug"])
			}
			if strings.HasPrefix(tag, "ws-") && len(tag) > 3 {
				return tag[3:]
			}
		}
	}
	ids, _ := item["workstream_ids"].([]any)
	if len(ids) > 0 {
		return strings.TrimPrefix(jsString(ids[0]), "WS-")
	}
	return ""
}
func makeRow(item object, db, source string, now time.Time, fields ...string) boardRow {
	title := str(item["title"])
	if title == "" {
		title = str(item["id"])
	}
	out := object{"source": source, "id": item["id"], "title": title, "updated": ""}
	if v, has := item["status"]; has {
		out["status"] = v
	}
	if n, ok := integer(item["priority"]); ok && n > 0 {
		out["priority"] = item["priority"]
	}
	if ws := workstream(item, source == "torque"); ws != "" {
		out["workstream"] = ws
	}
	if db != "" {
		out["db"] = db
	}
	raw := ""
	for _, field := range fields {
		if s := str(item[field]); s != "" {
			raw = s
			break
		}
	}
	r := boardRow{value: out, instant: time.Unix(0, 0), edge: time.Unix(0, 0)}
	if t, ok := parseDate(raw); ok {
		t = t.Truncate(time.Millisecond)
		if t.After(now) {
			t = now
		}
		r.instant = t
		r.edge = t
		out["updated"] = iso(t)
		if len(raw) == 10 {
			end, err := time.Parse(time.RFC3339, raw+"T23:59:59Z")
			if err == nil {
				if end.After(now) {
					end = now
				}
				r.edge = end
			}
		}
	}
	return r
}
func newest(rows []boardRow) []boardRow {
	collation := collate.New(language.English)
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].instant.Equal(rows[j].instant) {
			return collation.CompareString(str(rows[i].value["id"]), str(rows[j].value["id"])) < 0
		}
		return rows[i].instant.After(rows[j].instant)
	})
	return rows
}
func objects(v any) []object {
	out := []object{}
	if a, ok := v.([]any); ok {
		for _, v := range a {
			if m, ok := v.(object); ok {
				out = append(out, m)
			}
		}
	}
	return out
}
func board(x *execution, in object) (any, error) {
	recent, active, limit := float64(72), float64(2), int64(30)
	if v, has := in["recent_hours"]; has {
		recent, _ = number(v)
	}
	if v, has := in["active_hours"]; has {
		active, _ = number(v)
	}
	if v, has := in["limit"]; has {
		limit, _ = integer(v)
	}
	if !(recent > 0 && recent <= 2160) {
		return nil, failure("bad_request", "recent_hours must be greater than 0 and at most 2160", nil)
	}
	if !(active > 0 && active <= 720) {
		return nil, failure("bad_request", "active_hours must be greater than 0 and at most 720", nil)
	}
	if limit < 1 || limit > 200 {
		return nil, failure("bad_request", "limit must be 1-200", nil)
	}
	firstFailure := "Torque unavailable"
	tasks := map[string][]object{}
	totals := map[string]int{}
	notices := []any{}
	scopeEvidence := object{}
	upstreamRefused := false
	after := x.now.Add(-time.Duration(recent * float64(time.Hour))).Truncate(time.Minute).UTC().Format("2006-01-02T15:04:05Z")
	for _, status := range boardStatuses {
		if upstreamRefused {
			scopeEvidence[status] = object{"partial": true, "code": "authority_refused"}
			continue
		}
		query := object{"status": status, "sort_by": "updated_at", "sort_dir": "desc", "limit": json.Number(fmt.Sprint(limit)), "include_total": true}
		if status == "done" {
			query["updated_after"] = after
		}
		got, err := x.upstream("torque_tasks", query)
		if err != nil {
			if x.scope != nil {
				scopeEvidence[status] = object{"partial": true, "code": "unavailable"}
				var domain *Error
				if upstreamAuthorityRefused(err) || errors.As(err, &domain) && domain.Details["invalidate_prior"] == true {
					upstreamRefused = true
					tasks = map[string][]object{}
					totals = map[string]int{}
				}
			}
			reason := safeUpstreamReason(err)
			if len(notices) == 0 {
				firstFailure = reason
			}
			notices = append(notices, "Torque "+status+" tasks unavailable: "+reason)
			continue
		}
		result, ok := got.(object)
		if !ok {
			notices = append(notices, "Torque "+status+" tasks unavailable: malformed response")
			continue
		}
		tasks[status] = objects(result["items"])
		meta, _ := result["meta"].(object)
		if x.scope != nil {
			scopeEvidence[status] = object{"meta": meta, "queries": result["evidence"]}
			if meta["partial"] == true {
				notices = append(notices, "Scoped Torque "+status+" tasks are partial; totals are lower bounds.")
			}
		}
		if n, ok := integer(meta["total"]); ok && n >= 0 && n <= 9007199254740991 {
			totals[status] = int(n)
		} else if x.scope != nil {
			if n, ok := integer(meta["lower_bound"]); ok && n >= 0 && n <= scopeMaxRows {
				totals[status] = int(n)
			}
		}
	}
	if len(notices) == len(boardStatuses) {
		notices = []any{"Torque is unavailable (" + firstFailure + "); showing tracker items only."}
	}
	activity := map[string]time.Time{}
	activityOK := true
	if doing, has := tasks["doing"]; has {
		for _, task := range doing {
			if x.scope != nil && (x.budget.pages >= scopeMaxPages || x.budget.rows >= scopeMaxRows || x.budget.bytes >= scopeMaxBytes) {
				activityOK = false
				break
			}
			if x.scope != nil {
				x.budget.pages++
			}
			got, err := x.upstream("torque_task", object{"id": task["id"]})
			if err != nil {
				activityOK = false
				if x.scope != nil && upstreamAuthorityRefused(err) {
					upstreamRefused = true
					tasks = map[string][]object{}
					totals = map[string]int{}
					break
				}
				continue
			}
			result, ok := got.(object)
			if !ok {
				activityOK = false
				continue
			}
			if x.scope != nil {
				raw, encodeErr := json.Marshal(result)
				comments, validComments := result["comments"].([]any)
				for _, value := range comments {
					comment, ok := value.(object)
					if !ok {
						validComments = false
						break
					}
					if _, ok := parseDate(str(comment["created_at"])); !ok {
						validComments = false
						break
					}
				}
				if encodeErr != nil || len(raw) > scopeMaxBytes-x.budget.bytes || len(comments) > scopeMaxRows-x.budget.rows {
					activityOK = false
					break
				}
				x.budget.bytes += len(raw)
				x.budget.rows += len(comments)
				meta, _ := result["comments_meta"].(object)
				if !validComments || meta["has_more"] != false {
					activityOK = false
				}
			}
			latest := ""
			for _, c := range objects(result["comments"]) {
				if s := str(c["created_at"]); s > latest {
					latest = s
				}
			}
			if t, ok := parseDate(latest); ok {
				activity[str(task["id"])] = t
			}
		}
		if !activityOK {
			notices = append(notices, "Torque comments unavailable; every doing task is shown as in flight.")
		}
	}
	out := composeBoard(x, recent, active, int(limit), tasks, totals, activity, activityOK, notices)
	if x.scope != nil {
		if upstreamRefused {
			scopeEvidence = object{}
			for _, status := range boardStatuses {
				scopeEvidence[status] = object{"partial": true, "code": "prior_results_invalidated"}
			}
		}
		scopeEvidence["activity"] = object{"complete": activityOK, "partial": !activityOK}
		out["scope"] = x.scope
		out["totals_kind"] = "selected_estimates"
		out["upstream"] = scopeEvidence
		membership := object{}
		unscoped := []any{}
		for _, section := range boardSections {
			values := out["sections"].(object)[section]
			for _, value := range values.([]any) {
				row := value.(object)
				id := str(row["id"])
				key := str(row["source"]) + ":" + id
				var m any
				if row["source"] == "torque" {
					for _, group := range tasks {
						for _, task := range group {
							if task["id"] == id {
								resolved := x.taskMembership(task)
								m = resolved
								if resolved.Unscoped() {
									unscoped = append(unscoped, key)
								}
							}
						}
					}
				} else {
					resolved := x.state.Membership.Resolve(id)
					m = resolved
					if resolved.Unscoped() {
						unscoped = append(unscoped, key)
					}
				}
				membership[key] = m
			}
		}
		out["membership"] = membership
		out["unscoped"] = object{"ids": unscoped, "returned": len(unscoped), "diagnostic_overlap": true}
	}
	return out, nil
}
func composeBoard(x *execution, recent, active float64, limit int, tasks map[string][]object, totals map[string]int, activity map[string]time.Time, activityOK bool, notices []any) object {
	torqueRows := func(status string) []boardRow {
		out := []boardRow{}
		for _, item := range tasks[status] {
			if item["status"] == status && truthy(item["id"]) {
				out = append(out, makeRow(item, "", "torque", x.now, "updated_at"))
			}
		}
		return out
	}
	doingAll := newest(torqueRows("doing"))
	doing, preparing := []boardRow{}, []boardRow{}
	cutoff := x.now.Add(-time.Duration(active * float64(time.Hour)))
	for _, r := range doingAll {
		last := r.instant
		if a := activity[str(r.value["id"])]; a.After(last) {
			last = a
		}
		if !activityOK || !last.Before(cutoff) {
			doing = append(doing, r)
		} else {
			preparing = append(preparing, r)
		}
	}
	review := newest(torqueRows("review"))
	queued := torqueRows("queued")
	newest(queued)
	sort.SliceStable(queued, func(i, j int) bool {
		rank := func(r boardRow) int64 {
			n, ok := integer(r.value["priority"])
			if !ok {
				return 99
			}
			return n
		}
		return rank(queued[i]) < rank(queued[j])
	})
	recentFrom := x.now.Add(-time.Duration(recent * float64(time.Hour)))
	roadmapFrom := x.now.Add(-7 * 24 * time.Hour)
	inWindow := func(r boardRow, from time.Time) bool { return str(r.value["updated"]) != "" && !r.edge.Before(from) }
	doneAll := torqueRows("done")
	done := []boardRow{}
	for _, r := range doneAll {
		if inWindow(r, recentFrom) {
			done = append(done, r)
		}
	}
	roadFlight, boarding, roadLanded := []boardRow{}, []boardRow{}, []boardRow{}
	plannedItems := []object{}
	for _, item := range x.items("roadmap") {
		r := makeRow(item, "roadmap", "roadmap", x.now, "updated", "created")
		switch item["status"] {
		case "in-progress":
			roadFlight = append(roadFlight, r)
		case "adopting":
			boarding = append(boarding, r)
		case "landed", "done":
			if inWindow(r, roadmapFrom) {
				roadLanded = append(roadLanded, r)
			}
		case "planned":
			if item["horizon"] == "now" || item["horizon"] == "next" {
				plannedItems = append(plannedItems, item)
			}
		}
	}
	sort.SliceStable(plannedItems, func(i, j int) bool {
		a, b := plannedItems[i], plannedItems[j]
		if a["horizon"] != b["horizon"] {
			return a["horizon"] == "now"
		}
		av, ah := a["order"]
		bv, bh := b["order"]
		if !ah || av == nil {
			av = json.Number("1000000000")
		}
		if !bh || bv == nil {
			bv = json.Number("1000000000")
		}
		return jsLess(av, bv)
	})
	planned := []boardRow{}
	for _, item := range plannedItems {
		planned = append(planned, makeRow(item, "roadmap", "roadmap", x.now, "updated", "created"))
	}
	decided, decisions, risks, inbox := []boardRow{}, []boardRow{}, []boardRow{}, []boardRow{}
	for _, item := range x.items("decisions") {
		if item["status"] == "decided" {
			r := makeRow(item, "decisions", "decision", x.now, "decided_at", "date", "updated")
			if inWindow(r, recentFrom) {
				decided = append(decided, r)
			}
		}
		if item["status"] == "needs-decision" {
			decisions = append(decisions, makeRow(item, "decisions", "decision", x.now, "updated", "created"))
		}
	}
	for _, item := range x.items("risks") {
		if item["status"] == "open" && item["severity"] == "high" {
			risks = append(risks, makeRow(item, "risks", "risk", x.now, "updated", "created"))
		}
	}
	for _, item := range x.items("inbox") {
		if item["status"] == "new" {
			inbox = append(inbox, makeRow(item, "inbox", "inbox", x.now, "updated", "created"))
		}
	}
	full := map[string][]boardRow{
		"in_flight":  append(append(doing, newest(roadFlight)...), review...),
		"landed":     newest(append(append(done, roadLanded...), decided...)),
		"pre_flight": append(append(append(preparing, queued...), newest(boarding)...), planned...),
		"holds":      append(append(newest(decisions), newest(risks)...), newest(inbox)...),
	}
	extra := func(status string, fetched int) int {
		if n := totals[status] - fetched; n > 0 {
			return n
		}
		return 0
	}
	totalsOut := object{"in_flight": len(full["in_flight"]) + extra("doing", len(doingAll)) + extra("review", len(review)), "landed": len(full["landed"]) + extra("done", len(doneAll)), "pre_flight": len(full["pre_flight"]) + extra("queued", len(queued)), "holds": len(full["holds"])}
	sections, counts := object{}, object{}
	for _, key := range boardSections {
		rows := full[key]
		if len(rows) > limit {
			rows = rows[:limit]
		}
		out := []any{}
		for _, r := range rows {
			out = append(out, r.value)
		}
		sections[key] = out
		counts[key] = len(out)
	}
	return object{"generated_at": iso(x.now), "recent_hours": recent, "limit": limit, "sections": sections, "counts": counts, "totals": totalsOut, "notices": notices}
}

func safeUpstreamReason(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Message
	}
	return "Torque unavailable"
}
