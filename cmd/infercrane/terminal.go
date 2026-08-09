package main

import (
	"os"
	"strings"
)

func terminalColor(code, value string) string {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return value
	}
	info, err := os.Stdout.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return value
	}
	return "\x1b[" + code + "m" + value + "\x1b[0m"
}

func terminalStatus(value string) string {
	switch strings.ToLower(value) {
	case "healthy", "ready", "active", "succeeded", "pass":
		return terminalColor("32", strings.ToUpper(value))
	case "pending", "starting", "running", "waiting":
		return terminalColor("34", strings.ToUpper(value))
	case "degraded", "warning", "draining", "retrying":
		return terminalColor("33", strings.ToUpper(value))
	case "failed", "unhealthy", "error", "cancelled":
		return terminalColor("31", strings.ToUpper(value))
	default:
		return strings.ToUpper(value)
	}
}
