package coreutils

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"
)

// Basename strips the directory and an optional suffix from a slash separated
// path.
func Basename(input, suffix string) string {
	trimmed := strings.TrimRight(input, "/")
	if trimmed == "" {
		if strings.HasPrefix(input, "/") {
			return "/"
		}
		return "."
	}
	name := path.Base(trimmed)
	if suffix != "" && suffix != name {
		name = strings.TrimSuffix(name, suffix)
	}
	return name
}

// Dirname strips the last component from a slash separated path.
func Dirname(input string) string {
	return path.Dir(input)
}

// Sha256Sum returns the hexadecimal SHA-256 digest of data.
func Sha256Sum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
