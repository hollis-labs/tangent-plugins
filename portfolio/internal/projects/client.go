package projects

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Failure is safe to persist; upstream bodies, credentials and config are omitted.
type Failure string

func (e Failure) Error() string { return string(e) }

const (
	Missing     Failure = "missing_integration"
	Invalid     Failure = "invalid_integration"
	Denied      Failure = "denied"
	Unavailable Failure = "unavailable"
	Malformed   Failure = "malformed"
	TooLarge    Failure = "too_large"
	NotFound    Failure = "not_found"
)

// SecretResolver is an injected credential-reference seam. No ambient acquisition.
type SecretResolver func(context.Context, string) (string, error)

// Config selects an explicit endpoint and credential reference. Transport is injected
// for private fixtures or an explicitly configured UDS; it must honor request contexts.
type Config struct {
	Endpoint  string
	SecretRef string
	// Epoch changes whenever the selected credential reference is replaced.
	Epoch     string
	Resolve   SecretResolver
	Transport http.RoundTripper
	Timeout   time.Duration
	MaxBody   int64
	MaxPages  int
}

// Client has no writer methods or automatic token provisioning.
type Client struct {
	origin    *url.URL
	ref       string
	resolve   SecretResolver
	http      *http.Client
	timeout   time.Duration
	maxBody   int64
	maxPages  int
	partition string
}

// New refuses insecure bearer TCP, URL credentials, and implicit configuration.
func New(c Config) (*Client, error) {
	if c.Endpoint == "" || c.SecretRef == "" || c.Epoch == "" || c.Resolve == nil {
		return nil, Missing
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" || u.Host == "" {
		return nil, Invalid
	}
	ip := net.ParseIP(u.Hostname())
	loopback := u.Hostname() == "localhost" || ip != nil && ip.IsLoopback()
	if u.Scheme != "https" && (u.Scheme != "http" || !loopback) {
		return nil, Invalid
	}
	if c.Timeout == 0 {
		c.Timeout = 4 * time.Second
	}
	if c.MaxBody == 0 {
		c.MaxBody = 8 << 20
	}
	if c.MaxPages == 0 {
		c.MaxPages = 5
	}
	if c.Timeout < 0 || c.Timeout > 30*time.Second || c.MaxBody < 1 || c.MaxBody > 8<<20 || c.MaxPages < 1 || c.MaxPages > 5 {
		return nil, Invalid
	}
	transport := c.Transport
	if transport == nil {
		// An ambient HTTP proxy is not an explicitly selected credential target.
		base := http.DefaultTransport.(*http.Transport).Clone()
		base.Proxy = nil
		transport = base
	}
	partition := sha256.Sum256([]byte(c.Endpoint + "\x00" + c.SecretRef + "\x00" + c.Epoch))
	return &Client{origin: u, ref: c.SecretRef, resolve: c.Resolve, http: &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}, timeout: c.Timeout, maxBody: c.MaxBody, maxPages: c.MaxPages, partition: hex.EncodeToString(partition[:])}, nil
}

// ScopeKey partitions safe metadata by explicit endpoint/reference epoch; it
// contains neither raw configuration nor credential bytes.
func (c *Client) ScopeKey() string {
	if c == nil {
		return ""
	}
	return c.partition
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	if c == nil {
		return Missing
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	token, err := c.resolve(ctx, c.ref)
	if err != nil || token == "" || len(token) > 16384 || strings.ContainsAny(token, " \t\r\n\x00") {
		return Missing
	}
	u := *c.origin
	u.Path, err = url.PathUnescape(path)
	if err != nil {
		return Invalid
	}
	u.RawPath = path
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Invalid
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return Unavailable
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case 401, 403:
		return Denied
	case 404:
		return NotFound
	}
	if resp.StatusCode != 200 {
		return Unavailable
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody+1))
	if err != nil {
		return Unavailable
	}
	if int64(len(b)) > c.maxBody {
		return TooLarge
	}
	d := json.NewDecoder(bytes.NewReader(b))
	if err = d.Decode(out); err != nil {
		return Malformed
	}
	if err = d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return Malformed
	}
	return nil
}

// RegistryProject reads an explicit encoded URN using the same allowlist.
func (c *Client) RegistryProject(ctx context.Context, urn string) (Project, error) {
	if !ValidURN(urn) {
		return Project{}, Invalid
	}
	var p Project
	if err := c.get(ctx, "/registry/projects/"+url.PathEscape(urn), url.Values{"include": {"external_ids"}}, &p); err != nil {
		return Project{}, err
	}
	if p.URN != urn || p.Kind != "project" {
		return Project{}, Malformed
	}
	return p, nil
}

// Directory reads only the explicit external_ids include selector. The pinned
// route defaults to active projects, so absence means absent from this directory.
func (c *Client) Directory(ctx context.Context) (Directory, error) {
	var wire struct {
		Projects *[]Project `json:"projects"`
		Meta     *struct {
			Partial    bool   `json:"partial"`
			HasMore    bool   `json:"has_more"`
			NextCursor string `json:"next_cursor"`
			Total      *int   `json:"total"`
		} `json:"meta"`
	}
	err := c.get(ctx, "/registry/projects", url.Values{"include": {"external_ids"}}, &wire)
	if err != nil {
		return Directory{}, err
	}
	if wire.Projects == nil {
		return Directory{}, Malformed
	}
	seen := map[string]bool{}
	for _, p := range *wire.Projects {
		if !ValidURN(p.URN) || p.Kind != "project" || seen[p.URN] {
			return Directory{}, Malformed
		}
		seen[p.URN] = true
	}
	if len(*wire.Projects) > 1000 {
		return Directory{Evidence: Evidence{Partial: true, Code: "truncated", Pages: 1}}, TooLarge
	}
	evidence := Evidence{Complete: true, Pages: 1}
	if wire.Meta != nil {
		evidence.Partial = wire.Meta.Partial || wire.Meta.HasMore
		evidence.Complete = !evidence.Partial
		evidence.NextCursor = wire.Meta.NextCursor
		evidence.Total = wire.Meta.Total
	}
	return Directory{Projects: *wire.Projects, Evidence: evidence}, nil
}

// Project reads one explicit Torque ID; there is no name-based rebinding.
func (c *Client) Project(ctx context.Context, id string) (TorqueProject, error) {
	if !ValidProjectID(id) {
		return TorqueProject{}, Invalid
	}
	var p TorqueProject
	if err := c.get(ctx, "/api/v1/projects/"+id, nil, &p); err != nil {
		return p, err
	}
	if p.ID != id {
		return TorqueProject{}, Malformed
	}
	return p, nil
}

// Tasks filters at the upstream before page budgets and totals. Empty selectors
// refuse rather than widening to all tasks. Each result is checked against scope.
func (c *Client) Tasks(ctx context.Context, project, tag string) (TaskPage, error) {
	if c == nil {
		return TaskPage{}, Missing
	}
	if project == "" && tag == "" || project != "" && !ValidProjectID(project) || len(tag) > 128 || strings.ContainsAny(tag, "\r\n\x00,") {
		return TaskPage{}, Invalid
	}
	q := url.Values{"limit": {"200"}, "include_total": {"true"}}
	if project != "" {
		q.Set("project_id", project)
	}
	if tag != "" {
		q.Set("tag", tag)
	}
	out := TaskPage{Tasks: []Task{}}
	seen := map[string]bool{}
	cursors := map[string]bool{}
	for range c.maxPages {
		var wire struct {
			Items *[]Task `json:"items"`
			Meta  *struct {
				HasMore    *bool  `json:"has_more"`
				NextCursor string `json:"next_cursor"`
				Total      *int   `json:"total"`
			} `json:"meta"`
		}
		if err := c.get(ctx, "/api/v1/tasks", q, &wire); err != nil {
			out.Evidence.Partial = true
			out.Evidence.Code = safeCode(err)
			return out, err
		}
		if wire.Items == nil || wire.Meta == nil || wire.Meta.HasMore == nil || len(*wire.Items) > 200 {
			return TaskPage{}, Malformed
		}
		out.Evidence.Pages++
		out.Evidence.Total = wire.Meta.Total
		out.Evidence.NextCursor = wire.Meta.NextCursor
		for _, t := range *wire.Items {
			if !taskID.MatchString(t.ID) || project != "" && t.ProjectID != project {
				return TaskPage{}, Malformed
			}
			if tag != "" {
				found := false
				for _, v := range t.Tags {
					if v.Slug == tag {
						found = true
					}
				}
				if !found {
					return TaskPage{}, Malformed
				}
			}
			if !seen[t.ID] {
				out.Tasks = append(out.Tasks, t)
				seen[t.ID] = true
			}
		}
		if !*wire.Meta.HasMore {
			out.Evidence.Complete = true
			return out, nil
		}
		if wire.Meta.NextCursor == "" || cursors[wire.Meta.NextCursor] {
			return TaskPage{}, Malformed
		}
		cursors[wire.Meta.NextCursor] = true
		q.Set("cursor", wire.Meta.NextCursor)
	}
	out.Evidence.Partial = true
	out.Evidence.Code = "truncated"
	return out, nil
}
func safeCode(err error) string {
	var e Failure
	if errors.As(err, &e) {
		return string(e)
	}
	return string(Unavailable)
}
