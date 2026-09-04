package condense

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

type testEnvironment struct {
	databaseDir string
	store       *Store
	module      *Module
	queries     *db.Queries
	messages    message.Service
	sessions    session.Service
	sessionID   string
}

func newTestEnvironment(t *testing.T, summarizer Summarizer, options Options) *testEnvironment {
	t.Helper()
	dataDir := t.TempDir()
	database, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
	})
	queries := db.New(database)
	sessions := session.NewService(queries, database)
	sess, err := sessions.Create(t.Context(), "projection test")
	require.NoError(t, err)
	store := NewStore(queries, database)
	module := New(store, summarizer, options)
	return &testEnvironment{
		databaseDir: dataDir, store: store, module: module, queries: queries,
		messages: message.NewService(queries, message.WithDebounce(0)), sessions: sessions,
		sessionID: sess.ID,
	}
}

func (e *testEnvironment) createBatch(t *testing.T, contents ...string) []message.Message {
	t.Helper()
	calls := make([]message.ContentPart, len(contents))
	for index := range contents {
		calls[index] = message.ToolCall{
			ID: fmt.Sprintf("call-%d", index), Name: "view",
			Input: `{"path":"file"}`, Finished: true,
		}
	}
	assistant, err := e.messages.Create(t.Context(), e.sessionID, message.CreateMessageParams{
		Role: message.Assistant, Parts: calls,
	})
	require.NoError(t, err)
	assistant.AddFinish(message.FinishReasonToolUse, "", "")
	require.NoError(t, e.messages.Update(t.Context(), assistant))

	parts := make([]message.ContentPart, len(contents))
	for index, content := range contents {
		parts[index] = message.ToolResult{
			ToolCallID: fmt.Sprintf("call-%d", index), Name: "view", Content: content,
			Metadata: `{"canonical":true}`, IsError: index%2 == 1,
		}
	}
	tool, err := e.messages.Create(t.Context(), e.sessionID, message.CreateMessageParams{
		Role: message.Tool, Parts: parts,
	})
	require.NoError(t, err)
	return []message.Message{assistant, tool}
}

type fixedSummarizer struct {
	summary Summary
	err     error
	calls   int
}

func (s *fixedSummarizer) Summarize(_ context.Context, candidate Candidate) (Summary, error) {
	s.calls++
	if s.err != nil {
		return Summary{}, s.err
	}
	if len(s.summary.Items) == 0 {
		s.summary.Items = make([]SummaryItem, len(candidate.Items))
		for index := range candidate.Items {
			s.summary.Items[index] = SummaryItem{Ordinal: index, Description: "description " + strings.Repeat("x", 10)}
		}
	}
	return s.summary, nil
}

func enabledOptions() Options {
	return Options{Enabled: true, MinBatchChars: 0, KeepRecentBatches: 0, SummarizerTimeout: time.Second}
}
