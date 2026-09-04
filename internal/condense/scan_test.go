package condense

import (
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestScanCandidatesAcceptsCompleteBatches(t *testing.T) {
	t.Parallel()

	messages := []message.Message{
		{
			ID: "assistant", SessionID: "session", Role: message.Assistant,
			Parts: []message.ContentPart{
				message.ReasoningContent{Thinking: "why", StartedAt: 1, FinishedAt: 2},
				message.TextContent{Text: "working"},
				message.ToolCall{ID: "a", Name: "view", Input: `{"path":"a"}`, Finished: true},
				message.ToolCall{ID: "b", Name: "grep", Input: `{"pattern":"b"}`, Finished: true},
				message.Finish{Reason: message.FinishReasonToolUse, Time: 3},
			},
		},
		{
			ID: "tool-1", SessionID: "session", Role: message.Tool,
			Parts: []message.ContentPart{
				message.ToolResult{ToolCallID: "b", Name: "grep", Content: "βeta", Metadata: `{"m":1}`},
				message.Finish{Reason: message.FinishReasonEndTurn, Time: 4},
			},
		},
		{
			ID: "tool-2", SessionID: "session", Role: message.Tool,
			Parts: []message.ContentPart{
				message.TextContent{Text: "unsupported"},
			},
		},
	}

	// The unsupported second tool message protects the entire batch.
	require.Empty(t, scanCandidates(messages))

	messages[2].Parts = []message.ContentPart{
		message.ToolResult{ToolCallID: "a", Name: "view", Content: "alpha", IsError: true},
		message.Finish{Reason: message.FinishReasonEndTurn, Time: 5},
	}
	candidates := scanCandidates(messages)
	require.Len(t, candidates, 1)
	candidate := candidates[0]
	require.Equal(t, "assistant", candidate.FirstMessageID)
	require.Equal(t, "tool-2", candidate.LastMessageID)
	require.Len(t, candidate.Items, 2)
	require.Equal(t, "b", candidate.Items[0].ToolCallID, "results retain canonical order")
	require.Equal(t, 0, candidate.Items[0].PartOrdinal)
	require.Equal(t, "a", candidate.Items[1].ToolCallID)
	require.Equal(t, len([]rune("βetaalpha")), candidate.RawChars)
	require.Len(t, candidate.BatchKey, 64)
	require.Len(t, candidate.SourceHash, 64)
}

func TestScanCandidatesRejectsProtectedOrAmbiguousBatches(t *testing.T) {
	t.Parallel()

	base := []message.Message{
		{
			ID: "assistant", SessionID: "session", Role: message.Assistant,
			Parts: []message.ContentPart{
				message.ToolCall{ID: "call", Name: "view", Finished: true},
				message.Finish{Reason: message.FinishReasonToolUse},
			},
		},
		{
			ID: "tool", SessionID: "session", Role: message.Tool,
			Parts: []message.ContentPart{message.ToolResult{ToolCallID: "call", Name: "view", Content: "result"}},
		},
	}

	tests := []struct {
		name   string
		mutate func([]message.Message)
	}{
		{name: "unfinished call", mutate: func(msgs []message.Message) {
			msgs[0].Parts[0] = message.ToolCall{ID: "call", Name: "view"}
		}},
		{name: "missing finish", mutate: func(msgs []message.Message) {
			msgs[0].Parts = msgs[0].Parts[:1]
		}},
		{name: "duplicate finish", mutate: func(msgs []message.Message) {
			msgs[0].Parts = append(msgs[0].Parts, message.Finish{Reason: message.FinishReasonToolUse})
		}},
		{name: "duplicate call id", mutate: func(msgs []message.Message) {
			msgs[0].Parts = append(msgs[0].Parts, message.ToolCall{ID: "call", Name: "grep", Finished: true})
		}},
		{name: "orphan result", mutate: func(msgs []message.Message) {
			msgs[1].Parts = append(msgs[1].Parts, message.ToolResult{ToolCallID: "orphan", Name: "view", Content: "x"})
		}},
		{name: "duplicate result", mutate: func(msgs []message.Message) {
			msgs[1].Parts = append(msgs[1].Parts, message.ToolResult{ToolCallID: "call", Name: "view", Content: "x"})
		}},
		{name: "media result", mutate: func(msgs []message.Message) {
			msgs[1].Parts[0] = message.ToolResult{ToolCallID: "call", Name: "view", Content: "x", Data: "binary"}
		}},
		{name: "mimetype result", mutate: func(msgs []message.Message) {
			msgs[1].Parts[0] = message.ToolResult{ToolCallID: "call", Name: "view", Content: "x", MIMEType: "image/png"}
		}},
		{name: "recovery tool", mutate: func(msgs []message.Message) {
			msgs[0].Parts[0] = message.ToolCall{ID: "call", Name: recoveryToolName, Finished: true}
			msgs[1].Parts[0] = message.ToolResult{ToolCallID: "call", Name: recoveryToolName, Content: "x"}
		}},
		{name: "unsupported assistant part", mutate: func(msgs []message.Message) {
			msgs[0].Parts = append(msgs[0].Parts, message.ImageURLContent{URL: "image"})
		}},
		{name: "unsupported tool part", mutate: func(msgs []message.Message) {
			msgs[1].Parts = append(msgs[1].Parts, message.ShellCommand{Command: "pwd"})
		}},
		{name: "name mismatch", mutate: func(msgs []message.Message) {
			msgs[1].Parts[0] = message.ToolResult{ToolCallID: "call", Name: "grep", Content: "x"}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			msgs := make([]message.Message, len(base))
			for index := range base {
				msgs[index] = base[index].Clone()
			}
			tt.mutate(msgs)
			require.Empty(t, scanCandidates(msgs))
		})
	}
}

func TestScanCandidatesAcceptsOnlySuccessfulAssistantFinishes(t *testing.T) {
	t.Parallel()

	base := []message.Message{
		{
			ID: "assistant", SessionID: "session", Role: message.Assistant,
			Parts: []message.ContentPart{
				message.ToolCall{ID: "call", Name: "view", Finished: true},
				message.Finish{Reason: message.FinishReasonToolUse},
			},
		},
		{
			ID: "tool", SessionID: "session", Role: message.Tool,
			Parts: []message.ContentPart{message.ToolResult{ToolCallID: "call", Name: "view", Content: "result"}},
		},
	}

	for _, reason := range []message.FinishReason{message.FinishReasonToolUse, message.FinishReasonEndTurn} {
		messages := []message.Message{base[0].Clone(), base[1].Clone()}
		messages[0].Parts[1] = message.Finish{Reason: reason}
		require.Len(t, scanCandidates(messages), 1, reason)
	}
	for _, reason := range []message.FinishReason{
		message.FinishReasonError,
		message.FinishReasonCanceled,
		message.FinishReasonContentFilter,
		message.FinishReasonMaxTokens,
		message.FinishReasonUnknown,
		"unexpected",
	} {
		messages := []message.Message{base[0].Clone(), base[1].Clone()}
		messages[0].Parts[1] = message.Finish{Reason: reason}
		require.Empty(t, scanCandidates(messages), reason)
	}
}

func TestScanCandidatesUsesOccurrenceIdentity(t *testing.T) {
	t.Parallel()

	messages := []message.Message{
		{ID: "a1", SessionID: "s", Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{ID: "reused", Name: "view", Finished: true}, message.Finish{Reason: message.FinishReasonToolUse}}},
		{ID: "r1", SessionID: "s", Role: message.Tool, Parts: []message.ContentPart{message.TextContent{Text: "prefix"}, message.ToolResult{ToolCallID: "reused", Name: "view", Content: "one"}}},
		{ID: "a2", SessionID: "s", Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{ID: "reused", Name: "view", Finished: true}, message.Finish{Reason: message.FinishReasonToolUse}}},
		{ID: "r2", SessionID: "s", Role: message.Tool, Parts: []message.ContentPart{message.ToolResult{ToolCallID: "reused", Name: "view", Content: "two"}}},
	}

	// Unsupported text in the first tool message rejects only that batch.
	candidates := scanCandidates(messages)
	require.Len(t, candidates, 1)
	require.Equal(t, "r2", candidates[0].Items[0].MessageID)

	messages[1].Parts = []message.ContentPart{message.Finish{Reason: message.FinishReasonEndTurn}, message.ToolResult{ToolCallID: "reused", Name: "view", Content: "one"}}
	candidates = scanCandidates(messages)
	require.Len(t, candidates, 2)
	require.Equal(t, 1, candidates[0].Items[0].PartOrdinal)
	require.NotEqual(t, candidates[0].BatchKey, candidates[1].BatchKey)
	require.NotEqual(t, candidates[0].Items[0].SourceHash, candidates[1].Items[0].SourceHash)
}
