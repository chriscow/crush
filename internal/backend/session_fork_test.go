package backend

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/condense"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestForkSessionFlushesPendingMessages(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	queries := db.New(conn)
	sessions := session.NewService(queries, conn)
	messages := message.NewService(queries, message.WithDebounce(time.Hour))

	original, err := sessions.Create(t.Context(), "Original")
	require.NoError(t, err)
	originalMessage, err := messages.Create(t.Context(), original.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "before"}},
	})
	require.NoError(t, err)
	originalMessage.Parts = []message.ContentPart{message.TextContent{Text: "after"}}
	require.NoError(t, messages.Update(t.Context(), originalMessage))

	backend, _ := newTestBackend(t)
	workspace := &Workspace{
		App: &app.App{
			Sessions: sessions,
			Messages: messages,
		},
		ID:      "workspace",
		Path:    t.TempDir(),
		clients: make(map[string]*clientState),
	}
	backend.workspaces.Set(workspace.ID, workspace)

	forked, err := backend.ForkSession(context.Background(), workspace.ID, original.ID)
	require.NoError(t, err)
	forkedMessages, err := messages.List(t.Context(), forked.ID)
	require.NoError(t, err)
	require.Len(t, forkedMessages, 1)
	require.Equal(t, "after", forkedMessages[0].Content().Text)
}

func TestForkSessionEstablishesWriteBarrier(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	queries := db.New(conn)
	sessions := session.NewService(queries, conn)
	messages := message.NewService(queries, message.WithDebounce(time.Hour))
	original, err := sessions.Create(t.Context(), "Original")
	require.NoError(t, err)
	originalMessage, err := messages.Create(t.Context(), original.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "before"}},
	})
	require.NoError(t, err)
	originalMessage.Parts = []message.ContentPart{message.TextContent{Text: "accepted before barrier"}}
	require.NoError(t, messages.Update(t.Context(), originalMessage))

	require.NoError(t, messages.PrepareSessionSnapshot(t.Context(), original.ID))
	blocked := originalMessage.Clone()
	blocked.Parts = []message.ContentPart{message.TextContent{Text: "during barrier"}}
	require.ErrorContains(t, messages.Update(t.Context(), blocked), "being snapshotted")
	_, err = messages.Create(t.Context(), original.ID, message.CreateMessageParams{Role: message.User})
	require.ErrorContains(t, err, "being snapshotted")

	forked, err := sessions.Fork(t.Context(), original.ID)
	require.NoError(t, err)
	messages.FinishSessionSnapshot(original.ID)
	forkedMessages, err := messages.List(t.Context(), forked.ID)
	require.NoError(t, err)
	require.Len(t, forkedMessages, 1)
	require.Equal(t, "accepted before barrier", forkedMessages[0].Content().Text)
	persisted, err := messages.Get(t.Context(), originalMessage.ID)
	require.NoError(t, err)
	require.Equal(t, "accepted before barrier", persisted.Content().Text)

	blocked.Parts = []message.ContentPart{message.TextContent{Text: "after barrier"}}
	require.NoError(t, messages.Update(t.Context(), blocked))
	require.NoError(t, messages.Flush(t.Context(), blocked.ID))
	persisted, err = messages.Get(t.Context(), blocked.ID)
	require.NoError(t, err)
	require.Equal(t, "after barrier", persisted.Content().Text)
}

func TestForkSessionKeepsProjectionCanonical(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	queries := db.New(conn)
	sessions := session.NewService(queries, conn)
	messages := message.NewService(queries, message.WithDebounce(0))
	original, err := sessions.Create(t.Context(), "Original")
	require.NoError(t, err)
	assistant, err := messages.Create(t.Context(), original.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{message.ToolCall{
			ID: "call", Name: "view", Input: `{}`, Finished: true,
		}},
	})
	require.NoError(t, err)
	assistant.AddFinish(message.FinishReasonToolUse, "", "")
	require.NoError(t, messages.Update(t.Context(), assistant))
	toolMessage, err := messages.Create(t.Context(), original.ID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{message.ToolResult{
			ToolCallID: "call", Name: "view", Content: strings.Repeat("canonical", 100),
		}},
	})
	require.NoError(t, err)
	projection := condense.New(condense.NewStore(queries, conn), backendTestSummarizer{}, condense.Options{
		Enabled: true, SummarizerTimeout: time.Second,
	})
	_, err = projection.Project(t.Context(), original.ID, []message.Message{assistant, toolMessage})
	require.NoError(t, err)
	originalActive, err := queries.ListActiveContextProjectionNodes(t.Context(), db.ListActiveContextProjectionNodesParams{
		SessionID: original.ID, AlgorithmVersion: condense.AlgorithmVersion,
	})
	require.NoError(t, err)
	require.Len(t, originalActive, 1)
	originalItems, err := queries.ListContextProjectionItemsByNode(t.Context(), originalActive[0].ID)
	require.NoError(t, err)
	require.Len(t, originalItems, 1)

	backend, _ := newTestBackend(t)
	workspace := &Workspace{
		App: &app.App{Sessions: sessions, Messages: messages},
		ID:  "workspace", Path: t.TempDir(), clients: make(map[string]*clientState),
	}
	backend.workspaces.Set(workspace.ID, workspace)
	forked, err := backend.ForkSession(context.Background(), workspace.ID, original.ID)
	require.NoError(t, err)
	forkMessages, err := messages.List(t.Context(), forked.ID)
	require.NoError(t, err)
	require.Len(t, forkMessages, 2)
	for i := range forkMessages {
		require.NotEqual(t, []string{assistant.ID, toolMessage.ID}[i], forkMessages[i].ID)
	}

	for _, count := range []func(context.Context, string) (int64, error){
		queries.CountContextProjectionNodesBySession,
		queries.CountContextProjectionSourcesBySession,
		queries.CountContextProjectionItemsBySession,
	} {
		originalCount, err := count(t.Context(), original.ID)
		require.NoError(t, err)
		require.Positive(t, originalCount)
		forkCount, err := count(t.Context(), forked.ID)
		require.NoError(t, err)
		require.Zero(t, forkCount)
	}

	_, err = projection.Project(t.Context(), forked.ID, forkMessages)
	require.NoError(t, err)
	forkActive, err := queries.ListActiveContextProjectionNodes(t.Context(), db.ListActiveContextProjectionNodesParams{
		SessionID: forked.ID, AlgorithmVersion: condense.AlgorithmVersion,
	})
	require.NoError(t, err)
	require.Len(t, forkActive, 1)
	forkSources, err := queries.ListContextProjectionSourcesByNode(t.Context(), forkActive[0].ID)
	require.NoError(t, err)
	forkIDs := map[string]bool{forkMessages[0].ID: true, forkMessages[1].ID: true}
	for _, source := range forkSources {
		require.True(t, forkIDs[source.SourceMessageID])
	}
	forkItems, err := queries.ListContextProjectionItemsByNode(t.Context(), forkActive[0].ID)
	require.NoError(t, err)
	require.Len(t, forkItems, 1)
	require.Greater(t, forkItems[0].RefNumber, originalItems[0].RefNumber)
	_, err = queries.GetActiveContextProjectionItemByRef(t.Context(), db.GetActiveContextProjectionItemByRefParams{
		SessionID: forked.ID, RefNumber: originalItems[0].RefNumber,
	})
	require.Error(t, err)
}

type backendTestSummarizer struct{}

func (backendTestSummarizer) Summarize(_ context.Context, candidate condense.Candidate) (condense.Summary, error) {
	items := make([]condense.SummaryItem, len(candidate.Items))
	for i := range items {
		items[i] = condense.SummaryItem{Ordinal: i, Description: "description"}
	}
	return condense.Summary{Text: "summary", Items: items}, nil
}
