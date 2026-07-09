package util

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
)

var slugRegexp = regexp.MustCompile(`[^a-z0-9]+`)

// GenerateHumanReadableID creates a unique ID prefixed with the given prefix and a sanitized slug.
// E.g. prefix="ext", name="Computer Use" -> "ext_computer_use_1a2b3c4d1a2b3c4d"
func GenerateHumanReadableID(prefix, name string) string {
	slug := strings.ToLower(name)
	slug = slugRegexp.ReplaceAllString(slug, "_")
	slug = strings.Trim(slug, "_")

	// Limit slug length to 20 characters for clean UI
	if len(slug) > 20 {
		slug = slug[:20]
		slug = strings.TrimRight(slug, "_")
	}

	// Generate 8 bytes (16 hex chars) to ensure no collision
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	hash := hex.EncodeToString(b)

	if slug == "" {
		return prefix + "_" + hash
	}
	return prefix + "_" + slug + "_" + hash
}
