package cmd

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/condense"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

type statsSummarizer struct{}

func (statsSummarizer) Summarize(_ context.Context, candidate condense.Candidate) (condense.Summary, error) {
	items := make([]condense.SummaryItem, len(candidate.Items))
	for index := range items {
		items[index] = condense.SummaryItem{Ordinal: index, Description: "description"}
	}
	return condense.Summary{Text: "summary", Items: items}, nil
}

func newStatsTestDB(t *testing.T) (*sql.DB, *db.Queries, session.Service, message.Service) {
	t.Helper()
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	queries := db.New(conn)
	return conn, queries, session.NewService(queries, conn), message.NewService(queries, message.WithDebounce(0))
}

func activateStatsProjection(
	t *testing.T,
	conn *sql.DB,
	queries *db.Queries,
	sessions session.Service,
	messages message.Service,
) string {
	t.Helper()
	module := condense.New(condense.NewStore(queries, conn), statsSummarizer{}, condense.Options{
		Enabled: true, MinBatchChars: 0, KeepRecentBatches: 0, SummarizerTimeout: time.Second,
	})
	sess, err := sessions.Create(t.Context(), "Stats")
	require.NoError(t, err)
	_, err = messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{ID: "call-1", Name: "view", Input: `{}`, Finished: true},
			message.Finish{Reason: message.FinishReasonToolUse},
		},
	})
	require.NoError(t, err)
	_, err = messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{message.ToolResult{
			ToolCallID: "call-1", Name: "view", Content: strings.Repeat("canonical", 100),
		}},
	})
	require.NoError(t, err)
	canonical, err := messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	_, err = module.Project(t.Context(), sess.ID, canonical)
	require.NoError(t, err)
	return sess.ID
}

func TestGatherStatsIncludesProjectionSavings(t *testing.T) {
	conn, queries, sessions, messages := newStatsTestDB(t)
	activateStatsProjection(t, conn, queries, sessions, messages)

	stats, err := gatherStats(t.Context(), conn)
	require.NoError(t, err)
	require.Equal(t, int64(1), stats.Projection.ProjectedBatches)
	require.Positive(t, stats.Projection.RawChars)
	require.Positive(t, stats.Projection.ProjectedChars)
	require.Less(t, stats.Projection.ProjectedChars, stats.Projection.RawChars)
	require.Equal(t, stats.Projection.RawChars-stats.Projection.ProjectedChars, stats.Projection.CharsSaved)
}

func TestGatherStatsToleratesLegacyDatabaseWithoutProjectionTables(t *testing.T) {
	conn, queries, sessions, messages := newStatsTestDB(t)
	activateStatsProjection(t, conn, queries, sessions, messages)

	for _, table := range []string{
		"context_projection_usage_attempts",
		"context_projection_items",
		"context_projection_sources",
		"context_projection_nodes",
		"context_projection_ref_counter",
	} {
		_, err := conn.ExecContext(t.Context(), `DROP TABLE IF EXISTS `+table)
		require.NoError(t, err)
	}

	stats, err := gatherStats(t.Context(), conn)
	require.NoError(t, err)
	require.Zero(t, stats.Projection.ProjectedBatches)
	require.Zero(t, stats.Projection.CharsSaved)
}
