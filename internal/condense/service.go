package condense

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
)

// Module orchestrates scanning, claims, summarization, activation, rendering,
// and exact recovery.
type Module struct {
	store      *Store
	summarizer Summarizer

	mu      sync.RWMutex
	options Options
	now     func() time.Time
}

// New constructs a projection module. A nil summarizer leaves eligible
// batches raw while retaining query support.
func New(store *Store, summarizer Summarizer, options Options) *Module {
	return &Module{store: store, summarizer: summarizer, options: options.normalized(), now: time.Now}
}

// UpdateConfiguration atomically replaces the policy and summarizer used by
// future Project calls. An in-flight call continues with its entry snapshot.
func (m *Module) UpdateConfiguration(summarizer Summarizer, options Options) {
	m.mu.Lock()
	m.summarizer = summarizer
	m.options = options.normalized()
	m.mu.Unlock()
}

// UpdateOptions atomically replaces the policy used by future Project calls.
func (m *Module) UpdateOptions(options Options) {
	m.mu.Lock()
	m.options = options.normalized()
	m.mu.Unlock()
}

// Options returns the immutable configuration snapshot used by new calls.
func (m *Module) Options() Options {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.options
}

func (m *Module) configuration() (Summarizer, Options) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.summarizer, m.options
}

// AccountUsage persists any durable, unaccounted summarizer usage exactly once.
func (m *Module) AccountUsage(ctx context.Context, sessionID string) error {
	return m.store.AccountUsage(ctx, sessionID)
}

// Project returns provider-facing copies while preserving canonical input.
func (m *Module) Project(ctx context.Context, sessionID string, messages []message.Message) ([]message.Message, error) {
	summarizer, options := m.configuration()
	if !options.Enabled || len(messages) == 0 {
		return messages, nil
	}
	if cause := context.Cause(ctx); cause != nil {
		return messages, fatal(ErrorClassStore, cause)
	}
	if !m.store.available() {
		return messages, recoverable(ErrorClassStore, fmt.Errorf("projection store is unavailable"))
	}

	candidates := scanCandidates(messages)
	recent := make(map[string]struct{})
	windowStart := len(candidates) - options.KeepRecentBatches
	if windowStart < 0 {
		windowStart = 0
	}
	for _, candidate := range candidates[windowStart:] {
		recent[candidate.BatchKey] = struct{}{}
	}

	if _, err := m.validActive(ctx, sessionID, messages, recent); err != nil {
		return messages, err
	}

	if summarizer != nil {
		for _, candidate := range candidates[:windowStart] {
			if candidate.SessionID != sessionID {
				continue
			}
			node, nodeErr := m.store.activeNode(ctx, candidate)
			if errors.Is(nodeErr, sql.ErrNoRows) {
				node, nodeErr = m.store.nodeForConfiguration(ctx, candidate, options.ConfigurationID)
			}
			if nodeErr != nil && !errors.Is(nodeErr, sql.ErrNoRows) {
				return messages, projectError(ctx, ErrorClassStore, nodeErr)
			}
			if cause := context.Cause(ctx); cause != nil {
				return messages, fatal(ErrorClassStore, cause)
			}
			if nodeErr == nil && nodeSuppressesCandidate(node, m.now()) {
				if node.State == "active" {
					continue
				}
				return m.renderActive(ctx, sessionID, messages, recent)
			}
			if candidate.RawChars < options.MinBatchChars {
				claimed, ok, err := m.store.claimForHistory(ctx, candidate, options.ConfigurationID, messages, m.now(), options.SummarizerTimeout+defaultLeaseMargin)
				if err != nil {
					return messages, projectError(ctx, ErrorClassStore, err)
				}
				if !ok {
					continue
				}
				if err := m.store.skip(ctx, claimed, "batch is below minimum size", m.now()); err != nil {
					return messages, projectError(ctx, ErrorClassStore, err)
				}
				break
			}

			claimed, ok, err := m.store.claimForHistory(ctx, candidate, options.ConfigurationID, messages, m.now(), options.SummarizerTimeout+defaultLeaseMargin)
			if err != nil {
				return messages, projectError(ctx, ErrorClassStore, err)
			}
			if !ok {
				continue
			}

			summaryCtx, cancel := context.WithTimeout(ctx, options.SummarizerTimeout)
			summary, summaryErr := summarizer.Summarize(summaryCtx, candidate)
			childErr := summaryCtx.Err()
			cancel()
			if summaryErr != nil || childErr != nil {
				if childErr != nil {
					var classified *SummaryError
					if errors.As(summaryErr, &classified) {
						summaryErr = RetryableSummaryErrorWithUsage(childErr, classified.Usage)
					} else {
						summaryErr = RetryableSummaryErrorWithUsage(childErr, validSummaryUsage(summary))
					}
				}
				if err := m.recordSummaryFailure(ctx, claimed, summaryErr, false, Usage{}); err != nil {
					return messages, err
				}
				return m.renderActive(ctx, sessionID, messages, recent)
			}
			if cause := context.Cause(ctx); cause != nil {
				if err := m.recordSummaryFailure(ctx, claimed, RetryableSummaryErrorWithUsage(cause, validSummaryUsage(summary)), false, Usage{}); err != nil {
					return messages, err
				}
				return messages, fatal(ErrorClassSummarizer, cause)
			}

			usage := validSummaryUsage(summary)
			summary, err = validateSummary(summary, len(candidate.Items))
			if err != nil {
				if err := m.recordSummaryFailure(ctx, claimed, err, true, usage); err != nil {
					return messages, err
				}
				return m.renderActive(ctx, sessionID, messages, recent)
			}
			if _, err := m.store.activate(ctx, claimed, summary, m.now()); err != nil {
				if cause := context.Cause(ctx); cause != nil {
					return messages, fatal(ErrorClassStore, cause)
				}
				if IsRecoverable(err) {
					return m.renderActive(ctx, sessionID, messages, recent)
				}
				return messages, projectError(ctx, ErrorClassStore, err)
			}
			break
		}
	}
	return m.renderActive(ctx, sessionID, messages, recent)
}

func (m *Module) recordSummaryFailure(ctx context.Context, claimed claim, summaryErr error, validation bool, usage Usage) error {
	class := ErrorClassSummarizer
	failure := "summarizer retryable failure"
	var retryAfter *time.Time
	retry := true
	var classified *SummaryError
	if errors.As(summaryErr, &classified) && classified.Class == SummaryFailureTerminal {
		retry = false
		failure = "summarizer terminal failure"
	}
	if classified != nil {
		usage = classified.Usage
	}
	if validation {
		class = ErrorClassValidation
		failure = "summary validation failure"
	}
	if retry {
		value := m.now().Add(retryBackoff(claimed.attemptCount))
		retryAfter = &value
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	recordErr := m.store.fail(cleanupCtx, claimed, failure, retryAfter, usage, m.now())
	cleanupCancel()
	if cause := context.Cause(ctx); cause != nil {
		return fatal(class, cause)
	}
	if recordErr != nil {
		return recoverable(ErrorClassStore, fmt.Errorf("record sanitized summary failure: %w", recordErr))
	}
	return nil
}

func (m *Module) renderActive(ctx context.Context, sessionID string, messages []message.Message, recent map[string]struct{}) ([]message.Message, error) {
	active, err := m.validActive(ctx, sessionID, messages, recent)
	if err != nil {
		return messages, err
	}
	if cause := context.Cause(ctx); cause != nil {
		return messages, fatal(ErrorClassStore, cause)
	}
	return applyProjections(messages, active), nil
}

func (m *Module) validActive(ctx context.Context, sessionID string, messages []message.Message, recent map[string]struct{}) ([]activeProjection, error) {
	active, err := m.store.active(ctx, sessionID)
	if err != nil {
		return nil, projectError(ctx, ErrorClassStore, err)
	}
	eligible := make([]activeProjection, 0, len(active))
	for _, projection := range active {
		if projectionSourceMessagesPresent(messages, projection) && !projectionValid(messages, projection) {
			if err := m.store.staleActive(ctx, projection, m.now()); err != nil {
				return nil, projectError(ctx, ErrorClassStore, err)
			}
			continue
		}
		if !projectionValid(messages, projection) {
			continue
		}
		if _, protected := recent[projection.node.BatchKey]; !protected {
			eligible = append(eligible, projection)
		}
	}
	return eligible, nil
}

func projectionSourceMessagesPresent(messages []message.Message, projection activeProjection) bool {
	if len(projection.sources) == 0 {
		return true
	}
	present := make(map[string]struct{}, len(messages))
	for _, msg := range messages {
		if msg.SessionID == projection.node.SessionID {
			present[msg.ID] = struct{}{}
		}
	}
	for _, source := range projection.sources {
		if _, ok := present[source.SourceMessageID]; !ok {
			return false
		}
	}
	return true
}

func validSummaryUsage(summary Summary) Usage {
	if summary.PromptTokens < 0 || summary.CompletionTokens < 0 || summary.Cost < 0 || math.IsNaN(summary.Cost) || math.IsInf(summary.Cost, 0) {
		return Usage{}
	}
	return summaryUsage(summary)
}

func validateSummary(summary Summary, itemCount int) (Summary, error) {
	if summary.Truncated {
		return Summary{}, fmt.Errorf("summary output was truncated")
	}
	summary.Text = normalizeModelText(summary.Text, maxSummaryChars)
	if summary.Text == "" {
		return Summary{}, fmt.Errorf("summary is empty")
	}
	if len(summary.Items) != itemCount {
		return Summary{}, fmt.Errorf("summary item count does not match candidate")
	}
	items := make([]SummaryItem, itemCount)
	seen := make([]bool, itemCount)
	for _, item := range summary.Items {
		if item.Ordinal < 0 || item.Ordinal >= itemCount || seen[item.Ordinal] {
			return Summary{}, fmt.Errorf("summary has duplicate or unknown item ordinal")
		}
		item.Description = normalizeModelText(item.Description, maxDescriptionChars)
		if item.Description == "" {
			return Summary{}, fmt.Errorf("summary item description is empty")
		}
		seen[item.Ordinal] = true
		items[item.Ordinal] = item
	}
	if summary.PromptTokens < 0 || summary.CompletionTokens < 0 || summary.Cost < 0 || math.IsNaN(summary.Cost) || math.IsInf(summary.Cost, 0) {
		return Summary{}, fmt.Errorf("summary usage metadata is invalid")
	}
	summary.Items = items
	return summary, nil
}

func retryBackoff(attempt int64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	backoff := 5 * time.Minute
	for range attempt - 1 {
		if backoff >= time.Hour/2 {
			return time.Hour
		}
		backoff *= 2
	}
	if backoff > time.Hour {
		return time.Hour
	}
	return backoff
}

func nodeSuppressesCandidate(node db.ContextProjectionNode, now time.Time) bool {
	switch node.State {
	case "active", "skipped":
		return true
	case "pending":
		return node.LeaseExpiresAt.Valid && node.LeaseExpiresAt.Int64 > now.Unix()
	case "failed":
		return !node.RetryAfter.Valid || node.RetryAfter.Int64 > now.Unix()
	default:
		return false
	}
}
