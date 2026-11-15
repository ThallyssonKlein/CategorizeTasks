// Package classifier asks Claude Haiku to pick one category for a task.
package classifier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

const model = "claude-haiku-5-5"

// ErrBadAnswer wraps responses that cannot be turned into a category
// (refusal, truncation, invalid JSON). Retrying the same input rarely helps.
var ErrBadAnswer = errors.New("classifier: unusable answer")

const systemPrompt = `You categorize items from a task list. Given a task's title and description, choose the single category from the provided list that best fits it.

Base the choice on what the work actually is, not on incidental words. When the task is vague, pick the closest fit; the list is the only set of valid answers.`

// Category is one option the model may choose.
type Category struct {
	Name        string
	Description string
}

type Classifier struct {
	client anthropic.Client
}

// New builds a classifier. The API key is read from ANTHROPIC_API_KEY.
func New(opts ...option.RequestOption) *Classifier {
	opts = append([]option.RequestOption{
		option.WithRequestTimeout(20 * time.Second),
		option.WithMaxRetries(2),
	}, opts...)
	return &Classifier{client: anthropic.NewClient(opts...)}
}

// Classify returns the name of the category that best fits the task. The
// answer is constrained by a JSON schema enum, so it is always one of
// categories.
func (c *Classifier) Classify(ctx context.Context, title, description string, categories []Category) (string, error) {
	if len(categories) == 0 {
		return "", errors.New("classifier: no categories to choose from")
	}

	names := make([]string, len(categories))
	var list strings.Builder
	for i, cat := range categories {
		names[i] = cat.Name
		list.WriteString("- " + cat.Name)
		if cat.Description != "" {
			list.WriteString(": " + cat.Description)
		}
		list.WriteString("\n")
	}
	if description == "" {
		description = "(no description)"
	}

	prompt := fmt.Sprintf("<categories>\n%s</categories>\n\n<task>\n<title>%s</title>\n<description>%s</description>\n</task>",
		list.String(), title, description)

	resp, err := c.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     model,
		MaxTokens: 1024,
		System:    []anthropic.TextBlockParam{{Text: systemPrompt}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffortLow,
			Format: anthropic.JSONOutputFormatParam{
				Schema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"category": map[string]any{"type": "string", "enum": names},
					},
					"required":             []string{"category"},
					"additionalProperties": false,
				},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("classifier: %w", err)
	}

	switch resp.StopReason {
	case anthropic.StopReasonRefusal:
		return "", fmt.Errorf("%w: model refused (%s)", ErrBadAnswer, resp.StopDetails.Category)
	case anthropic.StopReasonMaxTokens:
		return "", fmt.Errorf("%w: hit max_tokens before answering", ErrBadAnswer)
	}

	var text strings.Builder
	for _, block := range resp.Content {
		if b, ok := block.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(b.Text)
		}
	}
	var out struct {
		Category string `json:"category"`
	}
	if err := json.Unmarshal([]byte(text.String()), &out); err != nil {
		return "", fmt.Errorf("%w: parse %q: %v", ErrBadAnswer, text.String(), err)
	}
	for _, n := range names {
		if n == out.Category {
			return n, nil
		}
	}
	return "", fmt.Errorf("%w: unknown category %q", ErrBadAnswer, out.Category)
}
