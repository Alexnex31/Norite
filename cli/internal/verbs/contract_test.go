// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package verbs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"

	"github.com/Alexnex31/Norite/cli/internal/daemonclient"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// The verbs are tested against a fake daemon that holds them to openapi.yaml: every relayed request must
// name a route and method the contract has, carry only query parameters it declares, and send a body its
// request schema accepts; every answer the tests script must match the response schema for its status. So a
// verb cannot send what the instance would refuse to decode, and a test cannot pass by answering with
// something the instance would never send (M19's lesson about fixtures, again).
//
// What every verb prints under --json is validated against its schema in contracts/cli-json/.

const contracts = "../../../contracts"

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

func loadAPI(t *testing.T) []*route {
	t.Helper()
	apiOnce.Do(func() { apiRoutes, apiErr = compileAPI() })
	require.NoError(t, apiErr)
	return apiRoutes
}

func compileAPI() ([]*route, error) {
	raw, err := os.ReadFile(filepath.Join(contracts, "openapi.yaml"))
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

func conforms(s *jsonschema.Schema, data []byte) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return err
	}
	return s.Validate(inst)
}

// ---------- the fake daemon ----------

type request struct {
	Method, Path, Op string
	Query            url.Values
	Body             json.RawMessage
}

type answerFunc func(r request) (status int, body any)

// fakeDaemon answers relayed requests as the instance would, through the contract.
type fakeDaemon struct {
	t      *testing.T
	routes []*route

	mu      sync.Mutex
	answers map[string]answerFunc // by operationId
	calls   []request
}

func newFake(t *testing.T) *fakeDaemon {
	return &fakeDaemon{t: t, routes: loadAPI(t), answers: map[string]answerFunc{}}
}

// on scripts the answer to one operation.
func (f *fakeDaemon) on(op string, answer answerFunc) *fakeDaemon {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[op] = answer
	return f
}

func (f *fakeDaemon) requests() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.calls...)
}

func (f *fakeDaemon) Do(_ context.Context, method, rawPath string, body any) (ipc.Result, error) {
	t := f.t
	path, rawQuery, _ := strings.Cut(rawPath, "?")
	r := matchRoute(f.routes, path)
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
		} else if err := conforms(op.body, encoded); err != nil {
			t.Errorf("%s %s: the body does not match %s's request schema: %s\n%v", method, rawPath, op.id,
				encoded, err)
		}
	} else if op.body != nil {
		t.Errorf("%s %s: %s takes a body and was sent none", method, rawPath, op.id)
	}

	req := request{Method: method, Path: path, Op: op.id, Query: q, Body: encoded}
	f.mu.Lock()
	f.calls = append(f.calls, req)
	answer := f.answers[op.id]
	f.mu.Unlock()
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
		} else if err := conforms(schema, respBody); err != nil {
			t.Errorf("%s's scripted %d does not match the contract: %s\n%v", op.id, status, respBody, err)
		}
	}
	return ipc.Result{Status: status, Body: respBody}, nil
}

// ---------- running a verb ----------

type ran struct {
	out string
	err error
}

// runVerb runs argv through the real verb tree, attached to f, with stdin answering any question.
func runVerb(t *testing.T, f *fakeDaemon, stdin string, argv ...string) ran {
	t.Helper()
	connect := func(context.Context) (daemonclient.Caller, func(), error) { return f, func() {}, nil }
	var out bytes.Buffer
	root := &cli.Command{
		Name: "norite", Writer: &out, Reader: strings.NewReader(stdin),
		Flags:          []cli.Flag{&cli.BoolFlag{Name: "json"}},
		ExitErrHandler: func(context.Context, *cli.Command, error) {},
		Commands:       Commands(connect),
	}
	err := root.Run(context.Background(), append([]string{"norite"}, argv...))
	return ran{out: out.String(), err: err}
}

// ---------- contracts/cli-json ----------

var (
	cliOnce     sync.Once
	cliCompiler *jsonschema.Compiler
	cliErr      error
)

// matchesCLISchema validates what a verb printed against one definition in contracts/cli-json/.
func matchesCLISchema(t *testing.T, out, file, def string) {
	t.Helper()
	cliOnce.Do(func() {
		cliCompiler = jsonschema.NewCompiler()
		cliCompiler.AssertFormat()
		entries, err := os.ReadDir(filepath.Join(contracts, "cli-json"))
		if err != nil {
			cliErr = err
			return
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".schema.json") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(contracts, "cli-json", e.Name()))
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
	require.NoError(t, conforms(s, []byte(out)), "%s does not match %s#%s:\n%s", "the output", file, def, out)
}
