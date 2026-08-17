package backend

import (
	"context"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/app"
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
