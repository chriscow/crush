package condense

import (
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestHashGoldenVectors(t *testing.T) {
	t.Parallel()

	candidate := scanCandidates([]message.Message{
		{
			ID: "assistant-1", SessionID: "session-1", Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "inspect"},
				message.ToolCall{ID: "call-1", Name: "view", Input: `{"file":"α"}`, ProviderExecuted: true, Finished: true},
				message.Finish{Reason: message.FinishReasonToolUse, Time: 123, Message: "m", Details: "d"},
			},
		},
		{
			ID: "tool-1", SessionID: "session-1", Role: message.Tool,
			Parts: []message.ContentPart{
				message.ToolResult{ToolCallID: "call-1", Name: "view", Content: "hello 🌍", Metadata: `{"exact":true}`, IsError: true},
				message.Finish{Reason: message.FinishReasonEndTurn, Time: 124},
			},
		},
	})[0]

	require.Equal(t, "74d64a1bdd437017fb4933a9c72d7af94eaba371bbb376a9bf705ed38acad078", candidate.BatchKey)
	require.Equal(t, "3bab18f6946ce7f0efcc5caef3d34f3e0ab434b4c1067caad6ec985c4113b115", candidate.SourceHash)
	require.Equal(t, "a1913aab229c421418a185bb42066921cc64b0db7870f6b3319948dd12a2969b", candidate.Items[0].SourceHash)
}

func TestHashesChangeForSourceFields(t *testing.T) {
	t.Parallel()

	base := []message.Message{
		{ID: "assistant", SessionID: "session", Role: message.Assistant, Parts: []message.ContentPart{
			message.TextContent{Text: "text"},
			message.ToolCall{ID: "call", Name: "view", Input: "{}", Finished: true},
			message.Finish{Reason: message.FinishReasonToolUse},
			message.ReasoningContent{Thinking: "thinking", Signature: "sig", ThoughtSignature: "thought", ToolID: "tool", StartedAt: 1, FinishedAt: 2},
		}},
		{ID: "tool", SessionID: "session", Role: message.Tool, Parts: []message.ContentPart{
			message.ToolResult{ToolCallID: "call", Name: "view", Content: "result", Metadata: "metadata"},
			message.Finish{Reason: message.FinishReasonEndTurn, Time: 5},
		}},
	}
	original := scanCandidates(base)[0]

	tests := []struct {
		name   string
		mutate func([]message.Message)
	}{
		{name: "assistant id", mutate: func(msgs []message.Message) { msgs[0].ID = "different" }},
		{name: "assistant text", mutate: func(msgs []message.Message) { msgs[0].Parts[0] = message.TextContent{Text: "different"} }},
		{name: "call input", mutate: func(msgs []message.Message) {
			msgs[0].Parts[1] = message.ToolCall{ID: "call", Name: "view", Input: `{"x":1}`, Finished: true}
		}},
		{name: "call provider executed", mutate: func(msgs []message.Message) {
			msgs[0].Parts[1] = message.ToolCall{ID: "call", Name: "view", Input: "{}", ProviderExecuted: true, Finished: true}
		}},
		{name: "assistant finish reason", mutate: func(msgs []message.Message) {
			msgs[0].Parts[2] = message.Finish{Reason: message.FinishReasonEndTurn}
		}},
		{name: "reasoning thinking", mutate: func(msgs []message.Message) {
			msgs[0].Parts[3] = message.ReasoningContent{Thinking: "different", Signature: "sig", ThoughtSignature: "thought", ToolID: "tool", StartedAt: 1, FinishedAt: 2}
		}},
		{name: "reasoning signature", mutate: func(msgs []message.Message) {
			msgs[0].Parts[3] = message.ReasoningContent{Thinking: "thinking", Signature: "different", ThoughtSignature: "thought", ToolID: "tool", StartedAt: 1, FinishedAt: 2}
		}},
		{name: "reasoning thought signature", mutate: func(msgs []message.Message) {
			msgs[0].Parts[3] = message.ReasoningContent{Thinking: "thinking", Signature: "sig", ThoughtSignature: "different", ToolID: "tool", StartedAt: 1, FinishedAt: 2}
		}},
		{name: "reasoning tool id", mutate: func(msgs []message.Message) {
			msgs[0].Parts[3] = message.ReasoningContent{Thinking: "thinking", Signature: "sig", ThoughtSignature: "thought", ToolID: "different", StartedAt: 1, FinishedAt: 2}
		}},
		{name: "reasoning started at", mutate: func(msgs []message.Message) {
			msgs[0].Parts[3] = message.ReasoningContent{Thinking: "thinking", Signature: "sig", ThoughtSignature: "thought", ToolID: "tool", StartedAt: 99, FinishedAt: 2}
		}},
		{name: "reasoning finished at", mutate: func(msgs []message.Message) {
			msgs[0].Parts[3] = message.ReasoningContent{Thinking: "thinking", Signature: "sig", ThoughtSignature: "thought", ToolID: "tool", StartedAt: 1, FinishedAt: 99}
		}},
		{name: "message id", mutate: func(msgs []message.Message) { msgs[1].ID = "different" }},
		{name: "content", mutate: func(msgs []message.Message) {
			msgs[1].Parts[0] = message.ToolResult{ToolCallID: "call", Name: "view", Content: "different", Metadata: "metadata"}
		}},
		{name: "metadata", mutate: func(msgs []message.Message) {
			msgs[1].Parts[0] = message.ToolResult{ToolCallID: "call", Name: "view", Content: "result", Metadata: "different"}
		}},
		{name: "error", mutate: func(msgs []message.Message) {
			msgs[1].Parts[0] = message.ToolResult{ToolCallID: "call", Name: "view", Content: "result", Metadata: "metadata", IsError: true}
		}},
		{name: "tool finish time", mutate: func(msgs []message.Message) {
			msgs[1].Parts[1] = message.Finish{Reason: message.FinishReasonEndTurn, Time: 999}
		}},
		{name: "tool finish details", mutate: func(msgs []message.Message) {
			msgs[1].Parts[1] = message.Finish{Reason: message.FinishReasonEndTurn, Time: 5, Details: "different"}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			msgs := []message.Message{base[0].Clone(), base[1].Clone()}
			tt.mutate(msgs)
			changed := scanCandidates(msgs)[0]
			require.NotEqual(t, original.SourceHash, changed.SourceHash)
			switch tt.name {
			case "assistant text", "call input":
				require.Equal(t, original.BatchKey, changed.BatchKey)
			case "content", "metadata", "error":
				require.Equal(t, original.BatchKey, changed.BatchKey)
				require.NotEqual(t, original.Items[0].SourceHash, changed.Items[0].SourceHash)
			}
		})
	}
}
