package model

import (
	"testing"

	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/crush/internal/workspace"
	"github.com/stretchr/testify/require"
)

func TestProjectionInfo(t *testing.T) {
	t.Run("shows zero when nothing is projected", func(t *testing.T) {
		t.Parallel()

		ui := &UI{com: &common.Common{Styles: &styles.Styles{}}}
		plain := stripANSI(ui.projectionInfo(40))
		require.Contains(t, plain, "tool output summarized")
		require.Contains(t, plain, "0%")
		require.NotContains(t, plain, "→")
	})

	t.Run("renders compression and size delta", func(t *testing.T) {
		t.Parallel()

		ui := &UI{com: &common.Common{Styles: &styles.Styles{}}}
		ui.projectionStats = workspace.AgentProjectionStats{
			Batches:        2,
			RawChars:       10_000,
			ProjectedChars: 6_200,
		}
		plain := stripANSI(ui.projectionInfo(40))
		require.Contains(t, plain, "tool output summarized")
		require.Contains(t, plain, "-38%")
		require.Contains(t, plain, "10K")
		require.Contains(t, plain, "6.2K")
	})

	t.Run("clamps negative compression to zero", func(t *testing.T) {
		t.Parallel()

		ui := &UI{com: &common.Common{Styles: &styles.Styles{}}}
		ui.projectionStats = workspace.AgentProjectionStats{
			Batches:        1,
			RawChars:       100,
			ProjectedChars: 200,
		}
		require.Contains(t, stripANSI(ui.projectionInfo(40)), "-0%")
	})

	t.Run("truncates to the sidebar width", func(t *testing.T) {
		t.Parallel()

		ui := &UI{com: &common.Common{Styles: &styles.Styles{}}}
		ui.projectionStats = workspace.AgentProjectionStats{
			Batches:        1,
			RawChars:       12_400,
			ProjectedChars: 7_700,
		}
		require.LessOrEqual(t, len([]rune(ui.projectionInfo(8))), 8)
	})
}
