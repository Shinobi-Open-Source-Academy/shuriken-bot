package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// fakeAPI is a stand-in for the Messages endpoint. Driving the real SDK against
// it exercises the tool loop, the call cap and the retry path, including the
// request serialisation, which a hand-rolled mock of the interface would skip.
type fakeAPI struct {
	mu       sync.Mutex
	requests []map[string]any
	// responses are returned in order; the last one repeats if the loop runs
	// longer than the script.
	responses []string
}

func (f *fakeAPI) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		f.mu.Lock()
		f.requests = append(f.requests, body)
		idx := len(f.requests) - 1
		if idx >= len(f.responses) {
			idx = len(f.responses) - 1
		}
		resp := f.responses[idx]
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, resp)
	}
}

func (f *fakeAPI) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeAPI) request(i int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

// textResponse is a terminal message carrying the structured JSON output.
func textResponse(payload string) string {
	escaped, _ := json.Marshal(payload)
	return fmt.Sprintf(`{
		"id": "msg_1", "type": "message", "role": "assistant",
		"model": "claude-opus-5-5", "stop_reason": "end_turn",
		"content": [{"type": "text", "text": %s}],
		"usage": {"input_tokens": 100, "output_tokens": 50,
		          "cache_read_input_tokens": 10, "cache_creation_input_tokens": 5}
	}`, escaped)
}

// toolUseResponse asks to read a file.
func toolUseResponse(id, path string) string {
	return fmt.Sprintf(`{
		"id": "msg_tool", "type": "message", "role": "assistant",
		"model": "claude-opus-5-5", "stop_reason": "tool_use",
		"content": [{"type": "tool_use", "id": %q, "name": "read_file",
		             "input": {"path": %q}}],
		"usage": {"input_tokens": 20, "output_tokens": 10,
		          "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}
	}`, id, path)
}

const validOutput = `{"summary":"One issue.","findings":[{"path":"a.go","line":2,` +
	`"side":"RIGHT","severity":"high","confidence":0.9,"title":"Unchecked error",` +
	`"explanation":"why","concept":"error handling","mechanical":false}]}`

func newTestClient(t *testing.T, api *fakeAPI, opts Options) Client {
	t.Helper()
	srv := httptest.NewServer(api.handler())
	t.Cleanup(srv.Close)

	if opts.Model == "" {
		opts.Model = "claude-opus-5-5"
	}
	if opts.MaxOutputTokens == 0 {
		opts.MaxOutputTokens = 4096
	}
	if opts.MaxToolCalls == 0 {
		opts.MaxToolCalls = 3
	}
	if opts.MaxFileBytes == 0 {
		opts.MaxFileBytes = 1000
	}

	return &anthropicClient{
		api: anthropic.NewClient(
			option.WithAPIKey("test-key"),
			option.WithBaseURL(srv.URL),
			// Retries would multiply the request count the tests assert on.
			option.WithMaxRetries(0),
		),
		opts: opts,
	}
}

func baseRequest() Request {
	return Request{
		SystemStatic: "system prompt",
		Conventions:  "project conventions",
		UserContent:  "the diff",
	}
}

func TestReviewParsesStructuredOutput(t *testing.T) {
	api := &fakeAPI{responses: []string{textResponse(validOutput)}}
	client := newTestClient(t, api, Options{})

	res, err := client.Review(context.Background(), baseRequest())
	if err != nil {
		t.Fatalf("Review() error: %v", err)
	}

	if res.Output.Summary != "One issue." {
		t.Errorf("Summary = %q", res.Output.Summary)
	}
	if len(res.Output.Findings) != 1 {
		t.Fatalf("got %d findings, want 1", len(res.Output.Findings))
	}
	if got := res.Output.Findings[0].Concept; got != "error handling" {
		t.Errorf("Concept = %q", got)
	}
	if res.Retried {
		t.Error("a valid response must not be retried")
	}
	if res.Usage.InputTokens != 100 || res.Usage.OutputTokens != 50 {
		t.Errorf("usage not accumulated: %+v", res.Usage)
	}
	if res.Usage.CacheReadInputTokens != 10 || res.Usage.CacheCreationInputTokens != 5 {
		t.Errorf("cache usage not captured: %+v", res.Usage)
	}
}

// The schema must travel in output_config.format. Forced tool use is rejected
// by current models, so this is the only thing guaranteeing the output shape.
func TestReviewSendsSchemaAndCacheBreakpoint(t *testing.T) {
	api := &fakeAPI{responses: []string{textResponse(validOutput)}}
	client := newTestClient(t, api, Options{})

	if _, err := client.Review(context.Background(), baseRequest()); err != nil {
		t.Fatal(err)
	}

	req := api.request(0)

	outputConfig, ok := req["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("no output_config in request: %v", keys(req))
	}
	format, ok := outputConfig["format"].(map[string]any)
	if !ok {
		t.Fatalf("no output_config.format: %v", outputConfig)
	}
	if format["type"] != "json_schema" {
		t.Errorf("format.type = %v, want json_schema", format["type"])
	}
	if _, ok := format["schema"].(map[string]any); !ok {
		t.Error("format.schema missing")
	}

	// tool_choice must never be "any" or "tool": both are 400s now.
	if tc, present := req["tool_choice"]; present {
		if m, ok := tc.(map[string]any); ok {
			if m["type"] == "any" || m["type"] == "tool" {
				t.Errorf("forced tool use is rejected by current models: %v", m)
			}
		}
	}

	// The cache breakpoint belongs on the last system block, so the volatile
	// diff in messages cannot invalidate the cached prefix.
	system, ok := req["system"].([]any)
	if !ok || len(system) != 2 {
		t.Fatalf("system = %v, want 2 blocks", req["system"])
	}
	last, _ := system[len(system)-1].(map[string]any)
	if _, ok := last["cache_control"]; !ok {
		t.Errorf("no cache_control on the final system block: %v", last)
	}
	if first, _ := system[0].(map[string]any); first["cache_control"] != nil {
		t.Error("cache_control should sit only on the last system block")
	}
}

func TestReviewOmitsToolWhenNoReaderSupplied(t *testing.T) {
	api := &fakeAPI{responses: []string{textResponse(validOutput)}}
	client := newTestClient(t, api, Options{})

	if _, err := client.Review(context.Background(), baseRequest()); err != nil {
		t.Fatal(err)
	}
	if _, present := api.request(0)["tools"]; present {
		t.Error("no file reader was supplied, so no tool should be offered")
	}
}

func TestReviewRunsToolLoop(t *testing.T) {
	api := &fakeAPI{responses: []string{
		toolUseResponse("tu_1", "a.go"),
		textResponse(validOutput),
	}}
	client := newTestClient(t, api, Options{})

	var readPaths []string
	req := baseRequest()
	req.ReadFile = func(_ context.Context, path string) (string, error) {
		readPaths = append(readPaths, path)
		return "package main", nil
	}

	res, err := client.Review(context.Background(), req)
	if err != nil {
		t.Fatalf("Review() error: %v", err)
	}

	if len(readPaths) != 1 || readPaths[0] != "a.go" {
		t.Errorf("readPaths = %v, want [a.go]", readPaths)
	}
	if res.Usage.ToolCalls != 1 {
		t.Errorf("ToolCalls = %d, want 1", res.Usage.ToolCalls)
	}
	// Usage must accumulate across every turn, not just the last.
	if res.Usage.InputTokens != 120 {
		t.Errorf("InputTokens = %d, want 120 (20 + 100)", res.Usage.InputTokens)
	}
	if res.ToolCallsCapped {
		t.Error("one call is well under the cap")
	}

	// The tool must be declared strict, which is what replaces the
	// schema-valid-arguments guarantee that forced tool use used to give.
	tools, ok := api.request(0)["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v", api.request(0)["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["strict"] != true {
		t.Errorf("tool is not strict: %v", tool)
	}
	if tool["name"] != readFileToolName {
		t.Errorf("tool name = %v, want %q", tool["name"], readFileToolName)
	}
}

// The cap is a hard budget: once spent, the model is told to finish and must
// not be able to ask for another file.
func TestReviewEnforcesToolCallCap(t *testing.T) {
	api := &fakeAPI{responses: []string{
		toolUseResponse("tu_1", "a.go"),
		toolUseResponse("tu_2", "b.go"),
		textResponse(validOutput),
	}}
	client := newTestClient(t, api, Options{MaxToolCalls: 2})

	var reads int
	req := baseRequest()
	req.ReadFile = func(context.Context, string) (string, error) {
		reads++
		return "contents", nil
	}

	res, err := client.Review(context.Background(), req)
	if err != nil {
		t.Fatalf("Review() error: %v", err)
	}

	if reads != 2 {
		t.Errorf("executed %d tool calls, want the cap of 2", reads)
	}
	if !res.ToolCallsCapped {
		t.Error("ToolCallsCapped should be set once the budget is spent")
	}

	// The request made after the cap must forbid further calls.
	final := api.request(api.calls() - 1)
	tc, ok := final["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("no tool_choice on the post-cap request: %v", keys(final))
	}
	if tc["type"] != "none" {
		t.Errorf("tool_choice.type = %v, want none", tc["type"])
	}
}

// A tool that fails must report back to the model, not abort the review.
func TestReviewToolErrorIsReportedToModel(t *testing.T) {
	api := &fakeAPI{responses: []string{
		toolUseResponse("tu_1", "missing.go"),
		textResponse(validOutput),
	}}
	client := newTestClient(t, api, Options{})

	req := baseRequest()
	req.ReadFile = func(context.Context, string) (string, error) {
		return "", errors.New("not found")
	}

	res, err := client.Review(context.Background(), req)
	if err != nil {
		t.Fatalf("a failing tool must not fail the review: %v", err)
	}
	if len(res.Output.Findings) != 1 {
		t.Error("the review should still complete")
	}

	// The follow-up request should carry an error tool_result.
	raw, _ := json.Marshal(api.request(1)["messages"])
	if !strings.Contains(string(raw), "is_error") {
		t.Errorf("tool error not reported back to the model: %s", raw)
	}
}

func TestReviewTruncatesOversizedFile(t *testing.T) {
	api := &fakeAPI{responses: []string{
		toolUseResponse("tu_1", "big.go"),
		textResponse(validOutput),
	}}
	client := newTestClient(t, api, Options{MaxFileBytes: 50})

	req := baseRequest()
	req.ReadFile = func(context.Context, string) (string, error) {
		return strings.Repeat("x", 5000), nil
	}

	if _, err := client.Review(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	raw, _ := json.Marshal(api.request(1)["messages"])
	if !strings.Contains(string(raw), "truncated") {
		t.Error("an oversized file should be truncated with a marker")
	}
	if len(raw) > 4000 {
		t.Errorf("tool result was not actually truncated (%d bytes)", len(raw))
	}
}

func TestReviewRetriesOnceOnMalformedOutput(t *testing.T) {
	tests := []struct {
		name string
		bad  string
	}{
		{"not json", "I found three problems with this code."},
		{"truncated json", `{"summary":"x","findings":[`},
		{"empty text", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAPI{responses: []string{
				textResponse(tc.bad),
				textResponse(validOutput),
			}}
			client := newTestClient(t, api, Options{})

			res, err := client.Review(context.Background(), baseRequest())
			if err != nil {
				t.Fatalf("Review() error: %v", err)
			}
			if !res.Retried {
				t.Error("Retried should be set")
			}
			if len(res.Output.Findings) != 1 {
				t.Error("the retry's output should be used")
			}
			if api.calls() != 2 {
				t.Errorf("made %d calls, want exactly 2 (one retry)", api.calls())
			}
			// The retry must tell the model what went wrong.
			raw, _ := json.Marshal(api.request(1)["messages"])
			if !strings.Contains(string(raw), "did not match the required output schema") {
				t.Errorf("retry did not explain the problem: %s", raw)
			}
		})
	}
}

// Exactly one retry. A model that keeps producing garbage must not loop.
func TestReviewGivesUpAfterOneRetry(t *testing.T) {
	api := &fakeAPI{responses: []string{textResponse("still not json")}}
	client := newTestClient(t, api, Options{})

	if _, err := client.Review(context.Background(), baseRequest()); err == nil {
		t.Fatal("expected an error after the retry also failed")
	}
	if api.calls() != 2 {
		t.Errorf("made %d calls, want exactly 2", api.calls())
	}
}

func TestReviewHandlesRefusal(t *testing.T) {
	api := &fakeAPI{responses: []string{`{
		"id": "msg_r", "type": "message", "role": "assistant",
		"model": "claude-opus-5-5", "stop_reason": "refusal",
		"stop_details": {"type": "refusal", "category": "cyber"},
		"content": [],
		"usage": {"input_tokens": 10, "output_tokens": 0,
		          "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}
	}`}}
	client := newTestClient(t, api, Options{})

	_, err := client.Review(context.Background(), baseRequest())
	if err == nil {
		t.Fatal("a refusal must surface as an error")
	}
	if !strings.Contains(err.Error(), "declined") {
		t.Errorf("error = %v, want it to mention the decline", err)
	}
	// A refusal is not malformed output, so it must not be retried.
	if api.calls() != 1 {
		t.Errorf("made %d calls, want 1: a refusal must not be retried", api.calls())
	}
}

func TestReviewOmitsEmptyConventionsBlock(t *testing.T) {
	api := &fakeAPI{responses: []string{textResponse(validOutput)}}
	client := newTestClient(t, api, Options{})

	req := baseRequest()
	req.Conventions = "   "

	if _, err := client.Review(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	system, _ := api.request(0)["system"].([]any)
	if len(system) != 1 {
		t.Errorf("system has %d blocks, want 1 when there are no conventions", len(system))
	}
}

func TestFindingsSchemaIsStructuredOutputCompatible(t *testing.T) {
	schema := FindingsSchema()

	// Structured outputs require additionalProperties: false on every object,
	// and reject numeric and string constraints.
	var walk func(node any, path string)
	walk = func(node any, path string) {
		m, ok := node.(map[string]any)
		if !ok {
			return
		}
		if m["type"] == "object" {
			if m["additionalProperties"] != false {
				t.Errorf("%s: object without additionalProperties:false", path)
			}
			if _, ok := m["required"]; !ok {
				t.Errorf("%s: object without required", path)
			}
		}
		for _, banned := range []string{"minimum", "maximum", "multipleOf", "minLength", "maxLength"} {
			if _, present := m[banned]; present {
				t.Errorf("%s: unsupported constraint %q", path, banned)
			}
		}
		if props, ok := m["properties"].(map[string]any); ok {
			for name, v := range props {
				walk(v, path+"."+name)
			}
		}
		if items, ok := m["items"]; ok {
			walk(items, path+"[]")
		}
	}
	walk(schema, "root")

	// Every field the post-processor depends on must be required.
	props := schema["properties"].(map[string]any)
	item := props["findings"].(map[string]any)["items"].(map[string]any)
	required := map[string]bool{}
	for _, r := range item["required"].([]string) {
		required[r] = true
	}
	for _, field := range []string{"path", "line", "side", "severity", "confidence", "title", "concept"} {
		if !required[field] {
			t.Errorf("finding field %q must be required", field)
		}
	}
}

func TestPricingCost(t *testing.T) {
	p := Pricing{
		InputPerMTok: 4, OutputPerMTok: 20, CacheReadPerMTok: 0.2, CacheWritePerMTok: 5,
	}
	got := p.Cost(Usage{
		InputTokens: 1_000_000, OutputTokens: 1_000_000,
		CacheReadInputTokens: 1_000_000, CacheCreationInputTokens: 1_000_000,
	})
	if want := 29.2; got != want {
		t.Fatalf("Cost() = %v, want %v", got, want)
	}

	if zero := p.Cost(Usage{}); zero != 0 {
		t.Errorf("Cost of no usage = %v, want 0", zero)
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
