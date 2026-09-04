package condense

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/google/uuid"
)

// Store owns projection transactions over generated database queries.
type Store struct {
	db *sql.DB
	q  *db.Queries
}

// NewStore constructs a projection store over the application's shared DB.
func NewStore(q *db.Queries, database *sql.DB) *Store {
	return &Store{q: q, db: database}
}

func (s *Store) available() bool {
	return s != nil && s.q != nil && s.db != nil
}

// DeleteMessage invalidates every active projection that depends on messageID,
// deletes their recoverable rows, and deletes the canonical message atomically.
func (s *Store) DeleteMessage(ctx context.Context, messageID string) error {
	if !s.available() {
		return fmt.Errorf("context projection store is unavailable")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin message deletion transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	qtx := s.q.WithTx(tx)
	canonical, err := qtx.GetMessage(ctx, messageID)
	if err != nil {
		return fmt.Errorf("load message for deletion: %w", err)
	}
	nodes, err := qtx.ListActiveContextProjectionNodesBySourceMessage(ctx, db.ListActiveContextProjectionNodesBySourceMessageParams{
		SessionID: canonical.SessionID, SourceMessageID: messageID,
	})
	if err != nil {
		return fmt.Errorf("list message projection nodes: %w", err)
	}
	now := time.Now()
	for _, node := range nodes {
		if err := staleActiveNode(ctx, qtx, node, "source message deleted", now); err != nil {
			return err
		}
	}
	if err := qtx.DeleteMessage(ctx, messageID); err != nil {
		return fmt.Errorf("delete canonical message: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit message deletion: %w", err)
	}
	return nil
}

// PrepareSessionDelete invalidates active projections before a caller deletes
// the session's canonical messages in the same transaction.
func (s *Store) PrepareSessionDelete(ctx context.Context, qtx *db.Queries, sessionID string) error {
	if s == nil || qtx == nil {
		return fmt.Errorf("context projection store is unavailable")
	}
	nodes, err := qtx.ListAllActiveContextProjectionNodesBySession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("list session projection nodes: %w", err)
	}
	now := time.Now()
	for _, node := range nodes {
		if err := staleActiveNode(ctx, qtx, node, "session deleted", now); err != nil {
			return err
		}
	}
	return nil
}

type claim struct {
	nodeID            string
	token             string
	attemptCount      int64
	configurationID   string
	candidate         Candidate
	historyIDs        []string
	historyHash       string
	summaryBoundaryID string
}

type sourceMismatchError struct {
	err error
}

func (e *sourceMismatchError) Error() string { return e.err.Error() }
func (e *sourceMismatchError) Unwrap() error { return e.err }

func (s *Store) node(ctx context.Context, candidate Candidate) (db.ContextProjectionNode, error) {
	return s.q.GetContextProjectionNodeForSource(ctx, db.GetContextProjectionNodeForSourceParams{
		SessionID: candidate.SessionID, BatchKey: candidate.BatchKey,
		SourceHash: candidate.SourceHash, AlgorithmVersion: AlgorithmVersion,
	})
}

func (s *Store) activeNode(ctx context.Context, candidate Candidate) (db.ContextProjectionNode, error) {
	return s.q.GetActiveContextProjectionNodeForSource(ctx, db.GetActiveContextProjectionNodeForSourceParams{
		SessionID: candidate.SessionID, BatchKey: candidate.BatchKey,
		SourceHash: candidate.SourceHash, AlgorithmVersion: AlgorithmVersion,
	})
}

func (s *Store) nodeForConfiguration(ctx context.Context, candidate Candidate, configurationID string) (db.ContextProjectionNode, error) {
	return s.q.GetContextProjectionNode(ctx, db.GetContextProjectionNodeParams{
		SessionID: candidate.SessionID, BatchKey: candidate.BatchKey,
		SourceHash: candidate.SourceHash, AlgorithmVersion: AlgorithmVersion,
		ConfigurationID: configurationID,
	})
}

func (s *Store) claim(ctx context.Context, candidate Candidate, now time.Time, lease time.Duration) (claim, bool, error) {
	return s.claimForHistory(ctx, candidate, Options{}.normalized().ConfigurationID, nil, now, lease)
}

func (s *Store) claimForHistory(
	ctx context.Context,
	candidate Candidate,
	configurationID string,
	history []message.Message,
	now time.Time,
	lease time.Duration,
) (claim, bool, error) {
	if configurationID == "" || now.IsZero() || lease <= 0 {
		return claim{}, false, fmt.Errorf("claim candidate: invalid configuration")
	}
	sessionRow, err := s.q.GetSessionByID(ctx, candidate.SessionID)
	if err != nil {
		return claim{}, false, fmt.Errorf("claim candidate: load session boundary: %w", err)
	}
	summaryBoundaryID := sessionRow.SummaryMessageID.String
	rows, err := s.q.ListCanonicalContextProjectionMessages(ctx, candidate.SessionID)
	if err != nil {
		return claim{}, false, fmt.Errorf("claim candidate: list canonical history: %w", err)
	}
	canonical, err := decodeCanonicalMessages(rows)
	if err != nil {
		return claim{}, false, fmt.Errorf("claim candidate: %w", err)
	}
	if history == nil {
		history, err = summaryBoundedHistory(canonical, summaryBoundaryID)
		if err != nil {
			return claim{}, false, fmt.Errorf("claim candidate: %w", err)
		}
	}
	historyIDs := make([]string, len(history))
	for index, msg := range history {
		if msg.SessionID != candidate.SessionID || msg.ID == "" {
			return claim{}, false, fmt.Errorf("claim candidate: invalid supplied history")
		}
		historyIDs[index] = msg.ID
	}
	boundedCanonical, err := exactSummaryBoundedHistory(canonical, historyIDs, summaryBoundaryID)
	if err != nil {
		return claim{}, false, fmt.Errorf("claim candidate: supplied history is not current summary-bounded history: %w", err)
	}
	canonicalHash, err := projectionHistoryHash(boundedCanonical)
	if err != nil {
		return claim{}, false, fmt.Errorf("claim candidate: hash canonical history: %w", err)
	}
	computedHistoryHash, err := projectionHistoryHash(history)
	if err != nil {
		return claim{}, false, fmt.Errorf("claim candidate: hash supplied history: %w", err)
	}
	if computedHistoryHash != canonicalHash {
		return claim{}, false, fmt.Errorf("claim candidate: supplied history content differs from canonical history")
	}
	historyHash := computedHistoryHash
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return claim{}, false, fmt.Errorf("begin claim transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	qtx := s.q.WithTx(tx)
	token := uuid.New().String()
	nowUnix := now.Unix()
	leaseUntil := now.Add(lease).Unix()
	params := db.CreateContextProjectionClaimParams{
		ID: uuid.New().String(), SessionID: candidate.SessionID, BatchKey: candidate.BatchKey,
		SourceHash: candidate.SourceHash, AlgorithmVersion: AlgorithmVersion, ConfigurationID: configurationID,
		FirstMessageID: candidate.FirstMessageID, LastMessageID: candidate.LastMessageID,
		RawChars: int64(candidate.RawChars), LeaseExpiresAt: sql.NullInt64{Int64: leaseUntil, Valid: true},
		ClaimToken: sql.NullString{String: token, Valid: true}, CreatedAt: nowUnix, UpdatedAt: nowUnix,
	}
	node, err := qtx.CreateContextProjectionClaim(ctx, params)
	if errors.Is(err, sql.ErrNoRows) {
		node, err = qtx.ReclaimContextProjectionClaim(ctx, db.ReclaimContextProjectionClaimParams{
			LeaseExpiresAt: sql.NullInt64{Int64: leaseUntil, Valid: true},
			ClaimToken:     sql.NullString{String: token, Valid: true}, UpdatedAt: nowUnix,
			SessionID: candidate.SessionID, BatchKey: candidate.BatchKey, SourceHash: candidate.SourceHash,
			AlgorithmVersion: AlgorithmVersion, ConfigurationID: configurationID,
			Now: sql.NullInt64{Int64: nowUnix, Valid: true},
		})
	}
	if errors.Is(err, sql.ErrNoRows) {
		return claim{}, false, nil
	}
	if err != nil {
		return claim{}, false, fmt.Errorf("claim candidate: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return claim{}, false, fmt.Errorf("commit claim: %w", err)
	}
	return claim{
		nodeID: node.ID, token: token, attemptCount: node.AttemptCount,
		configurationID: configurationID, candidate: candidate, historyIDs: historyIDs,
		historyHash: historyHash, summaryBoundaryID: summaryBoundaryID,
	}, true, nil
}

func (s *Store) skip(ctx context.Context, claimed claim, reason string, now time.Time) error {
	rows, err := s.q.SkipContextProjectionClaim(ctx, db.SkipContextProjectionClaimParams{
		Failure: boundedFailure(reason), UpdatedAt: now.Unix(), ID: claimed.nodeID,
		SessionID: claimed.candidate.SessionID, AlgorithmVersion: AlgorithmVersion,
		ClaimToken: sql.NullString{String: claimed.token, Valid: true},
	})
	return requireFencedRow(rows, err, "skip claim")
}

func (s *Store) fail(ctx context.Context, claimed claim, failure string, retryAfter *time.Time, usage Usage, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin failed-claim transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	qtx := s.q.WithTx(tx)
	retry := sql.NullInt64{}
	if retryAfter != nil {
		retry = sql.NullInt64{Int64: retryAfter.Unix(), Valid: true}
	}
	rows, err := qtx.FailContextProjectionClaim(ctx, db.FailContextProjectionClaimParams{
		RetryAfter: retry, Failure: boundedFailure(failure),
		SummarizerProvider: usage.Provider, SummarizerModel: usage.Model,
		PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens, Cost: usage.Cost,
		UpdatedAt: now.Unix(), ID: claimed.nodeID, SessionID: claimed.candidate.SessionID,
		AlgorithmVersion: AlgorithmVersion, ClaimToken: sql.NullString{String: claimed.token, Valid: true},
	})
	if err := requireFencedRow(rows, err, "fail claim"); err != nil {
		return err
	}
	if err := createUsageAttempt(ctx, qtx, claimed, usage, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit failed claim: %w", err)
	}
	return nil
}

func (s *Store) activate(ctx context.Context, claimed claim, summary Summary, now time.Time) (bool, error) {
	summary.Text = neutralizeProjectionMarkers(summary.Text)
	for index := range summary.Items {
		summary.Items[index].Description = neutralizeProjectionMarkers(summary.Items[index].Description)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin activation transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	qtx := s.q.WithTx(tx)

	if _, err := qtx.GetContextProjectionClaim(ctx, db.GetContextProjectionClaimParams{
		ID: claimed.nodeID, SessionID: claimed.candidate.SessionID, AlgorithmVersion: AlgorithmVersion,
		ClaimToken: sql.NullString{String: claimed.token, Valid: true},
	}); err != nil {
		return false, fmt.Errorf("verify claim fence: %w", err)
	}

	reloaded, err := s.reloadCandidate(ctx, qtx, claimed)
	var mismatch *sourceMismatchError
	if err != nil && !errors.As(err, &mismatch) {
		return false, fmt.Errorf("reload activation source: %w", err)
	}
	if err != nil || reloaded.BatchKey != claimed.candidate.BatchKey || reloaded.SourceHash != claimed.candidate.SourceHash {
		usage := summaryUsage(summary)
		if staleErr := staleClaim(ctx, qtx, claimed, "source changed", usage, now); staleErr != nil {
			return false, staleErr
		}
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("commit stale claim: %w", err)
		}
		return false, &ProjectionError{
			Class: ErrorClassStale, NodeID: claimed.nodeID, BatchKey: claimed.candidate.BatchKey,
			Recoverable: true, Err: fmt.Errorf("source changed before activation"),
		}
	}
	if err := staleConflictingActive(ctx, qtx, reloaded, now); err != nil {
		return false, err
	}

	nextRef, err := qtx.GetContextProjectionRefCounter(ctx)
	if err != nil {
		return false, fmt.Errorf("load ref counter: %w", err)
	}
	if int64(len(reloaded.Items)) > int64Max-nextRef {
		return false, fmt.Errorf("ref counter overflow")
	}
	prospective := make([]db.ContextProjectionItem, len(reloaded.Items))
	for index, item := range reloaded.Items {
		prospective[index] = db.ContextProjectionItem{
			ID: uuid.New().String(), NodeID: claimed.nodeID, SessionID: reloaded.SessionID,
			SourceMessageID: item.MessageID, SourcePartOrdinal: int64(item.PartOrdinal),
			ToolCallID: item.ToolCallID, ToolName: item.ToolName,
			Description: summary.Items[index].Description, RefNumber: nextRef + int64(index),
			SourceHash: item.SourceHash, Ordinal: int64(index), AlgorithmVersion: AlgorithmVersion,
		}
	}
	_, projectedChars, err := renderProjection(summary.Text, prospective)
	if err != nil {
		return false, fmt.Errorf("render prospective projection: %w", err)
	}
	if projectedChars >= reloaded.RawChars {
		rows, err := qtx.SkipContextProjectionClaim(ctx, db.SkipContextProjectionClaimParams{
			Failure: "projection does not save space", SummarizerProvider: summary.Provider,
			SummarizerModel: summary.Model, PromptTokens: summary.PromptTokens,
			CompletionTokens: summary.CompletionTokens, Cost: summary.Cost, UpdatedAt: now.Unix(),
			ID: claimed.nodeID, SessionID: reloaded.SessionID, AlgorithmVersion: AlgorithmVersion,
			ClaimToken: sql.NullString{String: claimed.token, Valid: true},
		})
		if err := requireFencedRow(rows, err, "skip non-saving claim"); err != nil {
			return false, err
		}
		if err := createUsageAttempt(ctx, qtx, claimed, summaryUsage(summary), now); err != nil {
			return false, err
		}
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("commit skipped claim: %w", err)
		}
		return false, nil
	}

	firstRef, err := qtx.ReserveContextProjectionRefs(ctx, int64(len(prospective)))
	if err != nil {
		return false, fmt.Errorf("reserve refs: %w", err)
	}
	if firstRef != nextRef {
		return false, fmt.Errorf("ref counter changed inside activation transaction")
	}
	for ordinal, source := range reloaded.Sources {
		if err := qtx.CreateContextProjectionSource(ctx, db.CreateContextProjectionSourceParams{
			NodeID: claimed.nodeID, SessionID: reloaded.SessionID, SourceMessageID: source.MessageID,
			SourcePartOrdinal: int64(source.PartOrdinal), PartKind: source.PartKind,
			SourceHash: source.SourceHash, Ordinal: int64(ordinal),
		}); err != nil {
			return false, fmt.Errorf("insert source %d: %w", ordinal, err)
		}
	}
	for _, item := range prospective {
		if err := qtx.CreateContextProjectionItem(ctx, db.CreateContextProjectionItemParams(item)); err != nil {
			return false, fmt.Errorf("insert item %d: %w", item.Ordinal, err)
		}
	}
	rows, err := qtx.ActivateContextProjectionClaim(ctx, db.ActivateContextProjectionClaimParams{
		Summary: summary.Text, ProjectedChars: int64(projectedChars), SummarizerProvider: summary.Provider,
		SummarizerModel: summary.Model, PromptTokens: summary.PromptTokens,
		CompletionTokens: summary.CompletionTokens, Cost: summary.Cost, UpdatedAt: now.Unix(),
		ID: claimed.nodeID, SessionID: reloaded.SessionID, AlgorithmVersion: AlgorithmVersion,
		ClaimToken: sql.NullString{String: claimed.token, Valid: true},
	})
	if err := requireFencedRow(rows, err, "activate claim"); err != nil {
		return false, err
	}
	if err := createUsageAttempt(ctx, qtx, claimed, summaryUsage(summary), now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit activation: %w", err)
	}
	return true, nil
}

func (s *Store) reloadCandidate(ctx context.Context, qtx *db.Queries, value any) (Candidate, error) {
	var claimed claim
	switch value := value.(type) {
	case claim:
		claimed = value
	case Candidate:
		claimed.candidate = value
		history := append([]message.Message{value.Assistant}, value.ToolMessages...)
		claimed.historyIDs = make([]string, len(history))
		for index, msg := range history {
			row, err := qtx.GetMessage(ctx, msg.ID)
			if errors.Is(err, sql.ErrNoRows) {
				return Candidate{}, &sourceMismatchError{err: fmt.Errorf("source message %s is missing", msg.ID)}
			}
			if err != nil {
				return Candidate{}, fmt.Errorf("load source message %s: %w", msg.ID, err)
			}
			if row.SessionID != value.SessionID {
				return Candidate{}, &sourceMismatchError{err: fmt.Errorf("source message %s belongs to another session", msg.ID)}
			}
			claimed.historyIDs[index] = msg.ID
		}
		historyHash, hashErr := projectionHistoryHash(history)
		if hashErr != nil {
			return Candidate{}, fmt.Errorf("hash activation source history: %w", hashErr)
		}
		claimed.historyHash = historyHash
	default:
		return Candidate{}, fmt.Errorf("unsupported activation source")
	}
	sessionRow, err := qtx.GetSessionByID(ctx, claimed.candidate.SessionID)
	if err != nil {
		return Candidate{}, fmt.Errorf("load activation session: %w", err)
	}
	if sessionRow.SummaryMessageID.String != claimed.summaryBoundaryID || sessionRow.SummaryMessageID.Valid != (claimed.summaryBoundaryID != "") {
		return Candidate{}, &sourceMismatchError{err: fmt.Errorf("summary history boundary moved")}
	}

	rows, err := qtx.ListCanonicalContextProjectionMessages(ctx, claimed.candidate.SessionID)
	if err != nil {
		return Candidate{}, fmt.Errorf("list canonical session messages: %w", err)
	}
	messages, err := decodeCanonicalMessages(rows)
	if err != nil {
		return Candidate{}, err
	}

	bounded, err := exactSummaryBoundedHistory(messages, claimed.historyIDs, claimed.summaryBoundaryID)
	if err != nil {
		return Candidate{}, &sourceMismatchError{err: err}
	}
	historyHash, err := projectionHistoryHash(bounded)
	if err != nil {
		return Candidate{}, fmt.Errorf("hash activation history: %w", err)
	}
	if historyHash != claimed.historyHash {
		return Candidate{}, &sourceMismatchError{err: fmt.Errorf("supplied history content changed")}
	}
	candidates := scanCandidates(bounded)
	for _, candidate := range candidates {
		if candidate.BatchKey == claimed.candidate.BatchKey {
			if candidate.FirstMessageID != claimed.candidate.FirstMessageID || candidate.LastMessageID != claimed.candidate.LastMessageID {
				return Candidate{}, &sourceMismatchError{err: fmt.Errorf("source boundary moved")}
			}
			return candidate, nil
		}
	}
	return Candidate{}, &sourceMismatchError{err: fmt.Errorf("source batch is no longer complete in supplied history")}
}

func projectionHistoryHash(messages []message.Message) (string, error) {
	e := newHashEncoder("crush-context-projection-history-v1")
	e.integer("message_count", int64(len(messages)))
	for _, msg := range messages {
		if err := encodeMessage(e, msg); err != nil {
			return "", err
		}
	}
	return e.sum(), nil
}

func decodeCanonicalMessages(rows []db.Message) ([]message.Message, error) {
	messages := make([]message.Message, len(rows))
	for index, row := range rows {
		parts, err := message.DecodeParts([]byte(row.Parts))
		if err != nil {
			return nil, fmt.Errorf("decode source message %s: %w", row.ID, err)
		}
		messages[index] = message.Message{
			ID: row.ID, SessionID: row.SessionID, Role: message.MessageRole(row.Role), Parts: parts,
			Model: row.Model.String, Provider: row.Provider.String, CreatedAt: row.CreatedAt,
			UpdatedAt: row.UpdatedAt, IsSummaryMessage: row.IsSummaryMessage != 0,
		}
	}
	return messages, nil
}

func summaryBoundedHistory(canonical []message.Message, summaryBoundaryID string) ([]message.Message, error) {
	start := 0
	if summaryBoundaryID != "" {
		start = -1
		for index, msg := range canonical {
			if msg.ID == summaryBoundaryID {
				start = index
				break
			}
		}
		if start < 0 {
			return nil, fmt.Errorf("summary history boundary is missing")
		}
	}
	bounded := append([]message.Message(nil), canonical[start:]...)
	if len(bounded) > 0 && bounded[0].IsSummaryMessage {
		bounded[0].Role = message.User
	}
	return bounded, nil
}

func exactSummaryBoundedHistory(canonical []message.Message, suppliedIDs []string, summaryBoundaryID string) ([]message.Message, error) {
	bounded, err := summaryBoundedHistory(canonical, summaryBoundaryID)
	if err != nil {
		return nil, err
	}
	if len(suppliedIDs) == 0 || len(suppliedIDs) != len(bounded) {
		return nil, fmt.Errorf("supplied history boundary changed")
	}
	for index, id := range suppliedIDs {
		if bounded[index].ID != id {
			return nil, fmt.Errorf("canonical history changed at supplied position %d", index)
		}
	}
	return bounded, nil
}

// AccountUsage atomically adds every unaccounted projection attempt to the
// owning session and marks those rows accounted in the same transaction.
func (s *Store) AccountUsage(ctx context.Context, sessionID string) error {
	if !s.available() {
		return fmt.Errorf("context projection store is unavailable")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin projection usage transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	qtx := s.q.WithTx(tx)
	nodes, err := qtx.ListUnaccountedContextProjectionUsage(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("list unaccounted projection usage: %w", err)
	}
	var promptTokens, completionTokens int64
	var cost float64
	for _, attempt := range nodes {
		if attempt.PromptTokens > int64Max-promptTokens || attempt.CompletionTokens > int64Max-completionTokens {
			return fmt.Errorf("context projection usage overflow")
		}
		if math.IsNaN(attempt.Cost) || math.IsInf(attempt.Cost, 0) || attempt.Cost > math.MaxFloat64-cost {
			return fmt.Errorf("context projection cost overflow")
		}
		promptTokens += attempt.PromptTokens
		completionTokens += attempt.CompletionTokens
		cost += attempt.Cost
		rows, err := qtx.AccountContextProjectionUsageAttempt(ctx, db.AccountContextProjectionUsageAttemptParams{
			UpdatedAt: time.Now().Unix(), NodeID: attempt.NodeID,
			AttemptNumber: attempt.AttemptNumber, SessionID: sessionID,
		})
		if err := requireFencedRow(rows, err, "account projection usage"); err != nil {
			return err
		}
	}
	if len(nodes) == 0 {
		return nil
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE sessions
		SET prompt_tokens = prompt_tokens + ?,
			completion_tokens = completion_tokens + ?,
			cost = cost + ?,
			updated_at = strftime('%s', 'now')
		WHERE id = ?`, promptTokens, completionTokens, cost, sessionID)
	if err != nil {
		return fmt.Errorf("add projection usage to session: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read projection usage session update: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("projection usage session %s not found", sessionID)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit projection usage: %w", err)
	}
	return nil
}

func (s *Store) active(ctx context.Context, sessionID string) ([]activeProjection, error) {
	nodes, err := s.q.ListActiveContextProjectionNodes(ctx, db.ListActiveContextProjectionNodesParams{
		SessionID: sessionID, AlgorithmVersion: AlgorithmVersion,
	})
	if err != nil {
		return nil, fmt.Errorf("list active nodes: %w", err)
	}
	active := make([]activeProjection, 0, len(nodes))
	for _, node := range nodes {
		sources, err := s.q.ListContextProjectionSourcesByNode(ctx, node.ID)
		if err != nil {
			return nil, fmt.Errorf("list node sources: %w", err)
		}
		items, err := s.q.ListContextProjectionItemsByNode(ctx, node.ID)
		if err != nil {
			return nil, fmt.Errorf("list node items: %w", err)
		}
		active = append(active, activeProjection{node: node, sources: sources, items: items})
	}
	return active, nil
}

// SessionStats summarizes projection savings across the session's active
// batches. RawChars is the canonical characters replaced by ProjectedChars
// of provider-facing copies.
func (s *Store) SessionStats(ctx context.Context, sessionID string) (SessionStats, error) {
	var stats SessionStats
	nodes, err := s.q.ListActiveContextProjectionNodes(ctx, db.ListActiveContextProjectionNodesParams{
		SessionID: sessionID, AlgorithmVersion: AlgorithmVersion,
	})
	if err != nil {
		return stats, fmt.Errorf("list active nodes: %w", err)
	}
	stats.Batches = int64(len(nodes))
	for _, node := range nodes {
		stats.RawChars += node.RawChars
		stats.ProjectedChars += node.ProjectedChars
	}
	return stats, nil
}

func (s *Store) staleActive(ctx context.Context, projection activeProjection, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin stale active transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if err := staleActiveNode(ctx, s.q.WithTx(tx), projection.node, "active source changed", now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit stale active node: %w", err)
	}
	return nil
}

func staleConflictingActive(ctx context.Context, qtx *db.Queries, candidate Candidate, now time.Time) error {
	seen := make(map[string]struct{})
	for _, item := range candidate.Items {
		nodes, err := qtx.ListConflictingActiveContextProjectionNodes(ctx, db.ListConflictingActiveContextProjectionNodesParams{
			SessionID: candidate.SessionID, AlgorithmVersion: AlgorithmVersion,
			SourceMessageID: item.MessageID, SourcePartOrdinal: int64(item.PartOrdinal),
		})
		if err != nil {
			return fmt.Errorf("list conflicting active nodes: %w", err)
		}
		for _, node := range nodes {
			if _, ok := seen[node.ID]; ok {
				continue
			}
			seen[node.ID] = struct{}{}
			if err := staleActiveNode(ctx, qtx, node, "superseded by changed source", now); err != nil {
				return err
			}
		}
	}
	return nil
}

func staleActiveNode(ctx context.Context, qtx *db.Queries, node db.ContextProjectionNode, failure string, now time.Time) error {
	rows, err := qtx.StaleActiveContextProjectionNode(ctx, db.StaleActiveContextProjectionNodeParams{
		Failure: boundedFailure(failure), UpdatedAt: now.Unix(), ID: node.ID,
		SessionID: node.SessionID, AlgorithmVersion: node.AlgorithmVersion,
	})
	if err := requireFencedRow(rows, err, "stale active node"); err != nil {
		return err
	}
	if err := qtx.DeleteContextProjectionItemsByNode(ctx, node.ID); err != nil {
		return fmt.Errorf("delete stale active items: %w", err)
	}
	if err := qtx.DeleteContextProjectionSourcesByNode(ctx, node.ID); err != nil {
		return fmt.Errorf("delete stale active sources: %w", err)
	}
	return nil
}

func summaryUsage(summary Summary) Usage {
	return Usage{
		Provider: summary.Provider, Model: summary.Model,
		PromptTokens: summary.PromptTokens, CompletionTokens: summary.CompletionTokens, Cost: summary.Cost,
	}
}

func createUsageAttempt(ctx context.Context, qtx *db.Queries, claimed claim, usage Usage, now time.Time) error {
	if usage.PromptTokens == 0 && usage.CompletionTokens == 0 && usage.Cost == 0 {
		return nil
	}
	if err := qtx.CreateContextProjectionUsageAttempt(ctx, db.CreateContextProjectionUsageAttemptParams{
		NodeID: claimed.nodeID, SessionID: claimed.candidate.SessionID, AttemptNumber: claimed.attemptCount,
		SummarizerProvider: usage.Provider, SummarizerModel: usage.Model,
		PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
		Cost: usage.Cost, CreatedAt: now.Unix(), UpdatedAt: now.Unix(),
	}); err != nil {
		return fmt.Errorf("record projection usage attempt: %w", err)
	}
	return nil
}

func staleClaim(ctx context.Context, qtx *db.Queries, claimed claim, failure string, usage Usage, now time.Time) error {
	if err := qtx.DeleteContextProjectionItemsByNode(ctx, claimed.nodeID); err != nil {
		return fmt.Errorf("delete stale items: %w", err)
	}
	if err := qtx.DeleteContextProjectionSourcesByNode(ctx, claimed.nodeID); err != nil {
		return fmt.Errorf("delete stale sources: %w", err)
	}
	rows, err := qtx.StaleContextProjectionClaim(ctx, db.StaleContextProjectionClaimParams{
		Failure: boundedFailure(failure), SummarizerProvider: usage.Provider, SummarizerModel: usage.Model,
		PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens, Cost: usage.Cost,
		UpdatedAt: now.Unix(), ID: claimed.nodeID, SessionID: claimed.candidate.SessionID,
		AlgorithmVersion: AlgorithmVersion, ClaimToken: sql.NullString{String: claimed.token, Valid: true},
	})
	if err := requireFencedRow(rows, err, "stale claim"); err != nil {
		return err
	}
	return createUsageAttempt(ctx, qtx, claimed, usage, now)
}

func requireFencedRow(rows int64, err error, operation string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if rows != 1 {
		return fmt.Errorf("%s: claim fence rejected transition", operation)
	}
	return nil
}

func neutralizeProjectionMarkers(value string) string {
	value = strings.ReplaceAll(value, "[Context projection]", "[Context-projection]")
	return strings.ReplaceAll(value, "[Projected as", "[Projected-as")
}

func boundedFailure(value string) string {
	value = normalizeModelText(value, 1000)
	if utf8.RuneCountInString(value) == 0 {
		return "unspecified failure"
	}
	return value
}

const int64Max = int64(^uint64(0) >> 1)
