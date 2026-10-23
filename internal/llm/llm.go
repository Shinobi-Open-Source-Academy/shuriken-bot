// Package llm wraps the Anthropic API behind an interface.
//
// Two things in here are load bearing for correctness:
//
//   - The findings schema is enforced with structured outputs
//     (output_config.format), not with a forced tool call. Forced tool use
//     (tool_choice any/tool) is rejected with a 400 on current models, so the
//     schema guarantee has to come from output_config.
//   - Everything from the pull request is passed as delimited, clearly labelled
//     untrusted data. The system prompt tells the model never to follow
//     instructions found inside it.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Finding is one piece of feedback as the model reported it. It is validated
// before anything is done with it.
type Finding struct {
	Path         string  `json:"path"`
	Line         int     `json:"line"`
	Side         string  `json:"side"`
	Severity     string  `json:"severity"`
	Confidence   float64 `json:"confidence"`
	Title        string  `json:"title"`
	Explanation  string  `json:"explanation"`
	Concept      string  `json:"concept"`
	SuggestedFix string  `json:"suggested_fix,omitempty"`
	Mechanical   bool    `json:"mechanical"`
}

// ReviewOutput is the whole structured response.
type ReviewOutput struct {
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings"`
}

// Usage is the token accounting for one review.
type Usage struct {
	InputTokens              int64
	OutputTokens             int64
	CacheReadInputTokens     int64
	CacheCreationInputTokens int64
	ToolCalls                int
}

// Result is a completed model run.
type Result struct {
	Output  ReviewOutput
	Usage   Usage
	Retried bool
	// ToolCallsCapped reports that the model hit the tool budget and was told
	// to finish with what it had.
	ToolCallsCapped bool
}

// FileReader resolves the model's read_file tool against the pull request head
// commit. Returning an error is fine: the error text goes back to the model as
// a tool result so it can adapt.
type FileReader func(ctx context.Context, path string) (string, error)

// Request is one review job.
type Request struct {
	// SystemStatic is the frozen teaching prompt. It is cached.
	SystemStatic string
	// Conventions is the repository's CONTRIBUTING.md and .github/shuriken.md.
	// Cached separately so it survives across reviews of the same repo.
	Conventions string
	// UserContent is the volatile part: PR title, description and diff.
	UserContent string
	// ReadFile backs the read_file tool. When nil the tool is not offered.
	ReadFile FileReader
}

// Client is the model surface the review pipeline depends on.
type Client interface {
	Review(ctx context.Context, req Request) (*Result, error)
}

// Pricing is USD per million tokens, used only for cost logging.
type Pricing struct {
	InputPerMTok      float64
	OutputPerMTok     float64
	CacheReadPerMTok  float64
	CacheWritePerMTok float64
}

// Cost estimates what a review cost. It is an estimate for observability, not
// a billing figure.
func (p Pricing) Cost(u Usage) float64 {
	const perMillion = 1_000_000.0
	return float64(u.InputTokens)/perMillion*p.InputPerMTok +
		float64(u.OutputTokens)/perMillion*p.OutputPerMTok +
		float64(u.CacheReadInputTokens)/perMillion*p.CacheReadPerMTok +
		float64(u.CacheCreationInputTokens)/perMillion*p.CacheWritePerMTok
}

// Options configure the Anthropic client.
type Options struct {
	APIKey          string
	Model           string
	Effort          string
	MaxOutputTokens int64
	MaxToolCalls    int
	// MaxFileBytes caps a single read_file result, so one large file cannot
	// consume the whole context window.
	MaxFileBytes int
}

type anthropicClient struct {
	api  anthropic.Client
	opts Options
}

// New builds a Client backed by the Anthropic API.
func New(opts Options) Client {
	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = 60_000
	}
	return &anthropicClient{
		api:  anthropic.NewClient(option.WithAPIKey(opts.APIKey)),
		opts: opts,
	}
}

// readFileToolName is referenced in the system prompt, so keep them in sync.
const readFileToolName = "read_file"

// FindingsSchema is the JSON schema the model's response must satisfy.
//
// Numeric range and string length constraints are intentionally absent: the
// structured output dialect does not support minimum/maximum/minLength, so
// confidence and severity are range checked in Go after decoding.
func FindingsSchema() map[string]any {
	finding := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required": []string{
			"path", "line", "side", "severity", "confidence",
			"title", "explanation", "concept", "mechanical",
		},
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Repository-relative path, exactly as it appears in the diff.",
			},
			"line": map[string]any{
				"type":        "integer",
				"description": "Line number in the file version named by side. Must be a line present in the diff.",
			},
			"side": map[string]any{
				"type":        "string",
				"enum":        []string{"RIGHT", "LEFT"},
				"description": "RIGHT for the new version of the file, LEFT for the old one.",
			},
			"severity": map[string]any{
				"type": "string",
				"enum": []string{"high", "medium", "low"},
				"description": "high: a bug, data loss or security problem. " +
					"medium: likely incorrect or fragile. low: worth knowing, not urgent.",
			},
			"confidence": map[string]any{
				"type":        "number",
				"description": "0 to 1. How certain you are this is a real problem. Be honest; low confidence findings are discarded.",
			},
			"title": map[string]any{
				"type":        "string",
				"description": "One short line naming the problem.",
			},
			"explanation": map[string]any{
				"type":        "string",
				"description": "The teaching part: what is wrong, why it matters, and what to think about. Markdown.",
			},
			"concept": map[string]any{
				"type": "string",
				"description": "The underlying idea in two to four lowercase words, " +
					"e.g. \"error handling\", \"input validation\", \"race condition\". " +
					"Reuse the same wording for the same idea so it can be tracked over time.",
			},
			"suggested_fix": map[string]any{
				"type":        "string",
				"description": "Optional. The replacement code only, no fences and no prose.",
			},
			"mechanical": map[string]any{
				"type": "boolean",
				"description": "True only for a low risk mechanical fix where showing the " +
					"corrected code teaches as well as describing it.",
			},
		},
	}

	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"summary", "findings"},
		"properties": map[string]any{
			"summary": map[string]any{
				"type":        "string",
				"description": "Two to four sentences to the author. If nothing important was found, say so plainly.",
			},
			"findings": map[string]any{
				"type":        "array",
				"items":       finding,
				"description": "Only problems you are confident are real. An empty array is a valid and good answer.",
			},
		},
	}
}

func (c *anthropicClient) Review(ctx context.Context, req Request) (*Result, error) {
	res, err := c.run(ctx, req, false)
	if err == nil {
		return res, nil
	}

	// One retry, and only for a malformed response. A transport or status
	// error is already retried with backoff inside the SDK.
	if !errors.Is(err, errMalformedOutput) {
		return nil, err
	}
	res, retryErr := c.run(ctx, req, true)
	if retryErr != nil {
		return nil, fmt.Errorf("model output invalid after retry: %w", retryErr)
	}
	res.Retried = true
	return res, nil
}

var errMalformedOutput = errors.New("llm: model output did not match the findings schema")

func (c *anthropicClient) run(ctx context.Context, req Request, isRetry bool) (*Result, error) {
	system := []anthropic.TextBlockParam{{Text: req.SystemStatic}}
	if strings.TrimSpace(req.Conventions) != "" {
		system = append(system, anthropic.TextBlockParam{Text: req.Conventions})
	}
	// One breakpoint at the end of the system blocks caches the tool
	// definitions and the whole system prompt together. The volatile diff sits
	// after it in messages, so it never invalidates the cached prefix.
	system[len(system)-1].CacheControl = anthropic.NewCacheControlEphemeralParam()

	userText := req.UserContent
	if isRetry {
		userText += "\n\nYour previous response did not match the required output schema. " +
			"Return only an object with \"summary\" and \"findings\"."
	}

	params := anthropic.MessageNewParams{
		Model:     c.opts.Model,
		MaxTokens: c.opts.MaxOutputTokens,
		System:    system,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(userText)),
		},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffort(c.opts.Effort),
			Format: anthropic.JSONOutputFormatParam{Schema: FindingsSchema()},
		},
	}

	if req.ReadFile != nil {
		tool := anthropic.ToolParam{
			Name: readFileToolName,
			Description: anthropic.String(
				"Read a file from the pull request's head commit. Use it when the diff " +
					"does not show enough surrounding code to be sure about a finding.",
			),
			// Guarantees the arguments validate against the schema, which is
			// what forced tool use used to provide.
			Strict: anthropic.Bool(true),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Repository-relative path.",
					},
				},
				Required:    []string{"path"},
				ExtraFields: map[string]any{"additionalProperties": false},
			},
		}
		params.Tools = []anthropic.ToolUnionParam{{OfTool: &tool}}
	}

	var usage Usage
	capped := false

	for {
		msg, err := c.api.Messages.New(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("anthropic messages: %w", err)
		}

		usage.InputTokens += msg.Usage.InputTokens
		usage.OutputTokens += msg.Usage.OutputTokens
		usage.CacheReadInputTokens += msg.Usage.CacheReadInputTokens
		usage.CacheCreationInputTokens += msg.Usage.CacheCreationInputTokens

		if msg.StopReason == anthropic.StopReasonRefusal {
			return nil, fmt.Errorf("model declined the request (category %q)", msg.StopDetails.Category)
		}

		params.Messages = append(params.Messages, msg.ToParam())

		if msg.StopReason != anthropic.StopReasonToolUse {
			out, perr := parseOutput(msg)
			if perr != nil {
				return nil, perr
			}
			return &Result{Output: out, Usage: usage, ToolCallsCapped: capped}, nil
		}

		results := make([]anthropic.ContentBlockParamUnion, 0, 4)
		for _, block := range msg.Content {
			tu, ok := block.AsAny().(anthropic.ToolUseBlock)
			if !ok {
				continue
			}
			usage.ToolCalls++

			text, isErr := c.runTool(ctx, req, tu)
			results = append(results, anthropic.NewToolResultBlock(tu.ID, text, isErr))
		}

		if len(results) == 0 {
			// Claimed tool use but produced no tool_use block. Nothing to send
			// back; treat it as malformed rather than looping forever.
			return nil, errMalformedOutput
		}
		params.Messages = append(params.Messages, anthropic.NewUserMessage(results...))

		// Budget spent: forbid further calls and let it answer with what it has.
		// tool_choice "none" is still accepted on current models, unlike
		// "any" and "tool".
		if usage.ToolCalls >= c.opts.MaxToolCalls && !capped {
			capped = true
			params.ToolChoice = anthropic.ToolChoiceUnionParam{
				OfNone: &anthropic.ToolChoiceNoneParam{},
			}
			params.Messages = append(params.Messages, anthropic.NewUserMessage(
				anthropic.NewTextBlock(
					"You have used your file reading budget. Produce your final review now "+
						"from what you have already seen, and leave out anything you could not verify.",
				),
			))
		}
	}
}

func (c *anthropicClient) runTool(ctx context.Context, req Request, tu anthropic.ToolUseBlock) (string, bool) {
	if tu.Name != readFileToolName {
		return fmt.Sprintf("unknown tool %q", tu.Name), true
	}

	var in struct {
		Path string `json:"path"`
	}
	// Eager parsing is off, but a truncated response can still produce
	// unparseable input, so this is always checked.
	if err := json.Unmarshal([]byte(tu.JSON.Input.Raw()), &in); err != nil {
		return "could not parse tool input as JSON", true
	}
	if strings.TrimSpace(in.Path) == "" {
		return "path must not be empty", true
	}

	content, err := req.ReadFile(ctx, in.Path)
	if err != nil {
		// The error text is deliberately plain: it is model-visible.
		return fmt.Sprintf("could not read %q: %v", in.Path, err), true
	}
	if len(content) > c.opts.MaxFileBytes {
		content = content[:c.opts.MaxFileBytes] + "\n... [truncated]"
	}
	return content, false
}

// parseOutput pulls the structured JSON out of the response. With
// output_config.format set, the model's text content is the JSON document.
func parseOutput(msg *anthropic.Message) (ReviewOutput, error) {
	var buf strings.Builder
	for _, block := range msg.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			buf.WriteString(tb.Text)
		}
	}
	raw := strings.TrimSpace(buf.String())
	if raw == "" {
		return ReviewOutput{}, fmt.Errorf("%w: empty response", errMalformedOutput)
	}

	var out ReviewOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return ReviewOutput{}, fmt.Errorf("%w: %w", errMalformedOutput, err)
	}
	return out, nil
}
