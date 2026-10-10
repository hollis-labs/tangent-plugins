// Package projects owns allowlisted read-only upstream project contracts.
package projects

import (
	"net/url"
	"regexp"
	"strings"
)

var projectID = regexp.MustCompile(`^PRJ-[0-9]{8}-[0-9]{4}$`)
var taskID = regexp.MustCompile(`^CW-[0-9]{8}-[0-9]{4}$`)

// ValidURN accepts canonical explicit project identities, never display labels.
func ValidURN(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "msg" && u.Host == "project" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.Path != "" && u.Path != "/" && !strings.ContainsAny(s, " \t\r\n") && u.String() == s
}

// ValidProjectID validates the public Torque project identifier form.
func ValidProjectID(s string) bool { return projectID.MatchString(s) }

// ExternalID preserves explicit mapping provenance, including unresolved IDs.
type ExternalID struct {
	Substrate  string `json:"substrate"`
	ExternalID string `json:"external_id"`
	AttachedAt string `json:"attached_at"`
}

// Project is deliberately smaller than a registry Profile; no operational payload.
type Project struct {
	URN         string       `json:"urn"`
	Kind        string       `json:"kind"`
	DisplayName string       `json:"display_name"`
	Title       string       `json:"title,omitempty"`
	Status      string       `json:"status"`
	Tags        []string     `json:"tags,omitempty"`
	ExternalIDs []ExternalID `json:"external_ids,omitempty"`
	Environment string       `json:"tether_instance_id"`
	CreatedAt   string       `json:"created_at"`
	UpdatedAt   string       `json:"updated_at"`
}

// TorqueIDs returns all distinct valid mappings; ambiguity is never collapsed.
func (p Project) TorqueIDs() []string {
	out := []string{}
	seen := map[string]bool{}
	for _, e := range p.ExternalIDs {
		if e.Substrate == "torque" && ValidProjectID(e.ExternalID) && !seen[e.ExternalID] {
			out = append(out, e.ExternalID)
			seen[e.ExternalID] = true
		}
	}
	return out
}

// MappingState surfaces unresolved, duplicate and ambiguous mapping provenance.
func (p Project) MappingState() string {
	seen := map[string]bool{}
	duplicate, invalid := false, false
	for _, e := range p.ExternalIDs {
		if e.Substrate != "torque" {
			continue
		}
		if !ValidProjectID(e.ExternalID) {
			invalid = true
		}
		if seen[e.ExternalID] {
			duplicate = true
		}
		seen[e.ExternalID] = true
	}
	if len(p.TorqueIDs()) > 1 {
		return "ambiguous"
	}
	if invalid {
		return "invalid"
	}
	if duplicate {
		return "duplicate"
	}
	if len(p.TorqueIDs()) == 0 {
		return "missing"
	}
	return "explicit"
}

// TorqueProject enriches a registry identity without replacing it.
type TorqueProject struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	UpdatedAt string `json:"updated_at"`
}

// Task is a synthetic read-only task, never a local portfolio item.
type Task struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	UpdatedAt string `json:"updated_at"`
	Tags      []struct {
		Slug string `json:"slug"`
	} `json:"tags"`
}

// Evidence retains page completeness without claiming a capped total is exact.
type Evidence struct {
	Complete   bool   `json:"complete"`
	Partial    bool   `json:"partial"`
	NextCursor string `json:"next_cursor,omitempty"`
	Total      *int   `json:"total,omitempty"`
	Pages      int    `json:"pages"`
	Code       string `json:"code,omitempty"`
}

// Directory is one bounded registry result. The pinned registry has no paging.
type Directory struct {
	Projects []Project
	Evidence Evidence
}

// TaskPage carries filtered synthetic upstream tasks and pagination provenance.
type TaskPage struct {
	Tasks    []Task
	Evidence Evidence
}
