package app

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/condense"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// TestNewWiresContextProjectionDeletionLifecycle verifies that projection rows
// are invalidated through the application's message and session lifecycles.
func TestNewWiresContextProjectionDeletionLifecycle(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	cfg := &config.Config{
		Options:   &config.Options{DataDirectory: dataDir},
		Providers: csync.NewMap[string, config.ProviderConfig](),
	}
	store := config.NewTestStore(cfg)
	app, err := New(t.Context(), conn, store, nil)
	require.NoError(t, err)

	sess, err := app.Sessions.Create(t.Context(), "projection lifecycle")
	require.NoError(t, err)
	assistant, err := app.Messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{message.ToolCall{
			ID: "call-1", Name: "view", Input: `{}`, Finished: true,
		}},
	})
	require.NoError(t, err)
	assistant.AddFinish(message.FinishReasonToolUse, "", "")
	require.NoError(t, app.Messages.Update(t.Context(), assistant))
	toolMessage, err := app.Messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{message.ToolResult{
			ToolCallID: "call-1", Name: "view", Content: strings.Repeat("canonical", 100),
		}},
	})
	require.NoError(t, err)

	require.NotNil(t, app.projectionStore)
	projection := condense.New(app.projectionStore, appTestSummarizer{}, condense.Options{
		Enabled: true, SummarizerTimeout: time.Second,
	})
	_, err = projection.Project(t.Context(), sess.ID, []message.Message{assistant, toolMessage})
	require.NoError(t, err)

	queries := db.New(conn)
	require.NoError(t, app.Messages.Delete(t.Context(), toolMessage.ID))
	_, err = app.Messages.Get(t.Context(), toolMessage.ID)
	require.ErrorIs(t, err, sql.ErrNoRows)
	items, err := queries.CountContextProjectionItemsBySession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Zero(t, items)

	secondAssistant, err := app.Messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{message.ToolCall{
			ID: "call-2", Name: "view", Input: `{}`, Finished: true,
		}},
	})
	require.NoError(t, err)
	secondAssistant.AddFinish(message.FinishReasonToolUse, "", "")
	require.NoError(t, app.Messages.Update(t.Context(), secondAssistant))
	_, err = app.Messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{message.ToolResult{
			ToolCallID: "call-2", Name: "view", Content: strings.Repeat("second", 150),
		}},
	})
	require.NoError(t, err)
	currentMessages, err := app.Messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	_, err = projection.Project(t.Context(), sess.ID, currentMessages)
	require.NoError(t, err)

	require.NoError(t, app.Messages.PrepareSessionDelete(t.Context(), sess.ID))
	require.NoError(t, app.Sessions.Delete(t.Context(), sess.ID))
	app.Messages.FinishSessionDelete(sess.ID, true)
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

type appTestSummarizer struct{}

func (appTestSummarizer) Summarize(_ context.Context, candidate condense.Candidate) (condense.Summary, error) {
	items := make([]condense.SummaryItem, len(candidate.Items))
	for i := range items {
		items[i] = condense.SummaryItem{Ordinal: i, Description: "description"}
	}
	return condense.Summary{Text: "summary", Items: items}, nil
}

// TestSetupSubscriber_NormalFlow verifies that events published to the source
// broker are forwarded to the output broker.
func TestSetupSubscriber_NormalFlow(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	src := pubsub.NewBroker[string]()
	defer src.Shutdown()
	out := pubsub.NewBroker[tea.Msg]()
	defer out.Shutdown()

	ch := out.Subscribe(ctx)

	var wg sync.WaitGroup
	app := &App{serviceEventsWG: &wg, events: out}
	app.subscribe(ctx, "test", src.Subscribe)

	// Yield so the subscriber goroutine can call src.Subscribe before we publish.
	time.Sleep(10 * time.Millisecond)

	src.Publish(pubsub.CreatedEvent, "hello")
	src.Publish(pubsub.CreatedEvent, "world")

	for range 2 {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for forwarded event")
		}
	}

	cancel()
	wg.Wait()
}

// TestSetupSubscriber_ContextCancellation verifies the goroutine exits cleanly
// when the context is cancelled.
func TestSetupSubscriber_ContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	src := pubsub.NewBroker[string]()
	defer src.Shutdown()
	out := pubsub.NewBroker[tea.Msg]()
	defer out.Shutdown()

	var wg sync.WaitGroup
	app := &App{serviceEventsWG: &wg, events: out}
	app.subscribe(ctx, "test", src.Subscribe)

	src.Publish(pubsub.CreatedEvent, "event")
	cancel()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("subscribe goroutine did not exit after context cancellation")
	}
}

// TestEvents_ZeroConsumers verifies that publishing with no subscribers does
// not block or panic.
func TestEvents_ZeroConsumers(t *testing.T) {
	t.Parallel()

	broker := pubsub.NewBroker[tea.Msg]()
	defer broker.Shutdown()

	require.Equal(t, 0, broker.GetSubscriberCount())

	// Must not block.
	done := make(chan struct{})
	go func() {
		broker.Publish(pubsub.UpdatedEvent, tea.Msg("msg1"))
		broker.Publish(pubsub.UpdatedEvent, tea.Msg("msg2"))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish with zero consumers blocked")
	}
}

// TestEvents_OneConsumer verifies that a single subscriber receives every event
// exactly once.
func TestEvents_OneConsumer(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	broker := pubsub.NewBroker[tea.Msg]()
	defer broker.Shutdown()

	ch := broker.Subscribe(ctx)

	const n = 10
	for i := range n {
		broker.Publish(pubsub.UpdatedEvent, tea.Msg(i))
	}

	for i := range n {
		select {
		case ev := <-ch:
			require.Equal(t, tea.Msg(i), ev.Payload)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for event %d", i)
		}
	}
}

// TestEvents_NConsumers verifies that every subscriber receives every event
// exactly once, regardless of how many concurrent consumers are attached.
func TestEvents_NConsumers(t *testing.T) {
	t.Parallel()

	for _, n := range []int{2, 5, 10} {
		t.Run(fmt.Sprintf("consumers=%d", n), func(t *testing.T) {
			t.Parallel()
			testNConsumers(t, n)
		})
	}
}

func testNConsumers(t *testing.T, n int) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	broker := pubsub.NewBroker[tea.Msg]()
	defer broker.Shutdown()

	// Subscribe all N consumers before publishing.
	channels := make([]<-chan pubsub.Event[tea.Msg], n)
	for i := range n {
		channels[i] = broker.Subscribe(ctx)
	}
	require.Equal(t, n, broker.GetSubscriberCount())

	const numEvents = 20
	for i := range numEvents {
		broker.Publish(pubsub.UpdatedEvent, tea.Msg(i))
	}

	// Each consumer must receive all numEvents messages.
	var wg sync.WaitGroup
	for i, ch := range channels {
		wg.Go(func() {
			for j := range numEvents {
				select {
				case ev := <-ch:
					require.Equal(t, tea.Msg(j), ev.Payload,
						"consumer %d: wrong payload for event %d", i, j)
				case <-time.After(5 * time.Second):
					t.Errorf("consumer %d: timed out waiting for event %d", i, j)
					return
				}
			}
		})
	}
	wg.Wait()
}
