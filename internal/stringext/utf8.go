package stringext

import "unicode/utf8"

// RunePrefix returns the prefix containing at most limit UTF-8 code points and
// the number of code points in that prefix. It slices the original string, so
// invalid UTF-8 bytes are preserved rather than replaced.
func RunePrefix(value string, limit int) (prefix string, count int) {
	if value == "" || limit <= 0 {
		return "", 0
	}

	end := 0
	for end < len(value) && count < limit {
		_, size := utf8.DecodeRuneInString(value[end:])
		end += size
		count++
	}
	return value[:end], count
}

// RunePage returns a code-point page and its clamped offset, returned count,
// and total count. It walks UTF-8 boundaries without allocating a full rune
// slice and preserves the original bytes in the returned page.
func RunePage(value string, offset, limit int) (page string, actualOffset, returned, total int) {
	if offset < 0 {
		offset = 0
	}
	if limit < 0 {
		limit = 0
	}

	startByte := -1
	endByte := -1
	for byteOffset := 0; byteOffset < len(value); {
		if total == offset {
			startByte = byteOffset
			endByte = byteOffset
		}
		_, size := utf8.DecodeRuneInString(value[byteOffset:])
		byteOffset += size
		if startByte >= 0 && returned < limit {
			endByte = byteOffset
			returned++
		}
		total++
	}

	actualOffset = min(offset, total)
	if startByte < 0 {
		startByte = len(value)
		endByte = len(value)
	}
	return value[startByte:endByte], actualOffset, returned, total
}
