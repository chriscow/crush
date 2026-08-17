package loop

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestScheduler_Schedule(t *testing.T) {
	var runCount int64
	scheduler := NewScheduler(
		WithTaskRunner(func(ctx context.Context, sessionID, prompt string) error {
			atomic.AddInt64(&runCount, 1)
			return nil
		}),
		WithTaskTimeout(100*time.Millisecond),
	)
	defer scheduler.Stop()

	task, err := scheduler.Schedule(context.Background(), "session-1", "test prompt", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, task)
	require.NotEmpty(t, task.ID)
	require.Equal(t, "session-1", task.SessionID)
	require.Equal(t, "test prompt", task.Prompt)
}

func TestScheduler_MaxTasks(t *testing.T) {
	scheduler := NewScheduler(
		WithMaxTasks(2),
		WithTaskRunner(func(ctx context.Context, sessionID, prompt string) error { return nil }),
	)
	defer scheduler.Stop()

	// Should succeed up to max
	_, err := scheduler.Schedule(context.Background(), "session-1", "task 1", time.Minute)
	require.NoError(t, err)

	_, err = scheduler.Schedule(context.Background(), "session-1", "task 2", time.Minute)
	require.NoError(t, err)

	// Should fail when exceeding max
	_, err = scheduler.Schedule(context.Background(), "session-1", "task 3", time.Minute)
	require.Error(t, err)
	require.Contains(t, err.Error(), "maximum")
}

func TestScheduler_MinimumInterval(t *testing.T) {
	scheduler := NewScheduler(
		WithTaskRunner(func(ctx context.Context, sessionID, prompt string) error { return nil }),
	)
	defer scheduler.Stop()

	// Should fail with interval less than 1 minute
	_, err := scheduler.Schedule(context.Background(), "session-1", "task 1", 30*time.Second)
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be at least")

	// Should succeed with 1 minute
	_, err = scheduler.Schedule(context.Background(), "session-1", "task 2", time.Minute)
	require.NoError(t, err)
}

func TestScheduler_Cancel(t *testing.T) {
	scheduler := NewScheduler(
		WithTaskRunner(func(ctx context.Context, sessionID, prompt string) error { return nil }),
	)
	defer scheduler.Stop()

	task, err := scheduler.Schedule(context.Background(), "session-1", "test prompt", time.Minute)
	require.NoError(t, err)

	// Cancel the task
	err = scheduler.Cancel(task.ID)
	require.NoError(t, err)

	// Verify task is removed
	_, err = scheduler.Get(task.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestScheduler_CancelSession(t *testing.T) {
	scheduler := NewScheduler(
		WithTaskRunner(func(ctx context.Context, sessionID, prompt string) error { return nil }),
	)
	defer scheduler.Stop()

	// Create multiple tasks for the same session
	_, err := scheduler.Schedule(context.Background(), "session-1", "task 1", time.Minute)
	require.NoError(t, err)

	_, err = scheduler.Schedule(context.Background(), "session-1", "task 2", time.Minute)
	require.NoError(t, err)

	// Create a task for a different session
	_, err = scheduler.Schedule(context.Background(), "session-2", "task 3", time.Minute)
	require.NoError(t, err)

	// Cancel all tasks for session-1
	count := scheduler.CancelSession("session-1")
	require.Equal(t, 2, count)

	// Verify only session-2 tasks remain
	tasks := scheduler.List("session-2")
	require.Len(t, tasks, 1)

	tasks = scheduler.List("session-1")
	require.Len(t, tasks, 0)
}

func TestScheduler_List(t *testing.T) {
	scheduler := NewScheduler(
		WithTaskRunner(func(ctx context.Context, sessionID, prompt string) error { return nil }),
	)
	defer scheduler.Stop()

	// Create multiple tasks
	_, err := scheduler.Schedule(context.Background(), "session-1", "task 1", time.Minute)
	require.NoError(t, err)

	_, err = scheduler.Schedule(context.Background(), "session-1", "task 2", time.Minute)
	require.NoError(t, err)

	_, err = scheduler.Schedule(context.Background(), "session-2", "task 3", time.Minute)
	require.NoError(t, err)

	// List tasks for session-1
	tasks := scheduler.List("session-1")
	require.Len(t, tasks, 2)

	// List tasks for session-2
	tasks = scheduler.List("session-2")
	require.Len(t, tasks, 1)
}

func TestScheduler_Get(t *testing.T) {
	scheduler := NewScheduler(
		WithTaskRunner(func(ctx context.Context, sessionID, prompt string) error { return nil }),
	)
	defer scheduler.Stop()

	task, err := scheduler.Schedule(context.Background(), "session-1", "test prompt", time.Minute)
	require.NoError(t, err)

	// Get the task
	fetched, err := scheduler.Get(task.ID)
	require.NoError(t, err)
	require.Equal(t, task.ID, fetched.ID)
	require.Equal(t, task.SessionID, fetched.SessionID)
	require.Equal(t, task.Prompt, fetched.Prompt)
}

func TestParseInterval(t *testing.T) {
	tests := []struct {
		input    string
		expected time.Duration
		hasError bool
	}{
		{"30s", 30 * time.Second, false},
		{"5m", 5 * time.Minute, false},
		{"2h", 2 * time.Hour, false},
		{"1d", 24 * time.Hour, false},
		{"1.5h", 90 * time.Minute, false}, // Decimal support
		{"0.5m", 30 * time.Second, false}, // Decimal support
		{"invalid", 0, true},
		{"", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := ParseInterval(tt.input)
			if tt.hasError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.expected, result)
			}
		})
	}
}

func TestGenerateTaskID(t *testing.T) {
	// Generate multiple IDs and verify they're unique
	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := generateTaskID()
		require.Len(t, id, 8) // Should be 8 hex characters
		require.NotContains(t, ids, id, "Task ID should be unique")
		ids[id] = true
	}
}
