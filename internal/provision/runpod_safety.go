package provision

import (
	"strings"
	"unicode/utf8"
)

const runPodDiagnosticLimit = 4096

func safeRunPodDiagnostic(message, apiKey string) string {
	message = strings.TrimSpace(message)
	if apiKey != "" {
		message = strings.ReplaceAll(message, apiKey, "[REDACTED]")
	}
	if len(message) > runPodDiagnosticLimit {
		message = message[:runPodDiagnosticLimit]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
		message += "…"
	}
	return message
}
