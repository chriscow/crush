package condense

import (
	"database/sql"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestQueryRecoversExactUnicodePages(t *testing.T) {
	t.Parallel()

	env := newTestEnvironment(t, &fixedSummarizer{summary: Summary{Text: "summary"}}, enabledOptions())
	content := "a¢界🌍β\nexact\x00tail"
	canonical := env.createBatch(t, content+strings.Repeat("x", 500))
	_, err := env.module.Project(t.Context(), env.sessionID, canonical)
	require.NoError(t, err)

	limit := 3
	var reconstructed strings.Builder
	offset := 0
	for {
		result, err := env.module.Query(t.Context(), env.sessionID, Query{Ref: "t1", Offset: offset, Limit: &limit})
		require.NoError(t, err)
		require.Equal(t, "ok", result.Status)
		require.Equal(t, "view", result.ToolName)
		require.Equal(t, "call-0", result.ToolCallID)
		require.False(t, result.IsError)
		require.Equal(t, offset, result.Offset)
		require.Equal(t, utf8.RuneCountInString(canonical[1].ToolResults()[0].Content), result.Total)
		reconstructed.WriteString(result.Content)
		if result.Complete {
			require.Nil(t, result.NextOffset)
			break
		}
		require.NotNil(t, result.NextOffset)
		offset = *result.NextOffset
	}
	require.Equal(t, canonical[1].ToolResults()[0].Content, reconstructed.String())
}

func TestQueryReturnsNotFoundWithoutLeakingReferenceState(t *testing.T) {
	t.Parallel()

	env := newTestEnvironment(t, &fixedSummarizer{summary: Summary{Text: "summary"}}, enabledOptions())
	canonical := env.createBatch(t, strings.Repeat("canonical", 100))
	_, err := env.module.Project(t.Context(), env.sessionID, canonical)
	require.NoError(t, err)

	invalid := []Query{
		{Ref: ""},
		{Ref: "x1"},
		{Ref: "t0"},
		{Ref: "t01"},
		{Ref: "t-1"},
		{Ref: "t9223372036854775808"},
		{Ref: "t1", Offset: -1},
	}
	zero := 0
	invalid = append(invalid, Query{Ref: "t1", Limit: &zero})
	for _, query := range invalid {
		result, err := env.module.Query(t.Context(), env.sessionID, query)
		require.NoError(t, err)
		require.Equal(t, Result{Status: "not_found", Ref: query.Ref}, result)
	}

	otherSessionService := session.NewService(env.queries, env.store.db)
	other, err := otherSessionService.Create(t.Context(), "other")
	require.NoError(t, err)
	result, err := env.module.Query(t.Context(), other.ID, Query{Ref: "t1"})
	require.NoError(t, err)
	require.Equal(t, Result{Status: "not_found", Ref: "t1"}, result)

	stored, err := env.messages.Get(t.Context(), canonical[1].ID)
	require.NoError(t, err)
	toolResult := stored.ToolResults()[0]
	toolResult.Content = "changed"
	stored.Parts[0] = toolResult
	require.NoError(t, env.messages.Update(t.Context(), stored))
	result, err = env.module.Query(t.Context(), env.sessionID, Query{Ref: "t1"})
	require.NoError(t, err)
	require.Equal(t, "not_found", result.Status)
}

func TestQueryReturnsNotFoundForTamperedDurableState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		tamper  func(t *testing.T, env *testEnvironment, sourceMessageID string)
		wantErr string
	}{
		{
			name: "tool call id mismatch",
			tamper: func(t *testing.T, env *testEnvironment, sourceMessageID string) {
				_, err := env.store.db.Exec(`UPDATE context_projection_items SET tool_call_id = 'other' WHERE ref_number = 1`)
				require.NoError(t, err)
			},
		},
		{
			name: "tool name mismatch",
			tamper: func(t *testing.T, env *testEnvironment, sourceMessageID string) {
				_, err := env.store.db.Exec(`UPDATE context_projection_items SET tool_name = 'other' WHERE ref_number = 1`)
				require.NoError(t, err)
			},
		},
		{
			name: "algorithm version mismatch",
			tamper: func(t *testing.T, env *testEnvironment, sourceMessageID string) {
				tx, err := env.store.db.BeginTx(t.Context(), nil)
				require.NoError(t, err)
				_, err = tx.ExecContext(t.Context(), `PRAGMA defer_foreign_keys = ON`)
				require.NoError(t, err)
				_, err = tx.ExecContext(t.Context(), `UPDATE context_projection_nodes SET algorithm_version = 2
					WHERE id IN (SELECT node_id FROM context_projection_items WHERE ref_number = 1)`)
				require.NoError(t, err)
				_, err = tx.ExecContext(t.Context(), `UPDATE context_projection_items SET algorithm_version = 2 WHERE ref_number = 1`)
				require.NoError(t, err)
				require.NoError(t, tx.Commit())
			},
		},
		{
			name: "source part ordinal out of range",
			tamper: func(t *testing.T, env *testEnvironment, sourceMessageID string) {
				stored, err := env.messages.Get(t.Context(), sourceMessageID)
				require.NoError(t, err)
				stored.Parts = nil
				require.NoError(t, env.messages.Update(t.Context(), stored))
			},
		},
		{
			name: "source part no longer a tool result",
			tamper: func(t *testing.T, env *testEnvironment, sourceMessageID string) {
				stored, err := env.messages.Get(t.Context(), sourceMessageID)
				require.NoError(t, err)
				stored.Parts = []message.ContentPart{message.TextContent{Text: "not a tool result"}}
				require.NoError(t, env.messages.Update(t.Context(), stored))
			},
		},
		{
			name: "corrupt canonical parts",
			tamper: func(t *testing.T, env *testEnvironment, sourceMessageID string) {
				_, err := env.store.db.Exec(`UPDATE messages SET parts = 'not json' WHERE id = ?`, sourceMessageID)
				require.NoError(t, err)
			},
			wantErr: "decode canonical source",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newTestEnvironment(t, &fixedSummarizer{summary: Summary{Text: "summary"}}, enabledOptions())
			canonical := env.createBatch(t, strings.Repeat("canonical", 50))
			_, err := env.module.Project(t.Context(), env.sessionID, canonical)
			require.NoError(t, err)

			tt.tamper(t, env, canonical[1].ID)
			if tt.wantErr != "" {
				_, err := env.module.Query(t.Context(), env.sessionID, Query{Ref: "t1"})
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			result, err := env.module.Query(t.Context(), env.sessionID, Query{Ref: "t1"})
			require.NoError(t, err)
			require.Equal(t, Result{Status: "not_found", Ref: "t1"}, result)
		})
	}
}

func TestQueryHandlesEmptyContentAndClampsLimit(t *testing.T) {
	t.Parallel()

	env := newTestEnvironment(t, nil, enabledOptions())
	assistant, err := env.messages.Create(t.Context(), env.sessionID, message.CreateMessageParams{
		Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{ID: "call", Name: "view", Finished: true}},
	})
	require.NoError(t, err)
	assistant.AddFinish(message.FinishReasonToolUse, "", "")
	require.NoError(t, env.messages.Update(t.Context(), assistant))
	tool, err := env.messages.Create(t.Context(), env.sessionID, message.CreateMessageParams{
		Role: message.Tool, Parts: []message.ContentPart{message.ToolResult{ToolCallID: "call", Name: "view", Content: ""}},
	})
	require.NoError(t, err)
	candidate := scanCandidates([]message.Message{assistant, tool})[0]
	claimed, ok, err := env.store.claim(t.Context(), candidate, env.module.now(), env.module.options.SummarizerTimeout+defaultLeaseMargin)
	require.NoError(t, err)
	require.True(t, ok)

	// Install an active empty item directly to exercise pagination independently
	// of the activation savings check.
	tx, err := env.store.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	qtx := env.queries.WithTx(tx)
	for ordinal, source := range candidate.Sources {
		require.NoError(t, qtx.CreateContextProjectionSource(t.Context(), db.CreateContextProjectionSourceParams{
			NodeID: claimed.nodeID, SessionID: env.sessionID, SourceMessageID: source.MessageID,
			SourcePartOrdinal: int64(source.PartOrdinal), PartKind: source.PartKind,
			SourceHash: source.SourceHash, Ordinal: int64(ordinal),
		}))
	}
	ref, err := qtx.ReserveContextProjectionRefs(t.Context(), 1)
	require.NoError(t, err)
	require.NoError(t, qtx.CreateContextProjectionItem(t.Context(), db.CreateContextProjectionItemParams{
		ID: "empty-item", NodeID: claimed.nodeID, SessionID: env.sessionID,
		SourceMessageID: candidate.Items[0].MessageID, SourcePartOrdinal: int64(candidate.Items[0].PartOrdinal),
		ToolCallID: "call", ToolName: "view", Description: "empty", RefNumber: ref,
		SourceHash: candidate.Items[0].SourceHash, Ordinal: 0, AlgorithmVersion: AlgorithmVersion,
	}))
	rows, err := qtx.ActivateContextProjectionClaim(t.Context(), db.ActivateContextProjectionClaimParams{
		Summary: "summary", ProjectedChars: 1, UpdatedAt: env.module.now().Unix(), ID: claimed.nodeID,
		SessionID: env.sessionID, AlgorithmVersion: AlgorithmVersion,
		ClaimToken: sql.NullString{String: claimed.token, Valid: true},
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), rows)
	require.NoError(t, tx.Commit())

	largeLimit := maxQueryLimit + 100
	result, err := env.module.Query(t.Context(), env.sessionID, Query{Ref: refString(ref), Offset: 10, Limit: &largeLimit})
	require.NoError(t, err)
	require.Equal(t, "ok", result.Status)
	require.Zero(t, result.Total)
	require.Zero(t, result.Offset)
	require.Zero(t, result.Returned)
	require.True(t, result.Complete)
	require.Empty(t, result.Content)
}
