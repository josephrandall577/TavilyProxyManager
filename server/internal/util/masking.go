package util

import "strings"

func MaskAPIKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return "****"
	}
	prefix := ""
	if strings.HasPrefix(key, "tvly-") {
		prefix = "tvly-"
		key = strings.TrimPrefix(key, "tvly-")
	}
	if len(key) <= 4 {
		return prefix + "****"
	}
	return prefix + "****" + key[len(key)-4:]
}
