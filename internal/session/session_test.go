package session

import (
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestEstimatedUsageStateSurvivesFetchModifySave(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	sessions := NewService(db.New(conn), conn)

	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	created.PromptTokens = 100
	created.CompletionTokens = 50
	created.EstimatedUsage = true

	saved, err := sessions.Save(t.Context(), created)
	require.NoError(t, err)
	require.True(t, saved.EstimatedUsage)

	fetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.True(t, fetched.EstimatedUsage)

	fetched.Todos = []Todo{{
		Content:    "Check estimate state",
		Status:     TodoStatusInProgress,
		ActiveForm: "Checking estimate state",
	}}

	updated, err := sessions.Save(t.Context(), fetched)
	require.NoError(t, err)
	require.True(t, updated.EstimatedUsage)

	refetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.True(t, refetched.EstimatedUsage)
}

func TestEstimatedUsageStateCanBeClearedByExplicitSave(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	sessions := NewService(db.New(conn), conn)

	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	created.PromptTokens = 100
	created.CompletionTokens = 50
	created.EstimatedUsage = true

	saved, err := sessions.Save(t.Context(), created)
	require.NoError(t, err)
	require.True(t, saved.EstimatedUsage)

	saved.EstimatedUsage = false
	updated, err := sessions.Save(t.Context(), saved)
	require.NoError(t, err)
	require.False(t, updated.EstimatedUsage)

	refetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.False(t, refetched.EstimatedUsage)
}

func TestFork(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	q := db.New(conn)
	sessions := NewService(q, conn)
	messages := message.NewService(q)

	// Create original session
	original, err := sessions.Create(t.Context(), "Original Session")
	require.NoError(t, err)

	// Add some messages to the original session
	msg1, err := messages.Create(t.Context(), original.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "Hello"},
		},
	})
	require.NoError(t, err)

	msg2, err := messages.Create(t.Context(), original.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "Hi there!"},
		},
	})
	require.NoError(t, err)

	// Update the original session with usage stats
	original.PromptTokens = 100
	original.CompletionTokens = 50
	original.Cost = 0.05
	original, err = sessions.Save(t.Context(), original)
	require.NoError(t, err)

	// Fork the session
	forked, err := sessions.Fork(t.Context(), original.ID)
	require.NoError(t, err)

	// Verify the forked session has a new ID
	require.NotEqual(t, original.ID, forked.ID)

	// Verify the title has " (fork)" appended
	require.Equal(t, "Original Session (fork)", forked.Title)

	// Verify the usage stats were copied
	require.Equal(t, original.PromptTokens, forked.PromptTokens)
	require.Equal(t, original.CompletionTokens, forked.CompletionTokens)
	require.Equal(t, original.Cost, forked.Cost)

	// Verify messages were copied
	forkedMsgs, err := messages.List(t.Context(), forked.ID)
	require.NoError(t, err)
	require.Len(t, forkedMsgs, 2)

	// Verify the message content matches
	require.Equal(t, msg1.Parts, forkedMsgs[0].Parts)
	require.Equal(t, msg2.Parts, forkedMsgs[1].Parts)

	// Verify original session still has its messages
	originalMsgs, err := messages.List(t.Context(), original.ID)
	require.NoError(t, err)
	require.Len(t, originalMsgs, 2)

	// Verify forked session is independent - add a message to fork
	_, err = messages.Create(t.Context(), forked.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "Fork message"},
		},
	})
	require.NoError(t, err)

	// Verify original didn't change
	originalMsgs, err = messages.List(t.Context(), original.ID)
	require.NoError(t, err)
	require.Len(t, originalMsgs, 2)

	// Verify fork has 3 messages
	forkedMsgs, err = messages.List(t.Context(), forked.ID)
	require.NoError(t, err)
	require.Len(t, forkedMsgs, 3)
}

func TestForkNonExistentSession(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	sessions := NewService(db.New(conn), conn)

	// Try to fork a non-existent session
	_, err = sessions.Fork(t.Context(), "non-existent-id")
	require.Error(t, err)
}
