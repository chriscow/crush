package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/config"
)

func TestGrepToolFallbackRespectsContext(t *testing.T) {
	t.Parallel()

	// Tests disable ripgrep lookup, so this exercises the regex fallback.
	require.Empty(t, getRg())

	for _, tt := range []struct {
		name    string
		wantErr error
	}{
		{name: "uncancelled"},
		{name: "cancelled", wantErr: context.Canceled},
		{name: "deadline_expired", wantErr: context.DeadlineExceeded},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "sample.txt"), []byte("needle\n"), 0o644))
			input, err := json.Marshal(GrepParams{Pattern: "needle", Path: dir, Include: "*"})
			require.NoError(t, err)

			ctx := t.Context()
			switch tt.wantErr {
			case context.Canceled:
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case context.DeadlineExceeded:
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Unix(0, 0))
				t.Cleanup(cancel)
			}
			require.ErrorIs(t, ctx.Err(), tt.wantErr)

			tool := NewGrepTool(dir, config.ToolGrep{})
			resp, err := tool.Run(ctx, fantasy.ToolCall{
				ID: "grep-context-test", Name: GrepToolName, Input: string(input),
			})
			require.NoError(t, err)
			t.Logf("Context error: %v; IsError: %t; response: %q", ctx.Err(), resp.IsError, resp.Content)

			if tt.wantErr == nil {
				require.False(t, resp.IsError)
				require.Contains(t, resp.Content, "Found 1 matches")
				require.Contains(t, resp.Content, "needle")
				return
			}

			assert.True(t, resp.IsError, "Grep must report a completed parent context as an error")
			assert.Contains(t, resp.Content, tt.wantErr.Error(), "Grep must report the cancellation cause")
			assert.NotContains(t, resp.Content, "Found 1 matches", "Grep must not return successful matches after cancellation")
		})
	}
}
