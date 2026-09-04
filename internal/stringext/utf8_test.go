package stringext

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRunePrefixPreservesUTF8BoundariesAndBytes(t *testing.T) {
	t.Parallel()

	value := "a¢界🌍\xfftail"
	tests := []struct {
		limit  int
		prefix string
		count  int
	}{
		{limit: -1, prefix: "", count: 0},
		{limit: 0, prefix: "", count: 0},
		{limit: 1, prefix: "a", count: 1},
		{limit: 4, prefix: "a¢界🌍", count: 4},
		{limit: 5, prefix: "a¢界🌍\xff", count: 5},
		{limit: 100, prefix: value, count: 9},
	}
	for _, test := range tests {
		prefix, count := RunePrefix(value, test.limit)
		require.Equal(t, test.prefix, prefix)
		require.Equal(t, test.count, count)
	}
}

func TestRunePagePreservesUTF8BoundariesAndBytes(t *testing.T) {
	t.Parallel()

	value := "a¢界🌍\xffz"
	tests := []struct {
		offset       int
		limit        int
		page         string
		actualOffset int
		returned     int
		total        int
	}{
		{offset: 0, limit: 2, page: "a¢", actualOffset: 0, returned: 2, total: 6},
		{offset: 2, limit: 3, page: "界🌍\xff", actualOffset: 2, returned: 3, total: 6},
		{offset: 5, limit: 10, page: "z", actualOffset: 5, returned: 1, total: 6},
		{offset: 6, limit: 1, page: "", actualOffset: 6, returned: 0, total: 6},
		{offset: 99, limit: 1, page: "", actualOffset: 6, returned: 0, total: 6},
		{offset: 2, limit: 0, page: "", actualOffset: 2, returned: 0, total: 6},
	}
	for _, test := range tests {
		page, actualOffset, returned, total := RunePage(value, test.offset, test.limit)
		require.Equal(t, test.page, page)
		require.Equal(t, test.actualOffset, actualOffset)
		require.Equal(t, test.returned, returned)
		require.Equal(t, test.total, total)
	}
}
