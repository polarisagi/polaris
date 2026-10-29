package util

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

var slugRegexp = regexp.MustCompile(`[^a-z0-9]+`)

// GenerateHumanReadableID creates a unique ID prefixed with the given prefix and a sanitized slug.
// E.g. prefix="ext", name="Computer Use" -> "ext_computer_use_1a2b3c4d1a2b3c4d"
func GenerateHumanReadableID(prefix, name string) string {
	// Generate 8 bytes (16 hex chars) to ensure no collision
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return joinID(prefix, name, hex.EncodeToString(b))
}

// DeterministicID 与 GenerateHumanReadableID 同形，但后缀由 key 的 sha256 派生：同一 key 恒得同一 ID。
// 用于需要「重装后身份不变」的对象（ADR-0124），唯一性由 key 承担，name 只为可读。
func DeterministicID(prefix, name, key string) string {
	sum := sha256.Sum256([]byte(key))
	return joinID(prefix, name, hex.EncodeToString(sum[:6]))
}

func joinID(prefix, name, suffix string) string {
	slug := strings.ToLower(name)
	slug = slugRegexp.ReplaceAllString(slug, "_")
	slug = strings.Trim(slug, "_")

	// Limit slug length to 20 characters for clean UI
	if len(slug) > 20 {
		slug = slug[:20]
		slug = strings.TrimRight(slug, "_")
	}

	if slug == "" {
		return prefix + "_" + suffix
	}
	return prefix + "_" + slug + "_" + suffix
}
