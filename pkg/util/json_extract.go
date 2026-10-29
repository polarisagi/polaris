package util

// ExtractJSONBraces extracts the first JSON object bounded by '{' and '}' using bracket counting.
// It respects string literals to avoid premature termination.
// If no valid JSON brace structure is found, it returns the original string.
func ExtractJSONBraces(s string) string {
	startIdx := -1
	braceCount := 0
	inString := false
	var prevChar rune

	for i, c := range s {
		if c == '"' && prevChar != '\\' {
			inString = !inString
		}

		//nolint:nestif
		if !inString {
			//nolint:staticcheck
			if c == '{' {
				if startIdx == -1 {
					startIdx = i
				}
				braceCount++
			} else if c == '}' {
				if startIdx != -1 {
					braceCount--
					if braceCount == 0 {
						return s[startIdx : i+1]
					}
				}
			}
		}
		prevChar = c
	}
	return s
}
