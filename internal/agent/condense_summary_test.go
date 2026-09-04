package agent

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"github.com/charmbracelet/crush/internal/condense"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

type summaryCaptureModel struct {
	mu       sync.Mutex
	call     fantasy.Call
	response *fantasy.Response
	err      error
	block    bool
	provider string
	model    string
}

func (m *summaryCaptureModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	m.mu.Lock()
	m.call = call
	m.mu.Unlock()
	if m.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return m.response, m.err
}

func (*summaryCaptureModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return nil, errors.New("unexpected Stream call")
}

func (*summaryCaptureModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("unexpected GenerateObject call")
}

func (*summaryCaptureModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("unexpected StreamObject call")
}
func (m *summaryCaptureModel) Provider() string { return m.provider }
func (m *summaryCaptureModel) Model() string    { return m.model }

func condenseSummaryCandidate(result string) condense.Candidate {
	call := message.ToolCall{ID: "call-1", Name: "bash", Input: `{"command":"printf secret"}`, Finished: true}
	toolResult := message.ToolResult{ToolCallID: call.ID, Name: call.Name, Content: result, IsError: true}
	return condense.Candidate{
		SessionID: "session-1",
		Assistant: message.Message{Parts: []message.ContentPart{
			message.TextContent{Text: "Inspect the failure"},
			call,
		}},
		Items: []condense.Item{{ToolCallID: call.ID, ToolName: call.Name, Result: toolResult}},
	}
}

func TestFantasyCondenseSummarizerUsesStrictNoToolCall(t *testing.T) {
	model := &summaryCaptureModel{
		provider: "test-provider",
		model:    "test-model",
		response: &fantasy.Response{
			Content:      fantasy.ResponseContent{fantasy.TextContent{Text: `{"summary":"The command failed.","items":[{"ordinal":0,"description":"The shell command returned an error."}]}`}},
			FinishReason: fantasy.FinishReasonStop,
			Usage: fantasy.Usage{
				InputTokens: 5, CacheCreationTokens: 2, CacheReadTokens: 3, OutputTokens: 7,
			},
		},
	}
	summarizer := newFantasyCondenseSummarizer(Model{
		Model: model,
		CatwalkCfg: catwalk.Model{
			CostPer1MIn: 1, CostPer1MOut: 2,
		},
	}, nil, time.Second)

	summary, err := summarizer.Summarize(t.Context(), condenseSummaryCandidate("private output"))
	require.NoError(t, err)
	require.Equal(t, "The command failed.", summary.Text)
	require.Equal(t, "test-provider", summary.Provider)
	require.Equal(t, "test-model", summary.Model)
	require.Equal(t, int64(10), summary.PromptTokens)
	require.Equal(t, int64(7), summary.CompletionTokens)
	require.InDelta(t, 19.0/1_000_000, summary.Cost, 1e-12)
	require.False(t, summary.Truncated)

	model.mu.Lock()
	call := model.call
	model.mu.Unlock()
	require.Empty(t, call.Tools)
	require.NotNil(t, call.MaxOutputTokens)
	require.Equal(t, condenseSummaryMaxOutputTokens, *call.MaxOutputTokens)
	require.Len(t, call.Prompt, 2)
	require.Equal(t, fantasy.MessageRoleSystem, call.Prompt[0].Role)
	require.Equal(t, fantasy.MessageRoleUser, call.Prompt[1].Role)
	userText, ok := fantasy.AsMessagePart[fantasy.TextPart](call.Prompt[1].Content[0])
	require.True(t, ok)
	require.Contains(t, userText.Text, `"arguments":"{\"command\":\"printf secret\"}"`)
	require.Contains(t, userText.Text, `"is_error":true`)
	require.Contains(t, userText.Text, `"content_prefix":"private output"`)
}

func TestFantasyCondenseSummarizerReturnsUsageForValidAndInvalidResponses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		content   string
		wantError bool
	}{
		{name: "valid summary", content: `{"summary":"done","items":[{"ordinal":0,"description":"result"}]}`},
		{name: "invalid summary", content: `not json`, wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			model := &summaryCaptureModel{
				provider: "provider", model: "model",
				response: &fantasy.Response{
					Content: fantasy.ResponseContent{fantasy.TextContent{Text: tt.content}},
					Usage:   fantasy.Usage{InputTokens: 3, CacheCreationTokens: 2, CacheReadTokens: 1, OutputTokens: 4},
				},
			}
			summarizer := newFantasyCondenseSummarizer(
				Model{Model: model, CatwalkCfg: catwalk.Model{CostPer1MIn: 1, CostPer1MOut: 2}},
				nil,
				time.Second,
			)
			summary, err := summarizer.Summarize(t.Context(), condenseSummaryCandidate("result"))
			wantUsage := condense.Usage{
				Provider: "provider", Model: "model", PromptTokens: 6,
				CompletionTokens: 4, Cost: 11.0 / 1_000_000,
			}
			if tt.wantError {
				require.Error(t, err)
				var summaryErr *condense.SummaryError
				require.ErrorAs(t, err, &summaryErr)
				require.Equal(t, wantUsage, summaryErr.Usage)
			} else {
				require.NoError(t, err)
				require.Equal(t, wantUsage.Provider, summary.Provider)
				require.Equal(t, wantUsage.Model, summary.Model)
				require.Equal(t, wantUsage.PromptTokens, summary.PromptTokens)
				require.Equal(t, wantUsage.CompletionTokens, summary.CompletionTokens)
				require.Equal(t, wantUsage.Cost, summary.Cost)
			}
		})
	}
}

func TestFantasyCondenseSummarizerRejectsNonFiniteCostBeforeAccounting(t *testing.T) {
	t.Parallel()

	for _, cost := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		model := &summaryCaptureModel{
			provider: "provider", model: "model",
			response: &fantasy.Response{
				Content: fantasy.ResponseContent{fantasy.TextContent{Text: `{"summary":"done","items":[{"ordinal":0,"description":"result"}]}`}},
				Usage:   fantasy.Usage{InputTokens: 1},
			},
		}
		summarizer := newFantasyCondenseSummarizer(
			Model{Model: model, CatwalkCfg: catwalk.Model{CostPer1MIn: cost}},
			nil,
			time.Second,
		)
		_, err := summarizer.Summarize(t.Context(), condenseSummaryCandidate("result"))
		require.ErrorContains(t, err, "invalid usage metadata")
	}
}

func TestFantasyCondenseSummarizerBoundsSourceSerialization(t *testing.T) {
	model := &summaryCaptureModel{
		provider: "test", model: "test",
		response: &fantasy.Response{
			Content:      fantasy.ResponseContent{fantasy.TextContent{Text: `{"summary":"done","items":[{"ordinal":0,"description":"bounded"}]}`}},
			FinishReason: fantasy.FinishReasonStop,
		},
	}
	summarizer := newFantasyCondenseSummarizer(Model{Model: model}, nil, time.Second)
	_, err := summarizer.Summarize(t.Context(), condenseSummaryCandidate(strings.Repeat("界", condenseSummaryMaxResultRunes+100)))
	require.NoError(t, err)

	model.mu.Lock()
	call := model.call
	model.mu.Unlock()
	userText, ok := fantasy.AsMessagePart[fantasy.TextPart](call.Prompt[1].Content[0])
	require.True(t, ok)
	require.NotContains(t, userText.Text, strings.Repeat("界", condenseSummaryMaxResultRunes+1))
	require.Contains(t, userText.Text, strings.Repeat("界", 100))
}

func TestCondenseSummaryRunePrefixesPreserveBoundariesAndBudgets(t *testing.T) {
	t.Parallel()

	value := "a¢界🌍tail"
	require.Equal(t, "a¢界", runePrefix(value, 3))

	budget := 4
	require.Equal(t, "a¢界🌍", takeRunePrefix(value, &budget))
	require.Zero(t, budget)
	require.Empty(t, takeRunePrefix("unused", &budget))
}

func TestParseCondenseSummaryRejectsAnythingButOneExactObject(t *testing.T) {
	tests := []string{
		"```json\n{\"summary\":\"ok\",\"items\":[]}\n```",
		`before {"summary":"ok","items":[]}`,
		`{"summary":"ok","items":[]} {}`,
		`{"summary":"ok","items":[],"extra":true}`,
		`{"summary":"","items":[]}`,
		`{"summary":"ok","items":[{"ordinal":0,"description":"x"},{"ordinal":0,"description":"y"}]}`,
	}
	for _, value := range tests {
		_, err := parseCondenseSummary(value, 0)
		require.Error(t, err, value)
	}
}

func TestFantasyCondenseSummarizerReportsLengthStop(t *testing.T) {
	model := &summaryCaptureModel{
		provider: "test", model: "test",
		response: &fantasy.Response{
			Content:      fantasy.ResponseContent{fantasy.TextContent{Text: `{"summary":"done","items":[{"ordinal":0,"description":"bounded"}]}`}},
			FinishReason: fantasy.FinishReasonLength,
		},
	}
	summary, err := newFantasyCondenseSummarizer(Model{Model: model}, nil, time.Second).
		Summarize(t.Context(), condenseSummaryCandidate("result"))
	require.NoError(t, err)
	require.True(t, summary.Truncated)
}

func TestFantasyCondenseSummarizerTimeoutAndParentCancellation(t *testing.T) {
	t.Run("child timeout is retryable", func(t *testing.T) {
		model := &summaryCaptureModel{provider: "test", model: "test", block: true}
		_, err := newFantasyCondenseSummarizer(Model{Model: model}, nil, 10*time.Millisecond).
			Summarize(t.Context(), condenseSummaryCandidate("result"))
		var summaryErr *condense.SummaryError
		require.ErrorAs(t, err, &summaryErr)
		require.Equal(t, condense.SummaryFailureRetryable, summaryErr.Class)
	})

	t.Run("parent cancellation stays fatal to module", func(t *testing.T) {
		model := &summaryCaptureModel{provider: "test", model: "test", block: true}
		ctx, cancel := context.WithCancelCause(t.Context())
		cause := errors.New("stop the turn")
		cancel(cause)
		_, err := newFantasyCondenseSummarizer(Model{Model: model}, nil, time.Second).
			Summarize(ctx, condenseSummaryCandidate("result"))
		require.ErrorIs(t, err, cause)
		var summaryErr *condense.SummaryError
		require.False(t, errors.As(err, &summaryErr))
	})
}

func TestFantasyCondenseSummarizerClassifiesConfigurationFailure(t *testing.T) {
	model := &summaryCaptureModel{
		provider: "test", model: "test",
		err: &fantasy.ProviderError{StatusCode: 401, AuthError: true, Message: "unauthorized"},
	}
	_, err := newFantasyCondenseSummarizer(Model{Model: model}, nil, time.Second).
		Summarize(t.Context(), condenseSummaryCandidate("result"))
	var summaryErr *condense.SummaryError
	require.ErrorAs(t, err, &summaryErr)
	require.Equal(t, condense.SummaryFailureTerminal, summaryErr.Class)
}

type fakeNetworkError struct{}

func (fakeNetworkError) Error() string   { return "connection reset" }
func (fakeNetworkError) Timeout() bool   { return false }
func (fakeNetworkError) Temporary() bool { return false }

func TestClassifyCondenseSummaryError(t *testing.T) {
	t.Parallel()

	parentSentinel := errors.New("parent canceled")
	parent, cancelParent := context.WithCancelCause(context.Background())
	cancelParent(parentSentinel)

	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	t.Cleanup(cancelExpired)

	tests := []struct {
		name      string
		parent    context.Context
		callCtx   context.Context
		err       error
		wantCause error
		wantClass condense.SummaryFailureClass
	}{
		{
			name:   "parent cancellation wins over classification",
			parent: parent, callCtx: t.Context(),
			err: errors.New("provider down"), wantCause: parentSentinel,
		},
		{
			name:   "summarizer timeout is retryable",
			parent: context.Background(), callCtx: expired,
			err: errors.New("timeout"), wantClass: condense.SummaryFailureRetryable,
		},
		{
			name:   "transient provider error is retryable",
			parent: context.Background(), callCtx: t.Context(),
			err: &fantasy.ProviderError{TransientError: true}, wantClass: condense.SummaryFailureRetryable,
		},
		{
			name:   "rate limit status is retryable",
			parent: context.Background(), callCtx: t.Context(),
			err: &fantasy.ProviderError{StatusCode: 429}, wantClass: condense.SummaryFailureRetryable,
		},
		{
			name:   "server error status is retryable",
			parent: context.Background(), callCtx: t.Context(),
			err: &fantasy.ProviderError{StatusCode: 503}, wantClass: condense.SummaryFailureRetryable,
		},
		{
			name:   "auth error is terminal",
			parent: context.Background(), callCtx: t.Context(),
			err: &fantasy.ProviderError{AuthError: true}, wantClass: condense.SummaryFailureTerminal,
		},
		{
			name:   "unauthorized status is terminal",
			parent: context.Background(), callCtx: t.Context(),
			err: &fantasy.ProviderError{StatusCode: 401}, wantClass: condense.SummaryFailureTerminal,
		},
		{
			name:   "forbidden status is terminal",
			parent: context.Background(), callCtx: t.Context(),
			err: &fantasy.ProviderError{StatusCode: 403}, wantClass: condense.SummaryFailureTerminal,
		},
		{
			name:   "not found status is terminal",
			parent: context.Background(), callCtx: t.Context(),
			err: &fantasy.ProviderError{StatusCode: 404}, wantClass: condense.SummaryFailureTerminal,
		},
		{
			name:   "bad request status is terminal",
			parent: context.Background(), callCtx: t.Context(),
			err: &fantasy.ProviderError{StatusCode: 400}, wantClass: condense.SummaryFailureTerminal,
		},
		{
			name:   "network error is retryable",
			parent: context.Background(), callCtx: t.Context(),
			err: fakeNetworkError{}, wantClass: condense.SummaryFailureRetryable,
		},
		{
			name:   "unknown model message is terminal",
			parent: context.Background(), callCtx: t.Context(),
			err: errors.New("model not found: deepseek-v4"), wantClass: condense.SummaryFailureTerminal,
		},
		{
			name:   "unsupported capability message is terminal",
			parent: context.Background(), callCtx: t.Context(),
			err: errors.New("this provider does not support unsupported capability"), wantClass: condense.SummaryFailureTerminal,
		},
		{
			name:   "unclassified error is retryable",
			parent: context.Background(), callCtx: t.Context(),
			err: errors.New("something broke"), wantClass: condense.SummaryFailureRetryable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parent := tt.parent
			if parent == nil {
				parent = context.Background()
			}
			err := classifyCondenseSummaryError(parent, tt.callCtx, tt.err)
			if tt.wantCause != nil {
				require.ErrorIs(t, err, tt.wantCause)
				return
			}
			var summaryErr *condense.SummaryError
			require.ErrorAs(t, err, &summaryErr)
			require.Equal(t, tt.wantClass, summaryErr.Class)
		})
	}
}

func TestCondenseSummaryCost(t *testing.T) {
	t.Parallel()

	usage := fantasy.Usage{InputTokens: 100, OutputTokens: 50, CacheCreationTokens: 10, CacheReadTokens: 20}
	result := &fantasy.AgentResult{TotalUsage: usage}
	model := Model{CatwalkCfg: catwalk.Model{
		CostPer1MIn: 1, CostPer1MOut: 2, CostPer1MInCached: 3, CostPer1MOutCached: 4,
	}}
	want := float64(3*10+4*20+1*100+2*50) / 1_000_000
	require.InDelta(t, want, condenseSummaryCost(model, result), 1e-15)

	require.Zero(t, condenseSummaryCost(Model{FlatRate: true}, result))

	openrouterResult := &fantasy.AgentResult{
		TotalUsage: usage,
		Steps: []fantasy.StepResult{{
			Response: fantasy.Response{
				ProviderMetadata: fantasy.ProviderMetadata{
					openrouter.Name: &openrouter.ProviderMetadata{Usage: openrouter.UsageAccounting{Cost: 0.75}},
				},
			},
		}},
	}
	openrouterModel := Model{
		CatwalkCfg: catwalk.Model{CostPer1MIn: 1, CostPer1MOut: 2},
	}
	require.InDelta(t, 0.75, condenseSummaryCost(openrouterModel, openrouterResult), 1e-12)

	openrouterResult.Steps = append(openrouterResult.Steps, fantasy.StepResult{
		Response: fantasy.Response{
			ProviderMetadata: fantasy.ProviderMetadata{
				openrouter.Name: &openrouter.ProviderMetadata{Usage: openrouter.UsageAccounting{Cost: 0.25}},
			},
		},
	})
	require.InDelta(t, 1.0, condenseSummaryCost(openrouterModel, openrouterResult), 1e-12)

	unrelatedMetadata := &fantasy.AgentResult{
		TotalUsage: usage,
		Steps: []fantasy.StepResult{{
			Response: fantasy.Response{
				ProviderMetadata: fantasy.ProviderMetadata{
					"other": &openrouter.ProviderMetadata{Usage: openrouter.UsageAccounting{Cost: 5}},
				},
			},
		}},
	}
	require.InDelta(t, want, condenseSummaryCost(model, unrelatedMetadata), 1e-15)
}

func TestCondenseSummaryProviderOptionsRequestsJSONMode(t *testing.T) {
	t.Parallel()

	t.Run("injects response format for openai-compatible providers", func(t *testing.T) {
		t.Parallel()

		options := fantasy.ProviderOptions{
			openaicompat.Name: &openaicompat.ProviderOptions{
				ExtraBody: map[string]any{"enable_thinking": false},
			},
		}
		merged := condenseSummaryProviderOptions(options)
		parsed := merged[openaicompat.Name].(*openaicompat.ProviderOptions)
		require.Equal(t, map[string]any{"type": "json_object"}, parsed.ExtraBody["response_format"])
		require.Equal(t, false, parsed.ExtraBody["enable_thinking"], "existing extra body is preserved")

		original := options[openaicompat.Name].(*openaicompat.ProviderOptions)
		require.Nil(t, original.ExtraBody["response_format"], "the input options are not mutated")
	})

	t.Run("passes through providers without openai-compatible options", func(t *testing.T) {
		t.Parallel()

		options := fantasy.ProviderOptions{"other": &openaicompat.ProviderOptions{}}
		require.Equal(t, options, condenseSummaryProviderOptions(options))
		require.Equal(t, fantasy.ProviderOptions(nil), condenseSummaryProviderOptions(nil))
	})
}
