// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package daemontest is a fake daemon held to the contract, for testing anything that reaches the instance
// through the daemon: the command tree's verbs and, from M20a, the terminal client.
//
// It answers relayed requests as the instance would, and only as openapi.yaml allows: every request must name
// a route and method the contract has, carry only query parameters it declares, and send a body its request
// schema accepts, and every answer a test scripts must match the response schema for its status. So the code
// under test cannot send what the instance would refuse to decode, and a test cannot pass by answering with
// something the instance would never send (M19's lesson about fixtures). It moved here from the verbs'
// tests at M20a, when a second package needed it.
//
// A client attached with events receives dispatches as the daemon forwards them, and a message the fake
// accepts is fanned out to every such client as MESSAGE_CREATE, the sender's included, as the instance does.
// That is the one mutation it echoes, because sending is the one M20a's client does; a test of editing or
// deleting through the client teaches it MESSAGE_UPDATE or MESSAGE_DELETE the same way, in Do.
//
// A test-support package: nothing outside a test imports it, so it is never linked into a binary.
package daemontest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/Alexnex31/Norite/daemon/ipc"
)

// Contracts is the repository's contracts directory: found from this file, since the working directory
// differs by the package a test runs in, and failing that by walking up from the working directory. The
// second covers a build with -trimpath, where this file's recorded path is module-relative and leads
// nowhere (M20a's second /code-review).
func Contracts() string {
	if _, file, _, ok := runtime.Caller(0); ok {
		dir := filepath.Join(filepath.Dir(file), "..", "..", "..", "contracts")
		if _, err := os.Stat(filepath.Join(dir, "openapi.yaml")); err == nil {
			return dir
		}
	}
	dir, err := os.Getwd()
	if err != nil {
		return "contracts"
	}
	for {
		candidate := filepath.Join(dir, "contracts")
		if _, err := os.Stat(filepath.Join(candidate, "openapi.yaml")); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "contracts"
		}
		dir = parent
	}
}

// ---------- openapi.yaml ----------

type operation struct {
	id        string
	query     map[string]bool
	body      *jsonschema.Schema
	responses map[int]*jsonschema.Schema // nil schema: a status with no body
}

type route struct {
	template string
	re       *regexp.Regexp
	ops      map[string]*operation
}

var (
	apiOnce   sync.Once
	apiRoutes []*route
	apiErr    error
)

// normalize turns what yaml.v3 decodes into something encoding/json can encode.
func normalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = normalize(e)
		}
		return x
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[fmt.Sprint(k)] = normalize(e)
		}
		return out
	case []any:
		for i, e := range x {
			x[i] = normalize(e)
		}
		return x
	}
	return v
}

func pointer(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString("/")
		b.WriteString(strings.ReplaceAll(strings.ReplaceAll(p, "~", "~0"), "/", "~1"))
	}
	return b.String()
}

func loadAPI(t testing.TB) []*route {
	t.Helper()
	apiOnce.Do(func() { apiRoutes, apiErr = compileAPI() })
	require.NoError(t, apiErr)
	return apiRoutes
}

func compileAPI() ([]*route, error) {
	raw, err := os.ReadFile(filepath.Join(Contracts(), "openapi.yaml"))
	if err != nil {
		return nil, err
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	doc = normalize(doc)
	encoded, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	const id = "https://norite.example/contracts/openapi.json"
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(id, inst); err != nil {
		return nil, err
	}
	compile := func(ptr string) (*jsonschema.Schema, error) { return c.Compile(id + "#" + ptr) }

	root := doc.(map[string]any)
	var routes []*route
	for template, item := range root["paths"].(map[string]any) {
		r := &route{template: template, ops: map[string]*operation{}}
		param := regexp.MustCompile(`\\\{[^}]+\\\}`)
		r.re = regexp.MustCompile("^" + param.ReplaceAllString(regexp.QuoteMeta(template), `[^/]+`) + "$")

		methods := item.(map[string]any)
		shared := map[string]bool{}
		if ps, ok := methods["parameters"].([]any); ok {
			for _, p := range ps {
				if pm := p.(map[string]any); pm["in"] == "query" {
					shared[pm["name"].(string)] = true
				}
			}
		}
		for method, o := range methods {
			om, ok := o.(map[string]any)
			if !ok || method == "parameters" {
				continue
			}
			op := &operation{id: fmt.Sprint(om["operationId"]), query: map[string]bool{}, responses: map[int]*jsonschema.Schema{}}
			for k := range shared {
				op.query[k] = true
			}
			if ps, ok := om["parameters"].([]any); ok {
				for _, p := range ps {
					if pm := p.(map[string]any); pm["in"] == "query" {
						op.query[pm["name"].(string)] = true
					}
				}
			}
			if rb, ok := om["requestBody"].(map[string]any); ok {
				if content, ok := rb["content"].(map[string]any); ok && content["application/json"] != nil {
					if op.body, err = compile(pointer("paths", template, method, "requestBody", "content",
						"application/json", "schema")); err != nil {
						return nil, err
					}
				}
			}
			for code, resp := range om["responses"].(map[string]any) {
				status, err := strconv.Atoi(code)
				if err != nil {
					continue
				}
				ptr := pointer("paths", template, method, "responses", code)
				rm := resp.(map[string]any)
				if ref, ok := rm["$ref"].(string); ok {
					ptr = strings.TrimPrefix(ref, "#")
					rm = root
					for _, part := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
						rm = rm[part].(map[string]any)
					}
				}
				content, ok := rm["content"].(map[string]any)
				if !ok || content["application/json"] == nil {
					op.responses[status] = nil
					continue
				}
				if op.responses[status], err = compile(ptr + pointer("content", "application/json", "schema")); err != nil {
					return nil, err
				}
			}
			r.ops[strings.ToUpper(method)] = op
		}
		routes = append(routes, r)
	}
	// Each pattern is anchored at both ends, so no route can shadow another; sorted only so a failure
	// names the same route on every run.
	sort.Slice(routes, func(i, j int) bool { return routes[i].template < routes[j].template })
	return routes, nil
}

func matchRoute(routes []*route, path string) *route {
	for _, r := range routes {
		if r.re.MatchString(path) {
			return r
		}
	}
	return nil
}

// Conforms validates data against a compiled schema.
func Conforms(s *jsonschema.Schema, data []byte) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return err
	}
	return s.Validate(inst)
}

// ---------- the fake daemon ----------

// Request is one relayed request as the fake received it, after the contract accepted it.
type Request struct {
	Method, Path, Op string
	Query            url.Values
	Body             json.RawMessage
}

// Answer scripts the instance's answer to one operation: a status and a body the contract must accept.
type Answer func(r Request) (status int, body any)

// Daemon answers relayed requests as the instance would, through the contract, and holds the clients
// attached to it.
type Daemon struct {
	t      testing.TB
	routes []*route

	mu      sync.Mutex
	answers map[string]Answer // by operationId
	calls   []Request
	clients []*Client
}

// New builds a fake daemon for one test. Its clients are closed when the test ends.
func New(t testing.TB) *Daemon {
	d := &Daemon{t: t, routes: loadAPI(t), answers: map[string]Answer{}}
	t.Cleanup(func() { d.Drop(errors.New("the test ended")) })
	return d
}

// On scripts the answer to one operation, by its operationId.
func (d *Daemon) On(op string, answer Answer) *Daemon {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.answers[op] = answer
	return d
}

// Requests is every request the fake accepted, in order.
func (d *Daemon) Requests() []Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Request(nil), d.calls...)
}

// OK, Created and NoContent are the answers most tests script.
func OK(body any) Answer      { return func(Request) (int, any) { return 200, body } }
func Created(body any) Answer { return func(Request) (int, any) { return 201, body } }
func NoContent() Answer       { return func(Request) (int, any) { return 204, nil } }

// Client is one attachment to the fake: what ipc.Client is to the real daemon, as far as a client uses it.
type Client struct {
	d      *Daemon
	ready  ipc.Ready
	events chan ipc.Event
	done   chan struct{}

	// mu orders a dispatch against the client ending: numbering, the send and the close happen under it,
	// so a dispatch racing a drop neither sends on a closed channel nor races on seq (M20a's second
	// /code-review).
	mu     sync.Mutex
	seq    int64
	closed bool
	err    error
}

// Attach connects a client signed in as ready says. With events, it receives every dispatch from here on.
func (d *Daemon) Attach(ready ipc.Ready, events bool) *Client {
	c := &Client{d: d, ready: ready, done: make(chan struct{}), seq: 1}
	if events {
		c.events = make(chan ipc.Event, 256)
	}
	d.mu.Lock()
	d.clients = append(d.clients, c)
	d.mu.Unlock()
	return c
}

// Dispatch forwards an event to every client attached with events, numbered per client from 2 as the
// daemon numbers them, READY being 1. The payload is checked against nothing: a test choosing what the
// instance sends is the point of calling this directly.
func (d *Daemon) Dispatch(eventType string, payload json.RawMessage) {
	d.mu.Lock()
	clients := append([]*Client(nil), d.clients...)
	d.mu.Unlock()
	for _, c := range clients {
		c.dispatch(eventType, payload)
	}
}

// Drop ends every attached client with err, as a daemon stopping or resyncing its clients does. An
// *ipc.CloseError carries the close code.
func (d *Daemon) Drop(err error) {
	d.mu.Lock()
	clients := d.clients
	d.clients = nil
	d.mu.Unlock()
	for _, c := range clients {
		c.end(err)
	}
}

func (c *Client) dispatch(eventType string, payload json.RawMessage) {
	if c.events == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.seq++
	select {
	case c.events <- ipc.Event{Type: eventType, Seq: c.seq, Data: append(json.RawMessage(nil), payload...)}:
	default:
		// The real daemon drops a client that stops reading (4012), and so does this one: a test that fills
		// 256 events without reading has stopped reading, and ending the stream says so without a hang, and
		// without reporting to a test that may already have finished.
		c.endLocked(errors.New("the client stopped reading its events and was dropped as too slow"))
	}
}

func (c *Client) end(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.endLocked(err)
}

func (c *Client) endLocked(err error) {
	if c.closed {
		return
	}
	c.closed = true
	c.err = err
	close(c.done)
	if c.events != nil {
		close(c.events)
	}
}

// Do performs a relayed request through the fake daemon, once this client is still attached.
func (c *Client) Do(ctx context.Context, method, path string, body any) (ipc.Result, error) {
	select {
	case <-c.done:
		return ipc.Result{}, c.Err()
	default:
	}
	return c.d.Do(ctx, method, path, body)
}

// Ready is what the daemon said when the client attached.
func (c *Client) Ready() ipc.Ready { return c.ready }

// Events are the dispatches forwarded to this client, closed when it is dropped. Nil without events.
func (c *Client) Events() <-chan ipc.Event { return c.events }

// Done is closed when the client is dropped or closed.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err is why the client ended, once Done is closed.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close detaches the client.
func (c *Client) Close() error {
	c.end(errors.New("closed"))
	return nil
}

func (d *Daemon) Do(_ context.Context, method, rawPath string, body any) (ipc.Result, error) {
	t := d.t
	path, rawQuery, _ := strings.Cut(rawPath, "?")
	r := matchRoute(d.routes, path)
	if r == nil {
		t.Errorf("%s %s: no such route in openapi.yaml", method, rawPath)
		return ipc.Result{Status: 599}, nil
	}
	op := r.ops[method]
	if op == nil {
		t.Errorf("%s %s: openapi.yaml has no %s on %s", method, rawPath, method, r.template)
		return ipc.Result{Status: 599}, nil
	}

	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		t.Errorf("%s %s: the query does not parse: %v", method, rawPath, err)
	}
	for k := range q {
		if !op.query[k] {
			t.Errorf("%s %s: %s takes no query parameter %q", method, rawPath, op.id, k)
		}
	}

	var encoded json.RawMessage
	if body != nil {
		if encoded, err = json.Marshal(body); err != nil {
			t.Fatalf("encoding the request body: %v", err)
		}
		if op.body == nil {
			t.Errorf("%s %s: %s takes no body, and was sent %s", method, rawPath, op.id, encoded)
		} else if err := Conforms(op.body, encoded); err != nil {
			t.Errorf("%s %s: the body does not match %s's request schema: %s\n%v", method, rawPath, op.id,
				encoded, err)
		}
	} else if op.body != nil {
		t.Errorf("%s %s: %s takes a body and was sent none", method, rawPath, op.id)
	}

	req := Request{Method: method, Path: path, Op: op.id, Query: q, Body: encoded}
	d.mu.Lock()
	d.calls = append(d.calls, req)
	answer := d.answers[op.id]
	d.mu.Unlock()
	if answer == nil {
		t.Errorf("%s %s: the test scripted no answer for %s", method, rawPath, op.id)
		return ipc.Result{Status: 599}, nil
	}

	status, out := answer(req)
	schema, declared := op.responses[status]
	if !declared {
		t.Errorf("%s answered %d, which openapi.yaml does not declare for it", op.id, status)
	}
	var respBody json.RawMessage
	if out != nil {
		if respBody, err = json.Marshal(out); err != nil {
			t.Fatalf("encoding a scripted answer: %v", err)
		}
		if schema == nil {
			t.Errorf("%s answered %d with a body the contract does not declare", op.id, status)
		} else if err := Conforms(schema, respBody); err != nil {
			t.Errorf("%s's scripted %d does not match the contract: %s\n%v", op.id, status, respBody, err)
		}
	}
	if op.id == "sendMessage" && status == 201 {
		// What the instance does next: MESSAGE_CREATE to everybody watching, the sender included.
		d.Dispatch("MESSAGE_CREATE", respBody)
	}
	return ipc.Result{Status: status, Body: respBody}, nil
}

// ---------- contracts/cli-json ----------

var (
	cliOnce     sync.Once
	cliCompiler *jsonschema.Compiler
	cliErr      error
)

// MatchesCLISchema validates what a command printed under --json against one definition in
// contracts/cli-json/ (rule 15).
func MatchesCLISchema(t testing.TB, out, file, def string) {
	t.Helper()
	cliOnce.Do(func() {
		cliCompiler = jsonschema.NewCompiler()
		cliCompiler.AssertFormat()
		entries, err := os.ReadDir(filepath.Join(Contracts(), "cli-json"))
		if err != nil {
			cliErr = err
			return
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".schema.json") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(Contracts(), "cli-json", e.Name()))
			if err != nil {
				cliErr = err
				return
			}
			inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				cliErr = fmt.Errorf("%s: %w", e.Name(), err)
				return
			}
			if cliErr = cliCompiler.AddResource("https://norite.chat/contracts/cli-json/"+e.Name(), inst); cliErr != nil {
				return
			}
		}
	})
	require.NoError(t, cliErr)
	s, err := cliCompiler.Compile("https://norite.chat/contracts/cli-json/" + file + "#/$defs/" + def)
	require.NoError(t, err)
	require.NoError(t, Conforms(s, []byte(out)), "%s does not match %s#%s:\n%s", "the output", file, def, out)
}
