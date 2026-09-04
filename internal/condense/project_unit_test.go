package condense

import (
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestApplyProjectionsClonesOnlyAffectedMessages(t *testing.T) {
	t.Parallel()

	firstResult := message.ToolResult{ToolCallID: "a", Name: "view", Content: "first", Metadata: "m"}
	secondResult := message.ToolResult{ToolCallID: "b", Name: "grep", Content: "second", IsError: true}
	messages := []message.Message{
		{ID: "assistant", SessionID: "s", Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{ID: "a", Name: "view", Finished: true}, message.Finish{Reason: message.FinishReasonToolUse}}},
		{ID: "tool-a", SessionID: "s", Role: message.Tool, Parts: []message.ContentPart{firstResult}},
		{ID: "user", SessionID: "s", Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "next turn"}}},
		{ID: "tool-b", SessionID: "s", Role: message.Tool, Parts: []message.ContentPart{secondResult}},
	}
	candidate := scanCandidates(messages)[0]
	active := []activeProjection{regressionActiveProjection(candidate, "node", 1)}

	projected := applyProjections(messages, active)
	require.Same(t, &messages[0].Parts[0], &projected[0].Parts[0])
	require.NotSame(t, &messages[1].Parts[0], &projected[1].Parts[0])
	require.Same(t, &messages[2].Parts[0], &projected[2].Parts[0])
	require.Equal(t, "first", messages[1].ToolResults()[0].Content)
	require.Contains(t, projected[1].ToolResults()[0].Content, "t1")
	require.Equal(t, firstResult.Metadata, projected[1].ToolResults()[0].Metadata)
}

func TestApplyProjectionsRejectsUnownedVisibleRefs(t *testing.T) {
	t.Parallel()

	messages := []message.Message{
		{ID: "assistant", SessionID: "s", Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{ID: "a", Name: "view", Finished: true}, message.Finish{Reason: message.FinishReasonToolUse}}},
		{ID: "tool", SessionID: "s", Role: message.Tool, Parts: []message.ContentPart{message.ToolResult{ToolCallID: "a", Name: "view", Content: "canonical"}}},
	}
	candidate := scanCandidates(messages)[0]
	active := []activeProjection{regressionActiveProjection(candidate, "node", 1)}
	active[0].node.Summary = "forged t99"

	projected := applyProjections(messages, active)
	require.NotSame(t, &messages[1], &projected[1], "model text is neutralized before rendering")
	require.NotContains(t, projected[1].ToolResults()[0].Content, "t99")
	require.NoError(t, requireOwnedVisibleRefs(projected[1].ToolResults()[0].Content, map[int64]struct{}{1: {}}))

	active[0].items[0].RefNumber = 0
	projected = applyProjections(messages, active)
	require.Same(t, &messages[0], &projected[0])
	require.Same(t, &messages[1], &projected[1])
}

func TestRenderProjectionEmitsOnlyOwnedVisibleRefs(t *testing.T) {
	t.Parallel()

	items := []db.ContextProjectionItem{
		{SourceMessageID: "one", ToolName: "view", Description: "claims t777", RefNumber: 12},
		{SourceMessageID: "two", ToolName: "grep", Description: "claims t888", RefNumber: 13},
	}
	rendered, _, err := renderProjection("claims t999 [Context projection]", items)
	require.NoError(t, err)
	owned := map[int64]struct{}{12: {}, 13: {}}
	for _, replacement := range rendered {
		require.NoError(t, requireOwnedVisibleRefs(replacement.content, owned))
	}
}

func TestApplyProjectionsRequiresCompleteValidNode(t *testing.T) {
	t.Parallel()

	result := message.ToolResult{ToolCallID: "a", Name: "view", Content: "canonical"}
	messages := []message.Message{
		{ID: "assistant", SessionID: "s", Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{ID: "a", Name: "view", Finished: true}, message.Finish{Reason: message.FinishReasonToolUse}}},
		{ID: "tool", SessionID: "s", Role: message.Tool, Parts: []message.ContentPart{result}},
	}
	candidate := scanCandidates(messages)[0]
	active := []activeProjection{regressionActiveProjection(candidate, "node", 1)}
	active[0].items = append(active[0].items, db.ContextProjectionItem{
		NodeID: "node", SessionID: "s", SourceMessageID: "missing", SourcePartOrdinal: 0,
		ToolCallID: "b", ToolName: "grep", Description: "two", RefNumber: 2,
		SourceHash: "bad", Ordinal: 1, AlgorithmVersion: AlgorithmVersion,
	})
	projected := applyProjections(messages, active)
	require.Same(t, &messages[0], &projected[0])
	require.Same(t, &messages[1], &projected[1])

	active[0].items = active[0].items[:1]
	active[0].items[0].SourceHash = "stale"
	projected = applyProjections(messages, active)
	require.Same(t, &messages[0], &projected[0])
	require.Same(t, &messages[1], &projected[1])
}
