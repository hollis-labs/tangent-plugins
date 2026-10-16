package ptrack

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/hollis-labs/tangent-plugins/portfolio/internal/textcompat"
)

func units(s string) int { return len(utf16.Encode([]rune(s))) }
func sliceUnits(s string, n int) string {
	v := utf16.Encode([]rune(s))
	if len(v) <= n {
		return s
	}
	return string(utf16.Decode(v[:n]))
}
func stringUnits(s string) []any {
	out := []any{}
	for _, v := range utf16.Encode([]rune(s)) {
		if v >= 0xd800 && v <= 0xdfff {
			out = append(out, json.RawMessage(fmt.Sprintf(`"\u%04x"`, v)))
		} else {
			out = append(out, string(rune(v)))
		}
	}
	return out
}
func pad(s string, n int) string { return s + strings.Repeat(" ", max(0, n-units(s))) }
func cut(s string, n int) string {
	if units(s) <= n {
		return s
	}
	return sliceUnits(s, n-1) + "…"
}

func writeResult(out io.Writer, name string, raw json.RawMessage, text bool) error {
	if len(raw) > maxOutput {
		return fail("unavailable", "plugin response exceeds 1 MiB")
	}
	v, err := decodeJSON(raw)
	if err != nil {
		return fail("unavailable", "plugin returned invalid JSON")
	}
	if text && name == "board" {
		board, ok := v.(object)
		if !ok {
			return fail("unavailable", "plugin returned an invalid board")
		}
		rendered, renderErr := boardText(board)
		if renderErr != nil {
			return renderErr
		}
		return writeOutput(out, rendered+"\n")
	}
	if rows, ok := v.([]any); ok && text {
		tabular := true
		for _, value := range rows {
			row, ok := value.(object)
			if !ok {
				tabular = false
				break
			}
			if _, ok = row["id"]; !ok {
				tabular = false
				break
			}
		}
		if tabular {
			return writeOutput(out, table(rows)+"\n")
		}
	}
	var buffer bytes.Buffer
	if text {
		err = json.Indent(&buffer, raw, "", "  ")
	} else {
		err = json.Compact(&buffer, raw)
	}
	if err != nil {
		return fail("unavailable", "plugin returned invalid JSON")
	}
	return writeOutput(out, buffer.String()+"\n")
}

func writeOutput(out io.Writer, text string) error {
	n, err := io.WriteString(out, text)
	if err == nil && n != len(text) {
		return io.ErrShortWrite
	}
	return err
}

func table(rows []any) string {
	cols := []string{"id", "status", "priority", "order", "rev", "title"}
	widths := make([]int, len(cols))
	for i, col := range cols {
		widths[i] = units(col)
	}
	data := [][]string{cols}
	for _, value := range rows {
		row := value.(object)
		cells := make([]string, len(cols))
		for i, col := range cols {
			if v, ok := row[col]; ok {
				cells[i] = textcompat.String(v)
			}
			if col == "title" {
				cells[i] = sliceUnits(cells[i], 70)
			}
			widths[i] = max(widths[i], units(cells[i]))
		}
		data = append(data, cells)
	}
	lines := []string{}
	for _, row := range data {
		padded := make([]string, len(row))
		for i, cell := range row {
			padded[i] = pad(cell, widths[i])
		}
		lines = append(lines, strings.TrimRight(strings.Join(padded, "  "), " "))
	}
	return strings.Join(lines, "\n")
}

var sections = []string{"in_flight", "landed", "pre_flight", "holds"}
var sectionTitles = map[string]string{"in_flight": "IN FLIGHT", "landed": "RECENTLY LANDED", "pre_flight": "PRE-FLIGHT", "holds": "HOLDS (WAITING ON YOU)"}
var statusWords = map[string]string{"doing": "IN FLIGHT", "review": "ARRIVING", "done": "LANDED", "landed": "LANDED", "decided": "LANDED", "queued": "QUEUED", "planned": "QUEUED", "in-progress": "IN FLIGHT", "adopting": "BOARDING"}
var holdWords = map[string]string{"decision": "DECIDE", "inbox": "TRIAGE", "risk": "RISK"}

func boardText(board object) (string, error) {
	group, ok := board["sections"].(object)
	if !ok {
		return "", fail("unavailable", "plugin returned invalid board sections")
	}
	generated, ok := board["generated_at"].(string)
	if !ok {
		return "", fail("unavailable", "plugin returned invalid board timestamp")
	}
	now, _ := parseDate(generated)
	totals, _ := board["totals"].(object)
	idWidth := 16
	rowsBySection := map[string][]object{}
	for _, section := range sections {
		values, ok := group[section].([]any)
		if !ok {
			return "", fail("unavailable", "plugin returned invalid board rows")
		}
		for _, value := range values {
			row, ok := value.(object)
			if !ok {
				return "", fail("unavailable", "plugin returned invalid board row")
			}
			rowsBySection[section] = append(rowsBySection[section], row)
			idWidth = max(idWidth, units(textcompat.String(row["id"])))
		}
	}
	idWidth = min(30, idWidth)
	lines := []string{"DEPARTURES  " + generated}
	for _, section := range sections {
		rows := rowsBySection[section]
		more := ""
		if total, ok := totals[section].(json.Number); ok {
			if n, err := strconv.ParseFloat(string(total), 64); err == nil && n > float64(len(rows)) {
				more = " (+" + strconv.FormatFloat(n-float64(len(rows)), 'f', -1, 64) + " more)"
			}
		}
		lines = append(lines, "", sectionTitles[section]+"  "+strconv.Itoa(len(rows))+more)
		if len(rows) == 0 {
			empty := "NOTHING"
			if section == "pre_flight" {
				empty = "NOTHING QUEUED"
			}
			lines = append(lines, "  "+empty)
		}
		for _, row := range rows {
			status, _ := row["status"].(string)
			word := statusWords[status]
			if word == "" {
				word = strings.ToUpper(status)
			}
			if section == "holds" {
				source, _ := row["source"].(string)
				word = holdWords[source]
				if word == "" {
					word = "HOLD"
				}
			} else if section == "pre_flight" && status == "doing" {
				word = "PREPARING"
			}
			title, _ := row["title"].(string)
			workstream, _ := row["workstream"].(string)
			updated, _ := row["updated"].(string)
			age := boardAge(now, updated)
			line := "  " + pad(word, 9) + " " + pad(textcompat.String(row["id"]), idWidth) + " " + pad(cut(title, 56), 56) + " " + pad(cut(workstream, 18), 18) + " " + strings.Repeat(" ", max(0, 4-units(age))) + age
			lines = append(lines, strings.TrimRight(line, " "))
		}
	}
	if notices, ok := board["notices"].([]any); ok {
		for _, notice := range notices {
			lines = append(lines, "", "NOTICE  "+textcompat.String(notice))
		}
	}
	return strings.Join(lines, "\n"), nil
}
func parseDate(s string) (time.Time, bool) {
	for _, format := range []string{time.RFC3339Nano, "2006-01-02"} {
		t, err := time.Parse(format, s)
		if err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
func boardAge(now time.Time, updated string) string {
	t, ok := parseDate(updated)
	if !ok || now.IsZero() {
		return ""
	}
	minutes := math.Max(0, math.Floor(now.Sub(t).Minutes()+0.5))
	if minutes < 60 {
		return strconv.FormatFloat(minutes, 'f', 0, 64) + "m"
	}
	if minutes < 2880 {
		return strconv.FormatFloat(math.Floor(minutes/60+0.5), 'f', 0, 64) + "h"
	}
	return strconv.FormatFloat(math.Floor(minutes/1440+0.5), 'f', 0, 64) + "d"
}
