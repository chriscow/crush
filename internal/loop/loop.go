// Package loop provides session-scoped scheduled task execution for Crush.
package loop

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Task represents a scheduled loop task.
type Task struct {
	ID        string
	SessionID string
	Prompt    string
	Interval  time.Duration
	NextRun   time.Time
	CreatedAt time.Time

	cancel context.CancelFunc
}

// Scheduler manages scheduled loop tasks.
type Scheduler struct {
	mu    sync.RWMutex
	tasks map[string]*Task

	maxTasks    int
	maxDuration time.Duration
	taskTimeout time.Duration
	onTaskRun   func(ctx context.Context, sessionID, prompt string) error

	wg sync.WaitGroup // Track running goroutines for graceful shutdown
}

// SchedulerOption configures a Scheduler.
type SchedulerOption func(*Scheduler)

// WithMaxTasks sets the maximum number of concurrent tasks per session (default 50).
func WithMaxTasks(n int) SchedulerOption {
	return func(s *Scheduler) {
		s.maxTasks = n
	}
}

// WithMaxDuration sets the maximum duration a task can run (default 7 days).
func WithMaxDuration(d time.Duration) SchedulerOption {
	return func(s *Scheduler) {
		s.maxDuration = d
	}
}

// WithTaskTimeout sets the timeout for individual task executions.
func WithTaskTimeout(d time.Duration) SchedulerOption {
	return func(s *Scheduler) {
		s.taskTimeout = d
	}
}

// WithTaskRunner sets the callback that runs when a task fires.
func WithTaskRunner(fn func(ctx context.Context, sessionID, prompt string) error) SchedulerOption {
	return func(s *Scheduler) {
		s.onTaskRun = fn
	}
}

// NewScheduler creates a new loop scheduler.
func NewScheduler(opts ...SchedulerOption) *Scheduler {
	s := &Scheduler{
		tasks:       make(map[string]*Task),
		maxTasks:    50,
		maxDuration: 7 * 24 * time.Hour,
		taskTimeout: 5 * time.Minute,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Schedule creates and starts a new scheduled task.
// Returns the task ID or an error if the session has reached max tasks.
func (s *Scheduler) Schedule(ctx context.Context, sessionID, prompt string, interval time.Duration) (*Task, error) {
	// Validate interval
	minInterval := time.Minute
	if interval < minInterval {
		return nil, fmt.Errorf("interval must be at least %v", minInterval)
	}
	if interval > s.maxDuration {
		interval = s.maxDuration
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Check task limit
	sessionTasks := 0
	for _, t := range s.tasks {
		if t.SessionID == sessionID {
			sessionTasks++
		}
	}
	if sessionTasks >= s.maxTasks {
		return nil, fmt.Errorf("session has reached maximum of %d scheduled tasks", s.maxTasks)
	}

	task := &Task{
		ID:        generateTaskID(),
		SessionID: sessionID,
		Prompt:    prompt,
		Interval:  interval,
		NextRun:   time.Now().Add(interval),
		CreatedAt: time.Now(),
	}

	// Create a cancellable context for this task
	taskCtx, cancel := context.WithCancel(context.Background())
	task.cancel = cancel

	s.tasks[task.ID] = task

	// Track the goroutine
	s.wg.Add(1)

	// Start the task goroutine
	go s.runTask(taskCtx, task)

	slog.Info("Scheduled loop task",
		"task_id", task.ID,
		"session_id", sessionID,
		"interval", interval,
		"prompt", prompt,
	)

	return task, nil
}

// runTask executes the task on its schedule until cancelled or expired.
func (s *Scheduler) runTask(ctx context.Context, task *Task) {
	defer s.wg.Done()

	ticker := time.NewTicker(task.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Debug("Loop task cancelled", "task_id", task.ID)
			return

		case <-ticker.C:
			// Check if task has expired (read with lock to avoid race)
			s.mu.RLock()
			createdAt := task.CreatedAt
			maxDur := s.maxDuration
			s.mu.RUnlock()

			if time.Since(createdAt) > maxDur {
				// Cancel from separate goroutine to avoid deadlock
				go s.Cancel(task.ID)
				slog.Info("Loop task expired after max duration", "task_id", task.ID)
				return
			}

			// Execute the task with timeout
			slog.Debug("Loop task firing", "task_id", task.ID)
			if s.onTaskRun != nil {
				// Apply task timeout
				taskCtx, cancel := context.WithTimeout(context.Background(), s.taskTimeout)
				if err := s.onTaskRun(taskCtx, task.SessionID, task.Prompt); err != nil {
					slog.Error("Loop task execution failed",
						"task_id", task.ID,
						"error", err,
					)
				}
				cancel()
			}
		}
	}
}

// Cancel removes and stops a scheduled task.
func (s *Scheduler) Cancel(taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return fmt.Errorf("task %s not found", taskID)
	}

	if task.cancel != nil {
		task.cancel()
	}

	delete(s.tasks, taskID)
	slog.Info("Cancelled loop task", "task_id", taskID)
	return nil
}

// CancelSession cancels all tasks for a session.
func (s *Scheduler) CancelSession(sessionID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	for id, task := range s.tasks {
		if task.SessionID == sessionID {
			if task.cancel != nil {
				task.cancel()
			}
			delete(s.tasks, id)
			count++
		}
	}

	if count > 0 {
		slog.Info("Cancelled session loop tasks", "session_id", sessionID, "count", count)
	}
	return count
}

// List returns all tasks for a session.
func (s *Scheduler) List(sessionID string) []*Task {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var tasks []*Task
	for _, task := range s.tasks {
		if task.SessionID == sessionID {
			tasks = append(tasks, task)
		}
	}
	return tasks
}

// Get returns a specific task by ID.
func (s *Scheduler) Get(taskID string) (*Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return nil, fmt.Errorf("task %s not found", taskID)
	}
	return task, nil
}

// generateTaskID creates a unique 8-character task ID using crypto/rand.
func generateTaskID() string {
	b := make([]byte, 4) // 4 bytes = 8 hex characters
	_, _ = cryptorand.Read(b)
	return hex.EncodeToString(b)
}

// ParseInterval parses an interval string like "5m", "30s", "2h" into a duration.
// Supports s, m, h, d suffixes and decimal values like "1.5h".
func ParseInterval(s string) (time.Duration, error) {
	// Extract numeric part (support decimals)
	var value float64
	n, err := fmt.Sscanf(s, "%f", &value)
	if err != nil || n != 1 {
		return 0, fmt.Errorf("invalid interval format: %s", s)
	}

	// Extract suffix
	suffix := ""
	for i, c := range s {
		if (c < '0' || c > '9') && c != '.' {
			suffix = s[i:]
			break
		}
	}

	switch suffix {
	case "s":
		return time.Duration(value * float64(time.Second)), nil
	case "m":
		return time.Duration(value * float64(time.Minute)), nil
	case "h":
		return time.Duration(value * float64(time.Hour)), nil
	case "d":
		return time.Duration(value * float64(24*time.Hour)), nil
	default:
		return 0, fmt.Errorf("unknown interval suffix: %s", suffix)
	}
}

// Stop cancels all tasks and waits for goroutines to finish.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	for id, task := range s.tasks {
		if task.cancel != nil {
			task.cancel()
		}
		delete(s.tasks, id)
	}
	s.mu.Unlock()

	// Wait for all goroutines to finish with timeout
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		slog.Debug("All loop tasks stopped gracefully")
	case <-time.After(5 * time.Second):
		slog.Warn("Timeout waiting for loop tasks to stop")
	}
}
