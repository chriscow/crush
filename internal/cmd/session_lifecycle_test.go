package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/condense"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestNewSessionServicesWiresProjectionDeletionLifecycle(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})
	cfg := config.NewTestStore(&config.Config{
		Options:   &config.Options{DataDirectory: dataDir},
		Providers: csync.NewMap[string, config.ProviderConfig](),
	})
	svc := newSessionServices(conn, cfg)

	sess, err := svc.sessions.Create(t.Context(), "command lifecycle")
	require.NoError(t, err)
	assistant, err := svc.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{message.ToolCall{
			ID: "call", Name: "view", Input: `{}`, Finished: true,
		}},
	})
	require.NoError(t, err)
	assistant.AddFinish(message.FinishReasonToolUse, "", "")
	require.NoError(t, svc.messages.Update(t.Context(), assistant))
	toolMessage, err := svc.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{message.ToolResult{
			ToolCallID: "call", Name: "view", Content: strings.Repeat("canonical", 100),
		}},
	})
	require.NoError(t, err)

	queries := db.New(conn)
	projection := condense.New(condense.NewStore(queries, conn), commandTestSummarizer{}, condense.Options{
		Enabled: true, SummarizerTimeout: time.Second,
	})
	_, err = projection.Project(t.Context(), sess.ID, []message.Message{assistant, toolMessage})
	require.NoError(t, err)

	require.NoError(t, deleteSession(t.Context(), svc, sess.ID))
	for _, count := range []func(context.Context, string) (int64, error){
		queries.CountContextProjectionNodesBySession,
		queries.CountContextProjectionSourcesBySession,
		queries.CountContextProjectionItemsBySession,
	} {
		value, err := count(t.Context(), sess.ID)
		require.NoError(t, err)
		require.Zero(t, value)
	}
}

type commandTestSummarizer struct{}

func (commandTestSummarizer) Summarize(_ context.Context, candidate condense.Candidate) (condense.Summary, error) {
	items := make([]condense.SummaryItem, len(candidate.Items))
	for i := range items {
		items[i] = condense.SummaryItem{Ordinal: i, Description: "description"}
	}
	return condense.Summary{Text: "summary", Items: items}, nil
}
