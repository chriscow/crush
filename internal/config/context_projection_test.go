package config

import (
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/stretchr/testify/require"
)

func TestContextProjectionDefaults(t *testing.T) {
	cfg := &Config{}
	cfg.setDefaults(t.TempDir(), "")

	opts := cfg.Options.ContextProjection
	require.NotNil(t, opts)
	require.False(t, opts.Enabled)
	require.Equal(t, 1_000, opts.GetMinBatchChars())
	require.Equal(t, 3, opts.GetKeepRecentBatches())
	require.Equal(t, SelectedModelTypeSmall, opts.SummarizerModel)
	require.Equal(t, 2*time.Minute, opts.SummarizerTimeout)
}

func TestContextProjectionJSONPreservesExplicitZero(t *testing.T) {
	cfg, err := loadFromBytes([][]byte{[]byte(`{
		"options": {
			"context_projection": {
				"min_batch_chars": 0,
				"keep_recent_batches": 0,
				"summarizer_model": "large",
				"summarizer_timeout": 90000000000
			}
		}
	}`)})
	require.NoError(t, err)
	cfg.setDefaults(t.TempDir(), "")

	opts := cfg.Options.ContextProjection
	require.Equal(t, 0, opts.GetMinBatchChars())
	require.Equal(t, 0, opts.GetKeepRecentBatches())
	require.Equal(t, SelectedModelTypeLarge, opts.SummarizerModel)
	require.Equal(t, 90*time.Second, opts.SummarizerTimeout)
}

func TestContextProjectionDefaultsPreserveExplicitZero(t *testing.T) {
	zero := 0
	cfg := &Config{Options: &Options{ContextProjection: &ContextProjectionOptions{
		MinBatchChars:     &zero,
		KeepRecentBatches: &zero,
	}}}
	cfg.setDefaults(t.TempDir(), "")

	opts := cfg.Options.ContextProjection
	require.Equal(t, 0, opts.GetMinBatchChars())
	require.Equal(t, 0, opts.GetKeepRecentBatches())
	require.Equal(t, SelectedModelTypeSmall, opts.SummarizerModel)
	require.Equal(t, 2*time.Minute, opts.SummarizerTimeout)
}

func TestValidateContextProjection(t *testing.T) {
	intPtr := func(value int) *int { return &value }

	tests := []struct {
		name    string
		opts    ContextProjectionOptions
		wantErr string
	}{
		{
			name: "valid",
			opts: ContextProjectionOptions{
				MinBatchChars:     intPtr(0),
				KeepRecentBatches: intPtr(0),
				SummarizerModel:   SelectedModelTypeSmall,
				SummarizerTimeout: 2 * time.Minute,
			},
		},
		{
			name: "negative minimum batch characters",
			opts: ContextProjectionOptions{
				MinBatchChars:     intPtr(-1),
				KeepRecentBatches: intPtr(3),
				SummarizerModel:   SelectedModelTypeSmall,
				SummarizerTimeout: 2 * time.Minute,
			},
			wantErr: "minimum batch characters must be non-negative",
		},
		{
			name: "negative recent batch count",
			opts: ContextProjectionOptions{
				MinBatchChars:     intPtr(1_000),
				KeepRecentBatches: intPtr(-1),
				SummarizerModel:   SelectedModelTypeSmall,
				SummarizerTimeout: 2 * time.Minute,
			},
			wantErr: "recent batch count must be non-negative",
		},
		{
			name: "invalid summarizer model",
			opts: ContextProjectionOptions{
				MinBatchChars:     intPtr(1_000),
				KeepRecentBatches: intPtr(3),
				SummarizerModel:   "medium",
				SummarizerTimeout: 2 * time.Minute,
			},
			wantErr: "summarizer model must be large or small",
		},
		{
			name: "zero timeout",
			opts: ContextProjectionOptions{
				MinBatchChars:     intPtr(1_000),
				KeepRecentBatches: intPtr(3),
				SummarizerModel:   SelectedModelTypeSmall,
			},
			wantErr: "summarizer timeout must be greater than zero and no more than 1h",
		},
		{
			name: "timeout above maximum",
			opts: ContextProjectionOptions{
				MinBatchChars:     intPtr(1_000),
				KeepRecentBatches: intPtr(3),
				SummarizerModel:   SelectedModelTypeSmall,
				SummarizerTimeout: time.Hour + time.Nanosecond,
			},
			wantErr: "summarizer timeout must be greater than zero and no more than 1h",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Options: &Options{ContextProjection: &tt.opts}}
			err := cfg.ValidateContextProjection()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestValidateContextProjectionModel(t *testing.T) {
	provider := ProviderConfig{
		ID:     "test",
		Models: []catwalk.Model{{ID: "small-model"}},
	}

	t.Run("disabled does not require a model", func(t *testing.T) {
		cfg := &Config{Options: &Options{ContextProjection: &ContextProjectionOptions{
			SummarizerModel: SelectedModelTypeSmall,
		}}}
		require.NoError(t, cfg.ValidateContextProjectionModel())
	})

	t.Run("enabled accepts a resolved model", func(t *testing.T) {
		cfg := &Config{
			Models: map[SelectedModelType]SelectedModel{
				SelectedModelTypeSmall: {Provider: "test", Model: "small-model"},
			},
			Providers: csync.NewMapFrom(map[string]ProviderConfig{"test": provider}),
			Options: &Options{ContextProjection: &ContextProjectionOptions{
				Enabled:         true,
				SummarizerModel: SelectedModelTypeSmall,
			}},
		}
		require.NoError(t, cfg.ValidateContextProjectionModel())
	})

	t.Run("enabled rejects an unresolved model", func(t *testing.T) {
		cfg := &Config{
			Models:    map[SelectedModelType]SelectedModel{},
			Providers: csync.NewMap[string, ProviderConfig](),
			Options: &Options{ContextProjection: &ContextProjectionOptions{
				Enabled:         true,
				SummarizerModel: SelectedModelTypeSmall,
			}},
		}
		require.ErrorContains(t, cfg.ValidateContextProjectionModel(), `summarizer model "small" is not configured`)
	})
}
