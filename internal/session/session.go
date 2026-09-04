package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/event"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/google/uuid"
	"github.com/zeebo/xxh3"
)

type TodoStatus string

const (
	TodoStatusPending    TodoStatus = "pending"
	TodoStatusInProgress TodoStatus = "in_progress"
	TodoStatusCompleted  TodoStatus = "completed"
)

// HashID returns the XXH3 hash of a session ID (UUID) as a hex string.
func HashID(id string) string {
	h := xxh3.New()
	h.WriteString(id)
	return fmt.Sprintf("%x", h.Sum(nil))
}

type Todo struct {
	Content    string     `json:"content"`
	Status     TodoStatus `json:"status"`
	ActiveForm string     `json:"active_form"`
}

// HasIncompleteTodos returns true if there are any non-completed todos.
func HasIncompleteTodos(todos []Todo) bool {
	for _, todo := range todos {
		if todo.Status != TodoStatusCompleted {
			return true
		}
	}
	return false
}

type Session struct {
	ID               string
	ParentSessionID  string
	Title            string
	MessageCount     int64
	PromptTokens     int64
	CompletionTokens int64
	EstimatedUsage   bool
	SummaryMessageID string
	Cost             float64
	Todos            []Todo
	Channel          string
	CreatedAt        int64
	UpdatedAt        int64
}

type Service interface {
	pubsub.Subscriber[Session]
	Create(ctx context.Context, title string) (Session, error)
	CreateTitleSession(ctx context.Context, parentSessionID string) (Session, error)
	CreateTaskSession(ctx context.Context, toolCallID, parentSessionID, title string) (Session, error)
	Get(ctx context.Context, id string) (Session, error)
	GetLast(ctx context.Context) (Session, error)
	List(ctx context.Context) ([]Session, error)
	Save(ctx context.Context, session Session) (Session, error)
	SetChannel(ctx context.Context, sessionID, channel string) (Session, error)
	UpdateTitleAndUsage(ctx context.Context, sessionID, title string, promptTokens, completionTokens int64, cost float64) error
	AddUsage(ctx context.Context, sessionID string, promptTokens, completionTokens int64, cost float64) error
	Rename(ctx context.Context, id string, title string) error
	Delete(ctx context.Context, id string) error

	// Agent tool session management
	CreateAgentToolSessionID(messageID, toolCallID string) string
	ParseAgentToolSessionID(sessionID string) (messageID string, toolCallID string, ok bool)
	IsAgentToolSession(sessionID string) bool

	// Fork creates an independent copy of a session with all its messages.
	// The new session has its own ID but contains the full conversation history.
	Fork(ctx context.Context, sessionID string) (Session, error)
}

// Fork creates an independent copy of a session with all its messages.
// The new session gets a new UUID but contains all messages from the original.
func (s *service) Fork(ctx context.Context, sessionID string) (Session, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	qtx := s.q.WithTx(tx)
	dbOriginal, err := qtx.GetSessionByID(ctx, sessionID)
	if err != nil {
		return Session{}, fmt.Errorf("failed to get session: %w", err)
	}
	original := s.fromDBItem(dbOriginal)
	s.applyEstimatedUsageState(&original)

	forkTitle := original.Title + " (fork)"
	newID := uuid.New().String()
	if _, err := qtx.CreateSession(ctx, db.CreateSessionParams{
		ID:               newID,
		Title:            forkTitle,
		PromptTokens:     original.PromptTokens,
		CompletionTokens: original.CompletionTokens,
		Cost:             original.Cost,
	}); err != nil {
		return Session{}, fmt.Errorf("failed to create forked session: %w", err)
	}

	messages, err := qtx.ListMessagesBySession(ctx, sessionID)
	if err != nil {
		return Session{}, fmt.Errorf("failed to list messages: %w", err)
	}

	messageIDMap := make(map[string]string, len(messages))
	for _, msg := range messages {
		newMessageID := uuid.New().String()
		messageIDMap[msg.ID] = newMessageID
		if _, err := qtx.CreateMessageAt(ctx, db.CreateMessageAtParams{
			ID:               newMessageID,
			SessionID:        newID,
			Role:             msg.Role,
			Parts:            msg.Parts,
			Model:            msg.Model,
			Provider:         msg.Provider,
			IsSummaryMessage: msg.IsSummaryMessage,
			CreatedAt:        msg.CreatedAt,
			UpdatedAt:        msg.UpdatedAt,
			FinishedAt:       msg.FinishedAt,
		}); err != nil {
			return Session{}, fmt.Errorf("failed to copy message %s: %w", msg.ID, err)
		}
	}

	summaryMessageID := ""
	if remapped, ok := messageIDMap[original.SummaryMessageID]; ok {
		summaryMessageID = remapped
	}

	todosJSON, err := marshalTodos(original.Todos)
	if err != nil {
		return Session{}, fmt.Errorf("failed to marshal todos: %w", err)
	}

	dbSession, err := qtx.UpdateSession(ctx, db.UpdateSessionParams{
		ID:               newID,
		Title:            forkTitle,
		PromptTokens:     original.PromptTokens,
		CompletionTokens: original.CompletionTokens,
		SummaryMessageID: sql.NullString{String: summaryMessageID, Valid: summaryMessageID != ""},
		Cost:             original.Cost,
		Todos:            sql.NullString{String: todosJSON, Valid: todosJSON != ""},
	})
	if err != nil {
		return Session{}, fmt.Errorf("failed to update forked session: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("failed to commit transaction: %w", err)
	}

	forked := s.fromDBItem(dbSession)
	forked.EstimatedUsage = original.EstimatedUsage
	s.setEstimatedUsageState(newID, original.EstimatedUsage)
	s.Publish(pubsub.CreatedEvent, forked)
	event.SessionCreated()
	return forked, nil
}

type deleteLifecycle interface {
	PrepareSessionDelete(context.Context, *db.Queries, string) error
}

type service struct {
	*pubsub.Broker[Session]
	db              *sql.DB
	q               *db.Queries
	deleteLifecycle deleteLifecycle

	// Estimated usage stays in memory so fetch-modify-save paths (e.g.,
	// updating todos or parent-session cost) do not rebuild a session from
	// SQLite and incorrectly clear the UI "~" marker.
	estimatedUsageMu sync.RWMutex
	estimatedUsage   map[string]bool
}

func (s *service) Create(ctx context.Context, title string) (Session, error) {
	dbSession, err := s.q.CreateSession(ctx, db.CreateSessionParams{
		ID:    uuid.New().String(),
		Title: title,
	})
	if err != nil {
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.CreatedEvent, session)
	event.SessionCreated()
	return session, nil
}

func (s *service) CreateTaskSession(ctx context.Context, toolCallID, parentSessionID, title string) (Session, error) {
	dbSession, err := s.q.CreateSession(ctx, db.CreateSessionParams{
		ID:              toolCallID,
		ParentSessionID: sql.NullString{String: parentSessionID, Valid: true},
		Title:           title,
	})
	if err != nil {
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.CreatedEvent, session)
	return session, nil
}

func (s *service) CreateTitleSession(ctx context.Context, parentSessionID string) (Session, error) {
	dbSession, err := s.q.CreateSession(ctx, db.CreateSessionParams{
		ID:              "title-" + parentSessionID,
		ParentSessionID: sql.NullString{String: parentSessionID, Valid: true},
		Title:           "Generate a title",
	})
	if err != nil {
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.CreatedEvent, session)
	return session, nil
}

func (s *service) Delete(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	qtx := s.q.WithTx(tx)

	dbSession, err := qtx.GetSessionByID(ctx, id)
	if err != nil {
		return err
	}
	if s.deleteLifecycle != nil {
		if err := s.deleteLifecycle.PrepareSessionDelete(ctx, qtx, dbSession.ID); err != nil {
			return fmt.Errorf("preparing projection deletion: %w", err)
		}
	}
	if err = qtx.DeleteSessionMessages(ctx, dbSession.ID); err != nil {
		return fmt.Errorf("deleting session messages: %w", err)
	}
	if err = qtx.DeleteSessionFiles(ctx, dbSession.ID); err != nil {
		return fmt.Errorf("deleting session files: %w", err)
	}
	if err = qtx.DeleteSession(ctx, dbSession.ID); err != nil {
		return fmt.Errorf("deleting session: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	session := s.fromDBItem(dbSession)
	s.clearEstimatedUsageState(dbSession.ID)
	s.Publish(pubsub.DeletedEvent, session)
	event.SessionDeleted()
	return nil
}

func (s *service) Get(ctx context.Context, id string) (Session, error) {
	dbSession, err := s.q.GetSessionByID(ctx, id)
	if err != nil {
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.applyEstimatedUsageState(&session)
	return session, nil
}

func (s *service) GetLast(ctx context.Context) (Session, error) {
	dbSession, err := s.q.GetLastSession(ctx)
	if err != nil {
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.applyEstimatedUsageState(&session)
	return session, nil
}

func (s *service) Save(ctx context.Context, session Session) (Session, error) {
	todosJSON, err := marshalTodos(session.Todos)
	if err != nil {
		return Session{}, err
	}

	dbSession, err := s.q.UpdateSession(ctx, db.UpdateSessionParams{
		ID:               session.ID,
		Title:            session.Title,
		PromptTokens:     session.PromptTokens,
		CompletionTokens: session.CompletionTokens,
		SummaryMessageID: sql.NullString{
			String: session.SummaryMessageID,
			Valid:  session.SummaryMessageID != "",
		},
		Cost: session.Cost,
		Todos: sql.NullString{
			String: todosJSON,
			Valid:  todosJSON != "",
		},
		Channel: sql.NullString{
			String: session.Channel,
			Valid:  session.Channel != "",
		},
	})
	if err != nil {
		return Session{}, err
	}
	estimatedUsage := session.EstimatedUsage
	s.setEstimatedUsageState(session.ID, estimatedUsage)
	session = s.fromDBItem(dbSession)
	session.EstimatedUsage = estimatedUsage
	s.Publish(pubsub.UpdatedEvent, session)
	return session, nil
}

func (s *service) SetChannel(ctx context.Context, sessionID, channel string) (Session, error) {
	dbSession, err := s.q.SetSessionChannel(ctx, db.SetSessionChannelParams{
		ID: sessionID,
		Channel: sql.NullString{
			String: channel,
			Valid:  channel != "",
		},
	})
	if err != nil {
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.applyEstimatedUsageState(&session)
	s.Publish(pubsub.UpdatedEvent, session)
	return session, nil
}

// UpdateTitleAndUsage updates only the title and usage fields atomically.
// This is safer than fetching, modifying, and saving the entire session.
// AddUsage atomically adds auxiliary provider usage to session totals without
// replacing the main model's current-context counters.
func (s *service) AddUsage(ctx context.Context, sessionID string, promptTokens, completionTokens int64, cost float64) error {
	if promptTokens < 0 || completionTokens < 0 || cost < 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
		return fmt.Errorf("session usage is invalid")
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		SET prompt_tokens = prompt_tokens + ?,
			completion_tokens = completion_tokens + ?,
			cost = cost + ?,
			updated_at = strftime('%s', 'now')
		WHERE id = ?`, promptTokens, completionTokens, cost, sessionID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("session %s not found", sessionID)
	}
	s.publishSessionUpdate(ctx, sessionID)
	return nil
}

func (s *service) UpdateTitleAndUsage(ctx context.Context, sessionID, title string, promptTokens, completionTokens int64, cost float64) error {
	if err := s.q.UpdateSessionTitleAndUsage(ctx, db.UpdateSessionTitleAndUsageParams{
		ID:               sessionID,
		Title:            title,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		Cost:             cost,
	}); err != nil {
		return err
	}
	s.publishSessionUpdate(ctx, sessionID)
	return nil
}

// Rename updates only the title of a session without touching updated_at or
// usage fields.
func (s *service) Rename(ctx context.Context, id string, title string) error {
	if err := s.q.RenameSession(ctx, db.RenameSessionParams{
		ID:    id,
		Title: title,
	}); err != nil {
		return err
	}
	s.publishSessionUpdate(ctx, id)
	return nil
}

func (s *service) List(ctx context.Context) ([]Session, error) {
	dbSessions, err := s.q.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	sessions := make([]Session, len(dbSessions))
	for i, dbSession := range dbSessions {
		sessions[i] = s.fromDBItem(dbSession)
		s.applyEstimatedUsageState(&sessions[i])
	}
	return sessions, nil
}

// publishSessionUpdate re-fetches a session and publishes an UpdatedEvent so
// that UI subscribers reflect title or usage changes.
func (s *service) publishSessionUpdate(ctx context.Context, sessionID string) {
	session, err := s.Get(ctx, sessionID)
	if err != nil {
		slog.Error("Failed to re-fetch session for event publish", "error", err, "sessionID", sessionID)
		return
	}
	s.Publish(pubsub.UpdatedEvent, session)
}

func (s *service) applyEstimatedUsageState(session *Session) {
	s.estimatedUsageMu.RLock()
	session.EstimatedUsage = s.estimatedUsage[session.ID]
	s.estimatedUsageMu.RUnlock()
}

func (s *service) setEstimatedUsageState(sessionID string, estimatedUsage bool) {
	s.estimatedUsageMu.Lock()
	defer s.estimatedUsageMu.Unlock()
	if estimatedUsage {
		s.estimatedUsage[sessionID] = true
		return
	}
	delete(s.estimatedUsage, sessionID)
}

func (s *service) clearEstimatedUsageState(sessionID string) {
	s.estimatedUsageMu.Lock()
	delete(s.estimatedUsage, sessionID)
	s.estimatedUsageMu.Unlock()
}

func (s *service) fromDBItem(item db.Session) Session {
	todos, err := unmarshalTodos(item.Todos.String)
	if err != nil {
		slog.Error("Failed to unmarshal todos", "session_id", item.ID, "error", err)
	}
	return Session{
		ID:               item.ID,
		ParentSessionID:  item.ParentSessionID.String,
		Title:            item.Title,
		MessageCount:     item.MessageCount,
		PromptTokens:     item.PromptTokens,
		CompletionTokens: item.CompletionTokens,
		SummaryMessageID: item.SummaryMessageID.String,
		Cost:             item.Cost,
		Todos:            todos,
		Channel:          item.Channel.String,
		CreatedAt:        item.CreatedAt,
		UpdatedAt:        item.UpdatedAt,
	}
}

func marshalTodos(todos []Todo) (string, error) {
	if len(todos) == 0 {
		return "", nil
	}
	data, err := json.Marshal(todos)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func unmarshalTodos(data string) ([]Todo, error) {
	if data == "" {
		return []Todo{}, nil
	}
	var todos []Todo
	if err := json.Unmarshal([]byte(data), &todos); err != nil {
		return []Todo{}, err
	}
	return todos, nil
}

// ServiceOption configures session lifecycle behavior.
type ServiceOption func(*service)

// WithDeleteLifecycle adds transaction-bound derived-state cleanup before
// canonical messages are removed during session deletion.
func WithDeleteLifecycle(lifecycle interface {
	PrepareSessionDelete(context.Context, *db.Queries, string) error
},
) ServiceOption {
	return func(s *service) {
		s.deleteLifecycle = lifecycle
	}
}

func NewService(q *db.Queries, conn *sql.DB, opts ...ServiceOption) Service {
	broker := pubsub.NewBroker[Session]()
	s := &service{
		Broker:         broker,
		db:             conn,
		q:              q,
		estimatedUsage: make(map[string]bool),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// CreateAgentToolSessionID creates a session ID for agent tool sessions using the format "messageID$$toolCallID"
func (s *service) CreateAgentToolSessionID(messageID, toolCallID string) string {
	return fmt.Sprintf("%s$$%s", messageID, toolCallID)
}

// ParseAgentToolSessionID parses an agent tool session ID into its components
func (s *service) ParseAgentToolSessionID(sessionID string) (messageID string, toolCallID string, ok bool) {
	parts := strings.Split(sessionID, "$$")
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// IsAgentToolSession checks if a session ID follows the agent tool session format
func (s *service) IsAgentToolSession(sessionID string) bool {
	_, _, ok := s.ParseAgentToolSessionID(sessionID)
	return ok
}
