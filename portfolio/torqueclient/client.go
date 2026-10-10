// Package torqueclient provides bounded, read-only Torque metadata access.
// It has no ambient configuration, identity provider, or production binding.
package torqueclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Failure never includes configuration, bearer bytes, or upstream error bodies.
type Failure string

func (e Failure) Error() string { return string(e) }

const (
	Invalid     Failure = "bad_request"
	Denied      Failure = "denied"
	Unavailable Failure = "unavailable"
	Malformed   Failure = "malformed"
	TooLarge    Failure = "too_large"
	NotFound    Failure = "not_found"
	Incomplete  Failure = "incomplete"
)

// Resource describes returned/requested resources for admission, not authority.
// Tags and associations let a trusted verifier enforce its exact resource scope.
type Resource struct {
	Kind      string
	ID        string
	ProjectID string
	EpicID    string
	SprintID  string
	Tags      []string
	Status    string
	Priority  *int
	CommentID string
}

// Request is detached evidence for the injected verifier. Query is canonical.
// Phase is query before access and disclosure before returning results.
type Request struct {
	Endpoint    string
	Host        string
	Integration string
	Operation   string
	Path        string
	Query       string
	Phase       string
	Resources   []Resource
}

// Grant is an opaque verified partition. Labels alone never admit a request.
// Verifier must resolve current assurance, grants and credential/scope epochs
// on every invocation, including cache hits. Missing fields refuse.
type Grant struct {
	Principal       string
	Scope           string
	ScopeEpoch      string
	CredentialRef   string
	CredentialEpoch string
}

// Verify must honor context and admit the exact query and every supplied
// resource using trusted opaque proof. Filter labels/service credentials are
// not authority. The client supplies no implementation of this contract.
type Verify func(context.Context, any, Request) (Grant, error)

// Resolve must honor context and resolve only the currently verified reference.
// It must not provision tokens or discover endpoints.
type Resolve func(context.Context, string, string) (string, error)

type Config struct {
	// Endpoint is an explicit origin with the /api/v1 base path.
	Endpoint    string
	Host        string
	Integration string
	Verify      Verify
	Resolve     Resolve
	Transport   http.RoundTripper
	// Injected transports must honor context and must not retry, redirect or
	// send credentials to a target other than the explicit request origin.
	// Zero selects the finite defaults. Overrides may only lower the bounds.
	GETTimeout       time.Duration
	OperationTimeout time.Duration
	CacheTTL         time.Duration
	MaxBody          int64
	MaxPages         int
	MaxEntries       int
	MaxCacheBytes    int
}

type cacheEntry struct {
	value     []byte // compact DTO only, never raw transport bodies or bearer bytes
	resources []byte
	expires   time.Time
	size      int
}

type Client struct {
	base                                *url.URL
	host, integration                   string
	verify                              Verify
	resolve                             Resolve
	http                                *http.Client
	getTimeout, operationTimeout, ttl   time.Duration
	maxBody                             int64
	maxPages, maxEntries, maxCacheBytes int
	capacity                            chan struct{}
	mu                                  sync.Mutex
	cache                               map[string]cacheEntry
	cacheBytes                          int
}

func New(c Config) (*Client, error) {
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" || strings.TrimSuffix(u.Path, "/") != "/api/v1" || c.Host == "" || strings.ContainsAny(c.Host, "/?#@ \t\r\n\x00") || c.Integration == "" || len(c.Host) > 255 || len(c.Integration) > 256 {
		return nil, Invalid
	}
	host, err := url.Parse("http://" + c.Host)
	if err != nil || host.Host != c.Host || host.Hostname() == "" || host.Path != "" {
		return nil, Invalid
	}
	for _, target := range []*url.URL{u, host} {
		if port := target.Port(); port != "" {
			n, err := strconv.Atoi(port)
			if err != nil || n < 1 || n > 65535 {
				return nil, Invalid
			}
		}
	}
	// Plain HTTP is permitted only for an explicitly selected loopback target.
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && (u.Scheme != "http" || u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback())) {
		return nil, Invalid
	}
	if c.GETTimeout == 0 {
		c.GETTimeout = 4 * time.Second
	}
	if c.OperationTimeout == 0 {
		c.OperationTimeout = 20 * time.Second
	}
	if c.CacheTTL == 0 {
		c.CacheTTL = 15 * time.Second
	}
	if c.MaxBody == 0 {
		c.MaxBody = 8 << 20
	}
	if c.MaxPages == 0 {
		c.MaxPages = 5
	}
	if c.MaxEntries == 0 {
		c.MaxEntries = 1000
	}
	if c.MaxCacheBytes == 0 {
		c.MaxCacheBytes = 16 << 20
	}
	if c.GETTimeout < 0 || c.GETTimeout > 4*time.Second || c.OperationTimeout < 0 || c.OperationTimeout > 20*time.Second || c.CacheTTL < 0 || c.CacheTTL > 15*time.Second || c.MaxBody < 1 || c.MaxBody > 8<<20 || c.MaxPages < 1 || c.MaxPages > 5 || c.MaxEntries < 1 || c.MaxEntries > 1000 || c.MaxCacheBytes < 1 || c.MaxCacheBytes > 16<<20 {
		return nil, Invalid
	}
	transport := c.Transport
	if transport == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Proxy = nil
		// Fresh HTTP/1 connections avoid Transport's implicit retry of GETs on
		// failed reused connections. No application retry loop exists either.
		t.DisableKeepAlives = true
		t.ForceAttemptHTTP2 = false
		t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		transport = t
	}
	u.Path = "/api/v1"
	return &Client{base: u, host: c.Host, integration: c.Integration, verify: c.Verify, resolve: c.Resolve,
		http:       &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		getTimeout: c.GETTimeout, operationTimeout: c.OperationTimeout, ttl: c.CacheTTL, maxBody: c.MaxBody,
		maxPages: c.MaxPages, maxEntries: c.MaxEntries, maxCacheBytes: c.MaxCacheBytes, capacity: make(chan struct{}, 6), cache: map[string]cacheEntry{}}, nil
}

// ClearCache invalidates all retained metadata. It cannot confer authority.
func (c *Client) ClearCache() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache = map[string]cacheEntry{}
	c.cacheBytes = 0
}

func (c *Client) authorize(ctx context.Context, proof any, r Request, expected *Grant) (Grant, error) {
	if c == nil || c.verify == nil || ctx.Err() != nil {
		return Grant{}, Denied
	}
	r.Resources = copyResources(r.Resources)
	r.Endpoint = c.base.String()
	r.Host = c.host
	r.Integration = c.integration
	g, err := c.verify(ctx, proof, r)
	if err != nil || ctx.Err() != nil || g.Principal == "" || g.Scope == "" || g.ScopeEpoch == "" || g.CredentialRef == "" || g.CredentialEpoch == "" || len(g.Principal)+len(g.Scope)+len(g.ScopeEpoch)+len(g.CredentialRef)+len(g.CredentialEpoch) > 4096 || expected != nil && g != *expected {
		return Grant{}, Denied
	}
	return g, nil
}

func copyResources(in []Resource) []Resource {
	out := append([]Resource(nil), in...)
	for i := range out {
		out[i].Tags = append([]string(nil), out[i].Tags...)
		if out[i].Priority != nil {
			n := *out[i].Priority
			out[i].Priority = &n
		}
	}
	return out
}

type operation struct {
	client *Client
	ctx    context.Context
	proof  any
	name   string
	grant  *Grant
	parent ParentGuard
}

func (o *operation) check(r Request) (Grant, error) {
	r.Operation = o.name
	principal := ""
	if o.parent != nil {
		var err error
		principal, err = o.parent(o.ctx)
		if err != nil || o.ctx.Err() != nil || principal == "" {
			return Grant{}, Denied
		}
	}
	g, err := o.client.authorize(o.ctx, o.proof, r, o.grant)
	if o.parent != nil {
		current, parentErr := o.parent(o.ctx)
		if parentErr != nil || o.ctx.Err() != nil || current != principal || current == "" || g.Principal != principal {
			return Grant{}, Denied
		}
	}
	if err == nil && o.grant == nil {
		o.grant = &g
	}
	return g, err
}

func (c *Client) forget(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.cache[key]; ok {
		delete(c.cache, key)
		c.cacheBytes -= entry.size
	}
}

func decode(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	v, err := jsonValue(d, 0)
	if err != nil {
		return nil, Malformed
	}
	if _, err = d.Token(); !errors.Is(err, io.EOF) {
		return nil, Malformed
	}
	return v, nil
}

// Refuse ambiguous duplicate metadata keys before inferring completeness.
// Depth is finite even when the body is below its byte limit.
func jsonValue(d *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, Malformed
	}
	t, err := d.Token()
	if err != nil {
		return nil, Malformed
	}
	delim, container := t.(json.Delim)
	if !container {
		return t, nil
	}
	switch delim {
	case '{':
		out := map[string]any{}
		for d.More() {
			token, err := d.Token()
			if err != nil {
				return nil, Malformed
			}
			key, ok := token.(string)
			if !ok {
				return nil, Malformed
			}
			if _, has := out[key]; has {
				return nil, Malformed
			}
			v, err := jsonValue(d, depth+1)
			if err != nil {
				return nil, Malformed
			}
			out[key] = v
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return nil, Malformed
		}
		return out, nil
	case '[':
		out := []any{}
		for d.More() {
			v, err := jsonValue(d, depth+1)
			if err != nil {
				return nil, Malformed
			}
			out = append(out, v)
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return nil, Malformed
		}
		return out, nil
	default:
		return nil, Malformed
	}
}

func (c *Client) key(g Grant, r Request) string {
	b, _ := json.Marshal([]any{c.base.String(), c.host, c.integration, g, r.Operation, r.Path, r.Query})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (c *Client) cached(key string) (cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.cache[key]
	if ok && !e.expires.After(time.Now()) {
		delete(c.cache, key)
		c.cacheBytes -= e.size
		return cacheEntry{}, false
	}
	return e, ok
}

func (c *Client) remember(key string, e cacheEntry) {
	e.size = len(e.value) + len(e.resources) + len(key) + 128
	if e.size > c.maxCacheBytes {
		return
	}
	e.expires = time.Now().Add(c.ttl)
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.cache[key]; ok {
		delete(c.cache, key)
		c.cacheBytes -= old.size
	}
	for k, v := range c.cache {
		if !v.expires.After(time.Now()) {
			delete(c.cache, k)
			c.cacheBytes -= v.size
		}
	}
	for len(c.cache) >= c.maxEntries || c.cacheBytes+e.size > c.maxCacheBytes {
		// Bounded eviction; order has no authority or completeness meaning.
		for k, v := range c.cache {
			delete(c.cache, k)
			c.cacheBytes -= v.size
			break
		}
	}
	c.cache[key] = e
	c.cacheBytes += e.size
}

type project func(any) (any, []Resource, error)

// get bounds verification, capacity wait, credential resolution and body read
// with one absolute deadline. Injected callbacks/transports must honor context.
func (o *operation) get(path string, q url.Values, requested []Resource, projection project) (any, error) {
	c := o.client
	ctx, cancel := context.WithTimeout(o.ctx, c.getTimeout)
	defer cancel()
	local := *o
	local.ctx = ctx
	r := Request{Operation: o.name, Path: path, Query: q.Encode(), Phase: "query", Resources: requested}
	g, err := local.check(r)
	if err != nil {
		return nil, err
	}
	select {
	case c.capacity <- struct{}{}:
	case <-ctx.Done():
		return nil, Unavailable
	}
	defer func() { <-c.capacity }()
	if ctx.Err() != nil {
		return nil, Unavailable
	}
	if _, err = local.check(r); err != nil {
		return nil, err
	}
	if c.resolve == nil {
		return nil, Denied
	}
	token, err := c.resolve(ctx, g.CredentialRef, g.CredentialEpoch)
	if err != nil || ctx.Err() != nil || token == "" || len(token) > 16384 || strings.ContainsAny(token, " \t\r\n\x00") {
		return nil, Denied
	}
	if _, err = local.check(r); err != nil {
		return nil, err
	}
	u := *c.base
	u.Path += path
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, Invalid
	}
	req.Host = c.host
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, Unavailable
	}
	defer func() { _ = resp.Body.Close() }()
	if ctx.Err() != nil {
		return nil, Unavailable
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		c.ClearCache()
		return nil, Denied
	}
	if resp.StatusCode == 404 {
		r.Phase = "disclosure"
		if _, err = local.check(r); err != nil {
			return nil, err
		}
		return nil, NotFound
	}
	if resp.StatusCode == 400 || resp.StatusCode == 422 {
		return nil, Invalid
	}
	if resp.StatusCode != 200 {
		return nil, Unavailable
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody+1))
	if err != nil || ctx.Err() != nil {
		return nil, Unavailable
	}
	if int64(len(body)) > c.maxBody {
		return nil, TooLarge
	}
	raw, err := decode(body)
	if err != nil {
		return nil, err
	}
	v, resources, err := projection(raw)
	if err != nil {
		return nil, err
	}
	r.Phase = "disclosure"
	r.Resources = resources
	if _, err = local.check(r); err != nil {
		return nil, err
	}
	return v, nil
}

func (c *Client) begin(ctx context.Context, proof any, name string) (*operation, context.CancelFunc, error) {
	if c == nil {
		return nil, func() {}, Denied
	}
	ctx, cancel := context.WithTimeout(ctx, c.operationTimeout)
	o := &operation{client: c, ctx: ctx, proof: proof, name: name}
	return o, cancel, nil
}

func isFailure(err error, f Failure) bool { return errors.Is(err, f) }
