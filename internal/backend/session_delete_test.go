package backend

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

type blockingDeleteSessionService struct {
	session.Service
	started chan struct{}
	release chan struct{}
}

func (s *blockingDeleteSessionService) Delete(ctx context.Context, id string) error {
	close(s.started)
	select {
	case <-s.release:
	case <-ctx.Done():
		return context.Cause(ctx)
	}
	return s.Service.Delete(ctx, id)
}

type failingDeleteSessionService struct {
	session.Service
	err error
}

func (s failingDeleteSessionService) Delete(context.Context, string) error {
	return s.err
}

func TestDeleteSessionBlocksMessageWritesUntilDeletionCompletes(t *testing.T) {
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
	sess, err := sessions.Create(t.Context(), "delete barrier")
	require.NoError(t, err)
	msg, err := messages.Create(t.Context(), sess.ID, message.CreateMessageParams{Role: message.Assistant})
	require.NoError(t, err)
	msg.AppendContent("pending")
	require.NoError(t, messages.Update(t.Context(), msg))

	blockingSessions := &blockingDeleteSessionService{
		Service: sessions,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	backend, _ := newTestBackend(t)
	workspace := &Workspace{
		App: &app.App{
			Sessions: blockingSessions,
			Messages: messages,
		},
		ID:      "workspace",
		Path:    t.TempDir(),
		clients: make(map[string]*clientState),
	}
	backend.workspaces.Set(workspace.ID, workspace)

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- backend.DeleteSession(t.Context(), workspace.ID, sess.ID)
	}()
	select {
	case <-blockingSessions.started:
	case <-time.After(time.Second):
		t.Fatal("session deletion did not start")
	}

	msg.AppendContent("late update")
	require.Error(t, messages.Update(t.Context(), msg))
	_, err = messages.Create(t.Context(), sess.ID, message.CreateMessageParams{Role: message.Assistant})
	require.Error(t, err)

	close(blockingSessions.release)
	require.NoError(t, <-deleteDone)
	require.NoError(t, messages.FlushAll(t.Context()))
	_, err = sessions.Get(t.Context(), sess.ID)
	require.Error(t, err)
	_, err = messages.Get(t.Context(), msg.ID)
	require.Error(t, err)
}

func TestDeleteSessionFailureReleasesMessageBarrier(t *testing.T) {
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
	sess, err := sessions.Create(t.Context(), "delete rollback")
	require.NoError(t, err)
	msg, err := messages.Create(t.Context(), sess.ID, message.CreateMessageParams{Role: message.Assistant})
	require.NoError(t, err)

	deleteErr := errors.New("delete failed")
	backend, _ := newTestBackend(t)
	workspace := &Workspace{
		App: &app.App{
			Sessions: failingDeleteSessionService{Service: sessions, err: deleteErr},
			Messages: messages,
		},
		ID:      "workspace",
		Path:    t.TempDir(),
		clients: make(map[string]*clientState),
	}
	backend.workspaces.Set(workspace.ID, workspace)

	require.ErrorIs(t, backend.DeleteSession(t.Context(), workspace.ID, sess.ID), deleteErr)
	msg.AppendContent("allowed after rollback")
	require.NoError(t, messages.Update(t.Context(), msg))
	_, err = messages.Create(t.Context(), sess.ID, message.CreateMessageParams{Role: message.Assistant})
	require.NoError(t, err)
}
