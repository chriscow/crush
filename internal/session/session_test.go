package session

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func newTestServices(t *testing.T) (Service, message.Service, *db.Queries) {
	t.Helper()

	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	queries := db.New(conn)
	return NewService(queries, conn), message.NewService(queries, message.WithDebounce(0)), queries
}

func TestEstimatedUsageStateSurvivesFetchModifySave(t *testing.T) {
	sessions, _, _ := newTestServices(t)

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

func TestSessionChannelPersists(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := NewService(db.New(conn), conn)

	created, err := sessions.Create(t.Context(), "channel")
	require.NoError(t, err)
	updated, err := sessions.SetChannel(t.Context(), created.ID, "signal")
	require.NoError(t, err)
	require.Equal(t, "signal", updated.Channel)

	fetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, "signal", fetched.Channel)
}

func TestEstimatedUsageStateCanBeClearedByExplicitSave(t *testing.T) {
	sessions, _, _ := newTestServices(t)

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

func TestAddUsageAccumulatesConcurrentSummarizerTotals(t *testing.T) {
	sessions, _, _ := newTestServices(t)
	created, err := sessions.Create(t.Context(), "usage")
	require.NoError(t, err)

	const additions = 12
	var wg sync.WaitGroup
	errs := make(chan error, additions)
	for range additions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- sessions.AddUsage(t.Context(), created.ID, 3, 2, 0.25)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	updated, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, int64(additions*3), updated.PromptTokens)
	require.Equal(t, int64(additions*2), updated.CompletionTokens)
	require.Equal(t, float64(additions)*0.25, updated.Cost)
}

func TestAddUsageRejectsInvalidOrMissingSession(t *testing.T) {
	sessions, _, _ := newTestServices(t)
	created, err := sessions.Create(t.Context(), "usage")
	require.NoError(t, err)

	for _, cost := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		require.Error(t, sessions.AddUsage(t.Context(), created.ID, 1, 1, cost))
	}
	require.Error(t, sessions.AddUsage(t.Context(), created.ID, -1, 1, 0))
	require.Error(t, sessions.AddUsage(t.Context(), "missing", 1, 1, 1))

	unchanged, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Zero(t, unchanged.PromptTokens)
	require.Zero(t, unchanged.CompletionTokens)
	require.Zero(t, unchanged.Cost)
}

type failingDeleteLifecycle struct {
	err error
}

func (f failingDeleteLifecycle) PrepareSessionDelete(context.Context, *db.Queries, string) error {
	return f.err
}

func TestDeleteRollsBackWhenProjectionInvalidationFails(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	queries := db.New(conn)
	sessions := NewService(queries, conn, WithDeleteLifecycle(failingDeleteLifecycle{err: errors.New("invalidation failed")}))
	messages := message.NewService(queries, message.WithDebounce(0))
	created, err := sessions.Create(t.Context(), "delete rollback")
	require.NoError(t, err)
	createdMessage, err := messages.Create(t.Context(), created.ID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "retained"}},
	})
	require.NoError(t, err)

	err = sessions.Delete(t.Context(), created.ID)
	require.ErrorContains(t, err, "invalidation failed")
	_, err = sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	persistedMessage, err := messages.Get(t.Context(), createdMessage.ID)
	require.NoError(t, err)
	require.Equal(t, "retained", persistedMessage.Content().Text)
}

func TestForkCopiesConversationState(t *testing.T) {
	sessions, messages, queries := newTestServices(t)

	original, err := sessions.Create(t.Context(), "Original Session")
	require.NoError(t, err)

	userMessage, err := messages.Create(t.Context(), original.ID, message.CreateMessageParams{
		Role:     message.User,
		Parts:    []message.ContentPart{message.TextContent{Text: "Hello"}},
		Model:    "large-model",
		Provider: "provider-a",
	})
	require.NoError(t, err)

	summaryMessage, err := messages.Create(t.Context(), original.ID, message.CreateMessageParams{
		Role:             message.Assistant,
		Parts:            []message.ContentPart{message.TextContent{Text: "Summary"}},
		Model:            "small-model",
		Provider:         "provider-b",
		IsSummaryMessage: true,
	})
	require.NoError(t, err)
	summaryMessage.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, messages.Update(t.Context(), summaryMessage))

	original.PromptTokens = 100
	original.CompletionTokens = 50
	original.Cost = 0.05
	original.EstimatedUsage = true
	original.SummaryMessageID = summaryMessage.ID
	original.Todos = []Todo{{
		Content:    "Verify the fork",
		Status:     TodoStatusInProgress,
		ActiveForm: "Verifying the fork",
	}}
	original, err = sessions.Save(t.Context(), original)
	require.NoError(t, err)

	eventsCtx, cancelEvents := context.WithCancel(t.Context())
	t.Cleanup(cancelEvents)
	events := sessions.Subscribe(eventsCtx)

	forked, err := sessions.Fork(t.Context(), original.ID)
	require.NoError(t, err)
	require.NotEqual(t, original.ID, forked.ID)
	require.Empty(t, forked.ParentSessionID)
	require.Equal(t, "Original Session (fork)", forked.Title)
	require.Equal(t, int64(2), forked.MessageCount)
	require.Equal(t, original.PromptTokens, forked.PromptTokens)
	require.Equal(t, original.CompletionTokens, forked.CompletionTokens)
	require.Equal(t, original.Cost, forked.Cost)
	require.True(t, forked.EstimatedUsage)
	require.Equal(t, original.Todos, forked.Todos)
	require.NotEmpty(t, forked.SummaryMessageID)
	require.NotEqual(t, original.SummaryMessageID, forked.SummaryMessageID)

	select {
	case event := <-events:
		require.Equal(t, pubsub.CreatedEvent, event.Type)
		require.Equal(t, forked.ID, event.Payload.ID)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for fork creation event")
	}

	persistedFork, err := sessions.Get(t.Context(), forked.ID)
	require.NoError(t, err)
	require.Equal(t, forked, persistedFork)

	forkedMessages, err := messages.List(t.Context(), forked.ID)
	require.NoError(t, err)
	require.Len(t, forkedMessages, 2)
	require.NotEqual(t, userMessage.ID, forkedMessages[0].ID)
	require.Equal(t, userMessage.Role, forkedMessages[0].Role)
	require.Equal(t, userMessage.Parts, forkedMessages[0].Parts)
	require.Equal(t, userMessage.Model, forkedMessages[0].Model)
	require.Equal(t, userMessage.Provider, forkedMessages[0].Provider)
	require.Equal(t, summaryMessage.Role, forkedMessages[1].Role)
	require.Equal(t, summaryMessage.Parts, forkedMessages[1].Parts)
	require.Equal(t, summaryMessage.Model, forkedMessages[1].Model)
	require.Equal(t, summaryMessage.Provider, forkedMessages[1].Provider)
	require.Equal(t, summaryMessage.IsSummaryMessage, forkedMessages[1].IsSummaryMessage)
	require.Equal(t, forked.SummaryMessageID, forkedMessages[1].ID)

	dbOriginalMessages, err := queries.ListMessagesBySession(t.Context(), original.ID)
	require.NoError(t, err)
	dbForkedMessages, err := queries.ListMessagesBySession(t.Context(), forked.ID)
	require.NoError(t, err)
	for index := range dbOriginalMessages {
		require.Equal(t, dbOriginalMessages[index].CreatedAt, dbForkedMessages[index].CreatedAt)
		require.Equal(t, dbOriginalMessages[index].UpdatedAt, dbForkedMessages[index].UpdatedAt)
		require.Equal(t, dbOriginalMessages[index].FinishedAt, dbForkedMessages[index].FinishedAt)
	}
	require.True(t, dbOriginalMessages[1].FinishedAt.Valid)
}

func TestForkCreatesIndependentConversation(t *testing.T) {
	sessions, messages, _ := newTestServices(t)

	original, err := sessions.Create(t.Context(), "Original Session")
	require.NoError(t, err)
	_, err = messages.Create(t.Context(), original.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "Original message"}},
	})
	require.NoError(t, err)

	forked, err := sessions.Fork(t.Context(), original.ID)
	require.NoError(t, err)
	_, err = messages.Create(t.Context(), forked.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "Fork-only message"}},
	})
	require.NoError(t, err)

	originalMessages, err := messages.List(t.Context(), original.ID)
	require.NoError(t, err)
	require.Len(t, originalMessages, 1)
	require.Equal(t, "Original message", originalMessages[0].Parts[0].(message.TextContent).Text)

	forkedMessages, err := messages.List(t.Context(), forked.ID)
	require.NoError(t, err)
	require.Len(t, forkedMessages, 2)
}

func TestForkEmptySession(t *testing.T) {
	sessions, messages, _ := newTestServices(t)

	original, err := sessions.Create(t.Context(), "Empty Session")
	require.NoError(t, err)
	forked, err := sessions.Fork(t.Context(), original.ID)
	require.NoError(t, err)
	require.Equal(t, "Empty Session (fork)", forked.Title)
	require.Zero(t, forked.MessageCount)
	require.Empty(t, forked.SummaryMessageID)

	forkedMessages, err := messages.List(t.Context(), forked.ID)
	require.NoError(t, err)
	require.Empty(t, forkedMessages)
}

func TestForkClearsDanglingSummaryMessageID(t *testing.T) {
	sessions, _, _ := newTestServices(t)

	original, err := sessions.Create(t.Context(), "Dangling Summary")
	require.NoError(t, err)
	original.SummaryMessageID = "missing-message"
	_, err = sessions.Save(t.Context(), original)
	require.NoError(t, err)

	forked, err := sessions.Fork(t.Context(), original.ID)
	require.NoError(t, err)
	require.Empty(t, forked.SummaryMessageID)

	persisted, err := sessions.Get(t.Context(), forked.ID)
	require.NoError(t, err)
	require.Empty(t, persisted.SummaryMessageID)
}

func TestForkNonExistentSessionDoesNotCreateSession(t *testing.T) {
	sessions, _, _ := newTestServices(t)

	_, err := sessions.Fork(t.Context(), "non-existent-id")
	require.ErrorIs(t, err, sql.ErrNoRows)

	allSessions, err := sessions.List(t.Context())
	require.NoError(t, err)
	require.Empty(t, allSessions)
}
