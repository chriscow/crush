package common

import (
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestFormatTokensAndCostPrefixesEstimatedUsage(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()

	rendered := formatTokensAndCost(&sty, 120, 1000, 0, true)
	actual := ansi.Strip(rendered)

	require.Contains(t, actual, "~12%")
	require.Contains(t, actual, "(120)")
	require.Contains(t, actual, "$0.00")
	require.True(t, strings.Contains(rendered, sty.ModelInfo.TokenPercentage.Render("~12%")))
}

func TestFormatTokensAndCostOmitsEstimatedPrefix(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()

	actual := ansi.Strip(formatTokensAndCost(&sty, 120, 1000, 0, false))

	require.Contains(t, actual, "12%")
	require.NotContains(t, actual, "~12%")
}

func TestFormatTokenCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		tokens int64
		want   string
	}{
		{tokens: 0, want: "0"},
		{tokens: 999, want: "999"},
		{tokens: 1_000, want: "1K"},
		{tokens: 12_400, want: "12.4K"},
		{tokens: 1_000_000, want: "1M"},
		{tokens: 2_500_000, want: "2.5M"},
	}
	for _, tt := range tests {
		require.Equal(t, tt.want, FormatTokenCount(tt.tokens), "FormatTokenCount(%d)", tt.tokens)
	}
}
