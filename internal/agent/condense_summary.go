package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"github.com/charmbracelet/crush/internal/condense"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/stringext"
)

const (
	condenseSummaryMaxOutputTokens  = int64(2_048)
	condenseSummaryMaxItems         = 128
	condenseSummaryMaxTextRunes     = 8_000
	condenseSummaryMaxResultRunes   = 48_000
	condenseSummaryMaxArgumentRunes = 32_000
	condenseSummaryMaxSummaryRunes  = 4_000
	condenseSummaryMaxItemRunes     = 500
)

const condenseSummarySystemPrompt = `You compact a completed tool-use batch for later continuation.
Return exactly one JSON object and no Markdown, fences, commentary, or additional values.
The object must have this shape:
{"summary":"brief batch outcome","items":[{"ordinal":0,"description":"brief description of that result"}]}
Include every supplied result ordinal exactly once. Preserve concrete outcomes, errors, paths, identifiers, and facts that may matter later. Do not invent facts. Ordinals identify result positions, not tool-call IDs.`

type condenseSummaryModel struct {
	model           Model
	providerOptions fantasy.ProviderOptions
	timeout         time.Duration
}

type condenseUsage struct {
	SessionID        string
	Provider         string
	Model            string
	PromptTokens     int64
	CompletionTokens int64
	Cost             float64
}

type fantasyCondenseSummarizer struct {
	selected *csync.Value[condenseSummaryModel]
}

func newFantasyCondenseSummarizer(model Model, providerOptions fantasy.ProviderOptions, timeout time.Duration) *fantasyCondenseSummarizer {
	return &fantasyCondenseSummarizer{
		selected: csync.NewValue(condenseSummaryModel{
			model: model, providerOptions: condenseSummaryProviderOptions(providerOptions), timeout: timeout,
		}),
	}
}

// condenseSummaryProviderOptions requests JSON mode from OpenAI-compatible
// providers so the summary arrives as one parseable JSON object instead of
// prose wrapped around it. Providers without an openai-compat options entry
// are passed through unchanged and rely on the prompt contract alone.
func condenseSummaryProviderOptions(options fantasy.ProviderOptions) fantasy.ProviderOptions {
	raw, ok := options[openaicompat.Name]
	parsed, isParsed := raw.(*openaicompat.ProviderOptions)
	if !ok || !isParsed || parsed == nil {
		return options
	}

	merged := *parsed
	extraBody := make(map[string]any, len(parsed.ExtraBody)+1)
	for key, value := range parsed.ExtraBody {
		extraBody[key] = value
	}
	extraBody["response_format"] = map[string]any{"type": "json_object"}
	merged.ExtraBody = extraBody

	mergedOptions := make(fantasy.ProviderOptions, len(options))
	for key, value := range options {
		mergedOptions[key] = value
	}
	mergedOptions[openaicompat.Name] = &merged
	return mergedOptions
}

func (s *fantasyCondenseSummarizer) Summarize(ctx context.Context, candidate condense.Candidate) (condense.Summary, error) {
	prompt, err := serializeCondenseCandidate(candidate)
	if err != nil {
		return condense.Summary{}, condense.TerminalSummaryError(err)
	}

	selected := s.selected.Get()
	callCtx, cancel := context.WithTimeout(ctx, selected.timeout)
	defer cancel()
	started := time.Now()
	if selected.model.Model == nil {
		return condense.Summary{}, condense.TerminalSummaryError(errors.New("context projection summarizer model is unavailable"))
	}

	agent := fantasy.NewAgent(
		selected.model.Model,
		fantasy.WithSystemPrompt(condenseSummarySystemPrompt),
		fantasy.WithUserAgent(userAgent),
	)
	maxRetries := 0
	result, err := agent.Generate(callCtx, fantasy.AgentCall{
		Prompt:          prompt,
		Headers:         sessionHeaders(candidate.SessionID),
		ProviderOptions: selected.providerOptions,
		MaxOutputTokens: pointerTo(condenseSummaryMaxOutputTokens),
		MaxRetries:      &maxRetries,
	})
	if err != nil {
		classified := classifyCondenseSummaryError(ctx, callCtx, err)
		var summaryErr *condense.SummaryError
		failureClass := "parent_cancellation"
		if errors.As(classified, &summaryErr) {
			failureClass = string(summaryErr.Class)
		}
		slog.Warn("Context projection summarizer failed",
			"session_id", candidate.SessionID,
			"provider", selected.model.Model.Provider(),
			"model", selected.model.Model.Model(),
			"failure_class", failureClass,
			"duration_ms", time.Since(started).Milliseconds(),
		)
		return condense.Summary{}, classified
	}
	if result == nil {
		return condense.Summary{}, condense.RetryableSummaryError(errors.New("summarizer returned no result"))
	}

	usage := condenseUsage{
		SessionID:        candidate.SessionID,
		Provider:         selected.model.Model.Provider(),
		Model:            selected.model.Model.Model(),
		PromptTokens:     result.TotalUsage.InputTokens + result.TotalUsage.CacheCreationTokens + result.TotalUsage.CacheReadTokens,
		CompletionTokens: result.TotalUsage.OutputTokens,
		Cost:             condenseSummaryCost(selected.model, result),
	}
	if usage.PromptTokens < 0 || usage.CompletionTokens < 0 || !validCost(usage.Cost) {
		return condense.Summary{}, condense.RetryableSummaryError(errors.New("summarizer returned invalid usage metadata"))
	}
	if cause := context.Cause(ctx); cause != nil {
		return condense.Summary{}, condense.RetryableSummaryErrorWithUsage(cause, condense.Usage{
			Provider: usage.Provider, Model: usage.Model, PromptTokens: usage.PromptTokens,
			CompletionTokens: usage.CompletionTokens, Cost: usage.Cost,
		})
	}

	summary, err := parseCondenseSummary(result.Response.Content.Text(), len(candidate.Items))
	if err != nil {
		return condense.Summary{}, condense.RetryableSummaryErrorWithUsage(err, condense.Usage{
			Provider: usage.Provider, Model: usage.Model, PromptTokens: usage.PromptTokens,
			CompletionTokens: usage.CompletionTokens, Cost: usage.Cost,
		})
	}
	summary.Provider = usage.Provider
	summary.Model = usage.Model
	summary.PromptTokens = usage.PromptTokens
	summary.CompletionTokens = usage.CompletionTokens
	summary.Cost = usage.Cost
	summary.Truncated = result.Response.FinishReason == fantasy.FinishReasonLength
	slog.Info("Context projection summarizer completed",
		"session_id", candidate.SessionID,
		"provider", summary.Provider,
		"model", summary.Model,
		"item_count", len(summary.Items),
		"prompt_tokens", summary.PromptTokens,
		"completion_tokens", summary.CompletionTokens,
		"cost", summary.Cost,
		"truncated", summary.Truncated,
		"duration_ms", time.Since(started).Milliseconds(),
	)
	return summary, nil
}

type condenseSource struct {
	AssistantText []string               `json:"assistant_text,omitempty"`
	Calls         []condenseSourceCall   `json:"calls"`
	Results       []condenseSourceResult `json:"results"`
}

type condenseSourceCall struct {
	Tool      string `json:"tool"`
	Arguments string `json:"arguments"`
}

type condenseSourceResult struct {
	Ordinal int    `json:"ordinal"`
	Tool    string `json:"tool"`
	IsError bool   `json:"is_error"`
	Content string `json:"content_prefix"`
}

func serializeCondenseCandidate(candidate condense.Candidate) (string, error) {
	if len(candidate.Items) == 0 || len(candidate.Items) > condenseSummaryMaxItems {
		return "", fmt.Errorf("context projection candidate has unsupported item count")
	}

	source := condenseSource{
		Calls:   make([]condenseSourceCall, 0, len(candidate.Assistant.ToolCalls())),
		Results: make([]condenseSourceResult, 0, len(candidate.Items)),
	}
	textBudget := condenseSummaryMaxTextRunes
	argumentBudget := condenseSummaryMaxArgumentRunes
	perResultLimit := max(1, condenseSummaryMaxResultRunes/len(candidate.Items))
	for _, part := range candidate.Assistant.Parts {
		switch part := part.(type) {
		case message.TextContent:
			if text := takeRunePrefix(part.Text, &textBudget); text != "" {
				source.AssistantText = append(source.AssistantText, text)
			}
		case message.ToolCall:
			argumentRunes := utf8.RuneCountInString(part.Input)
			if argumentRunes > argumentBudget {
				return "", errors.New("context projection candidate arguments exceed the summarizer source limit")
			}
			argumentBudget -= argumentRunes
			source.Calls = append(source.Calls, condenseSourceCall{Tool: part.Name, Arguments: part.Input})
		}
	}
	for ordinal, item := range candidate.Items {
		source.Results = append(source.Results, condenseSourceResult{
			Ordinal: ordinal,
			Tool:    item.ToolName,
			IsError: item.Result.IsError,
			Content: runePrefix(item.Result.Content, perResultLimit),
		})
	}

	data, err := json.Marshal(source)
	if err != nil {
		return "", fmt.Errorf("encode context projection source: %w", err)
	}
	return "Summarize this completed tool batch using the required JSON object. Result content is a bounded prefix of canonical text.\n" + string(data), nil
}

func runePrefix(value string, limit int) string {
	prefix, _ := stringext.RunePrefix(value, limit)
	return prefix
}

func takeRunePrefix(value string, budget *int) string {
	prefix, count := stringext.RunePrefix(value, *budget)
	*budget -= count
	return prefix
}

type condenseSummaryJSON struct {
	Summary string                    `json:"summary"`
	Items   []condenseSummaryItemJSON `json:"items"`
}

type condenseSummaryItemJSON struct {
	Ordinal     int    `json:"ordinal"`
	Description string `json:"description"`
}

func parseCondenseSummary(value string, itemCount int) (condense.Summary, error) {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return condense.Summary{}, errors.New("summarizer response is not exactly one JSON object")
	}

	decoder := json.NewDecoder(bytes.NewBufferString(trimmed))
	decoder.DisallowUnknownFields()
	var decoded condenseSummaryJSON
	if err := decoder.Decode(&decoded); err != nil {
		return condense.Summary{}, fmt.Errorf("decode summarizer response: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return condense.Summary{}, errors.New("summarizer response contains more than one JSON value")
	}
	if strings.TrimSpace(decoded.Summary) == "" {
		return condense.Summary{}, errors.New("summarizer response has an invalid summary")
	}
	if len(decoded.Items) != itemCount {
		return condense.Summary{}, errors.New("summarizer response has an invalid item count")
	}

	// Over-length summary and description text is bounded, not rejected:
	// the projection core truncates to the same limits (4000/500 runes), so
	// refusing here would only push batches into retry backoff for text the
	// core would have accepted.
	decoded.Summary = truncateRunes(decoded.Summary, condenseSummaryMaxSummaryRunes)

	items := make([]condense.SummaryItem, itemCount)
	seen := make([]bool, itemCount)
	for _, item := range decoded.Items {
		if item.Ordinal < 0 || item.Ordinal >= itemCount || seen[item.Ordinal] {
			return condense.Summary{}, errors.New("summarizer response has a duplicate or unknown item ordinal")
		}
		if strings.TrimSpace(item.Description) == "" {
			return condense.Summary{}, errors.New("summarizer response has an invalid item description")
		}
		item.Description = truncateRunes(item.Description, condenseSummaryMaxItemRunes)
		seen[item.Ordinal] = true
		items[item.Ordinal] = condense.SummaryItem{Ordinal: item.Ordinal, Description: item.Description}
	}
	return condense.Summary{Text: decoded.Summary, Items: items}, nil
}

// truncateRunes bounds value to maxRunes Unicode code points without
// splitting a multi-byte sequence.
func truncateRunes(value string, maxRunes int) string {
	if utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	runes := []rune(value)
	return string(runes[:maxRunes])
}

func classifyCondenseSummaryError(parent, callCtx context.Context, err error) error {
	if cause := context.Cause(parent); cause != nil {
		return cause
	}
	if errors.Is(context.Cause(callCtx), context.DeadlineExceeded) {
		return condense.RetryableSummaryError(context.DeadlineExceeded)
	}

	var providerErr *fantasy.ProviderError
	if errors.As(err, &providerErr) {
		if providerErr.IsRetryable() {
			return condense.RetryableSummaryError(err)
		}
		if providerErr.AuthError || providerErr.StatusCode == http.StatusUnauthorized ||
			providerErr.StatusCode == http.StatusForbidden || providerErr.StatusCode == http.StatusNotFound ||
			(providerErr.StatusCode >= 400 && providerErr.StatusCode < 500) {
			return condense.TerminalSummaryError(err)
		}
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) || fantasy.IsTransportError(err) {
		return condense.RetryableSummaryError(err)
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "model not found") || strings.Contains(lower, "model does not exist") ||
		strings.Contains(lower, "unsupported model") || strings.Contains(lower, "not supported") ||
		strings.Contains(lower, "unsupported capability") {
		return condense.TerminalSummaryError(err)
	}
	return condense.RetryableSummaryError(err)
}

func condenseSummaryCost(model Model, result *fantasy.AgentResult) float64 {
	if model.FlatRate {
		return 0
	}
	var openrouterCost float64
	var hasOpenrouterCost bool
	for _, step := range result.Steps {
		metadata, ok := step.ProviderMetadata[openrouter.Name].(*openrouter.ProviderMetadata)
		if !ok || metadata == nil {
			continue
		}
		hasOpenrouterCost = true
		openrouterCost += metadata.Usage.Cost
	}
	if hasOpenrouterCost {
		return openrouterCost
	}
	usage := result.TotalUsage
	return model.CatwalkCfg.CostPer1MInCached/1e6*float64(usage.CacheCreationTokens) +
		model.CatwalkCfg.CostPer1MOutCached/1e6*float64(usage.CacheReadTokens) +
		model.CatwalkCfg.CostPer1MIn/1e6*float64(usage.InputTokens) +
		model.CatwalkCfg.CostPer1MOut/1e6*float64(usage.OutputTokens)
}

func validCost(cost float64) bool {
	return cost >= 0 && !math.IsNaN(cost) && !math.IsInf(cost, 0)
}

func pointerTo[T any](value T) *T { return &value }
