package workspace

import (
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestAppWorkspaceDeleteSessionDropsPendingMessageUpdates(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	queries := db.New(conn)
	sessions := session.NewService(queries, conn)
	messages := message.NewService(queries, message.WithDebounce(time.Hour))
	sess, err := sessions.Create(t.Context(), "workspace delete")
	require.NoError(t, err)
	msg, err := messages.Create(t.Context(), sess.ID, message.CreateMessageParams{Role: message.Assistant})
	require.NoError(t, err)
	msg.AppendContent("pending")
	require.NoError(t, messages.Update(t.Context(), msg))

	workspace := NewAppWorkspace(&app.App{Sessions: sessions, Messages: messages}, nil)
	require.NoError(t, workspace.DeleteSession(t.Context(), sess.ID))
	require.NoError(t, messages.FlushAll(t.Context()))

	_, err = sessions.Get(t.Context(), sess.ID)
	require.Error(t, err)
	_, err = messages.Get(t.Context(), msg.ID)
	require.Error(t, err)
}
