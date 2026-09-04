// Package condense projects canonical historical tool batches into compact,
// recoverable provider-facing message copies.
package condense

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/charmbracelet/crush/internal/message"
)

const (
	// AlgorithmVersion identifies the scanner, hash, summary, and rendering
	// contract implemented by this package.
	AlgorithmVersion int64 = 1

	defaultLeaseMargin  = 30 * time.Second
	defaultQueryLimit   = 12_000
	maxQueryLimit       = 24_000
	maxSummaryChars     = 4_000
	maxDescriptionChars = 500
)

// Options controls projection behavior. Query remains available when Enabled
// is false so durable historical references remain recoverable.
type Options struct {
	Enabled           bool
	MinBatchChars     int
	KeepRecentBatches int
	SummarizerTimeout time.Duration
	ConfigurationID   string
}

func (o Options) normalized() Options {
	if o.MinBatchChars < 0 {
		o.MinBatchChars = 0
	}
	if o.KeepRecentBatches < 0 {
		o.KeepRecentBatches = 0
	}
	if o.SummarizerTimeout <= 0 {
		o.SummarizerTimeout = 2 * time.Minute
	}
	if o.ConfigurationID == "" {
		o.ConfigurationID = ConfigurationID(o, "")
	}
	return o
}

// ConfigurationID returns a deterministic, non-secret identity for the policy
// and summarizer configuration that controls durable suppression decisions.
// Active rows do not depend on this value for rendering.
func ConfigurationID(options Options, summarizerIdentity string) string {
	hash := sha256.New()
	for _, value := range []string{
		"context-projection-policy-v1",
		strconv.Itoa(options.MinBatchChars),
		strconv.Itoa(options.KeepRecentBatches),
		strconv.FormatInt(int64(options.SummarizerTimeout), 10),
		summarizerIdentity,
	} {
		_, _ = hash.Write([]byte(strconv.Itoa(len(value))))
		_, _ = hash.Write([]byte{':'})
		_, _ = hash.Write([]byte(value))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// Summarizer generates a bounded description of a canonical candidate.
type Summarizer interface {
	Summarize(context.Context, Candidate) (Summary, error)
}

// Usage records billable summarizer work independently of summary validity.
type Usage struct {
	Provider         string
	Model            string
	PromptTokens     int64
	CompletionTokens int64
	Cost             float64
}

// SummaryFailureClass tells the provider-neutral projection core whether a
// summarizer failure can be retried for the same source and configuration.
type SummaryFailureClass string

const (
	SummaryFailureRetryable SummaryFailureClass = "retryable"
	SummaryFailureTerminal  SummaryFailureClass = "terminal"
)

// SummaryError classifies a summarizer failure without coupling the core to a
// provider's error types. Err is used only for control flow and is never
// persisted by the projection core.
type SummaryError struct {
	Class SummaryFailureClass
	Usage Usage
	Err   error
}

func (e *SummaryError) Error() string {
	if e == nil || e.Err == nil {
		return "summary failed"
	}
	return e.Err.Error()
}

func (e *SummaryError) Unwrap() error { return e.Err }

// RetryableSummaryError marks a transient summarizer failure.
func RetryableSummaryError(err error) error {
	return RetryableSummaryErrorWithUsage(err, Usage{})
}

// RetryableSummaryErrorWithUsage preserves billable usage from an invalid
// provider response while marking the summary attempt retryable.
func RetryableSummaryErrorWithUsage(err error, usage Usage) error {
	return &SummaryError{Class: SummaryFailureRetryable, Usage: usage, Err: err}
}

// TerminalSummaryError marks a configuration or capability failure that must
// not be retried for the same durable source/configuration identity.
func TerminalSummaryError(err error) error {
	return TerminalSummaryErrorWithUsage(err, Usage{})
}

// TerminalSummaryErrorWithUsage preserves billable usage from an invalid
// provider response while marking the summary attempt terminal.
func TerminalSummaryErrorWithUsage(err error, usage Usage) error {
	return &SummaryError{Class: SummaryFailureTerminal, Usage: usage, Err: err}
}

// Summary is structured summarizer output plus optional usage metadata.
type Summary struct {
	Text             string
	Items            []SummaryItem
	Provider         string
	Model            string
	PromptTokens     int64
	CompletionTokens int64
	Cost             float64
	Truncated        bool
}

// SummaryItem describes one candidate result by its zero-based item ordinal.
type SummaryItem struct {
	Ordinal     int
	Description string
}

// Query requests a Unicode code-point page of an original tool result.
type Query struct {
	Ref    string
	Offset int
	Limit  *int
}

// SessionStats summarizes projection savings for one session's active
// batches. Batches counts active nodes, RawChars is the canonical characters
// they replaced, and ProjectedChars is what the provider-facing copies use.
type SessionStats struct {
	Batches        int64
	RawChars       int64
	ProjectedChars int64
}

// Result is an exact page from canonical persisted tool-result content.
type Result struct {
	Status     string
	Ref        string
	ToolName   string
	ToolCallID string
	IsError    bool
	Offset     int
	Returned   int
	Total      int
	NextOffset *int
	Complete   bool
	Content    string
}

// ErrorClass identifies failures without exposing source or summary content.
type ErrorClass string

const (
	ErrorClassStore       ErrorClass = "store"
	ErrorClassSummarizer  ErrorClass = "summarizer"
	ErrorClassValidation  ErrorClass = "validation"
	ErrorClassStale       ErrorClass = "stale"
	ErrorClassClaim       ErrorClass = "claim"
	ErrorClassUnsupported ErrorClass = "unsupported"
)

// ProjectionError describes a projection failure and whether callers may
// safely continue with canonical raw messages.
type ProjectionError struct {
	Class       ErrorClass
	NodeID      string
	BatchKey    string
	Recoverable bool
	Err         error
}

func (e *ProjectionError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		return fmt.Sprintf("context projection %s failure", e.Class)
	}
	return fmt.Sprintf("context projection %s failure: %v", e.Class, e.Err)
}

func (e *ProjectionError) Unwrap() error { return e.Err }

// IsRecoverable reports whether err permits a raw-message fallback.
func IsRecoverable(err error) bool {
	var projectionErr *ProjectionError
	return errors.As(err, &projectionErr) && projectionErr.Recoverable
}

// IsFatal reports whether err must abort the caller rather than falling back.
func IsFatal(err error) bool {
	var projectionErr *ProjectionError
	return errors.As(err, &projectionErr) && !projectionErr.Recoverable
}

// Candidate is one conservatively validated assistant-call/result batch.
type Candidate struct {
	SessionID      string
	Assistant      message.Message
	ToolMessages   []message.Message
	Sources        []Source
	Items          []Item
	FirstMessageID string
	LastMessageID  string
	BatchKey       string
	SourceHash     string
	RawChars       int
}

// Source identifies every canonical message part required to validate a node.
type Source struct {
	MessageID   string
	PartOrdinal int
	PartKind    string
	SourceHash  string
}

// Item identifies one canonical tool result occurrence.
type Item struct {
	MessageID   string
	PartOrdinal int
	ToolCallID  string
	ToolName    string
	Result      message.ToolResult
	SourceHash  string
}

func recoverable(class ErrorClass, err error) error {
	return &ProjectionError{Class: class, Recoverable: true, Err: err}
}

func fatal(class ErrorClass, err error) error {
	return &ProjectionError{Class: class, Recoverable: false, Err: err}
}

func projectError(ctx context.Context, class ErrorClass, err error) error {
	if cause := context.Cause(ctx); cause != nil {
		return fatal(class, cause)
	}
	if IsRecoverable(err) || IsFatal(err) {
		return err
	}
	return recoverable(class, err)
}
