package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
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

func renderOperationProgress(operation domain.Operation, now time.Time) string {
	symbol := "..."
	switch operation.Status {
	case "succeeded":
		symbol = "ok "
	case "failed", "cancelled":
		symbol = "x  "
	case "waiting":
		symbol = ":: "
	case "running", "leased":
		symbol = ">  "
	}
	elapsed := "0s"
	if !operation.CreatedAt.IsZero() {
		elapsed = now.Sub(operation.CreatedAt).Round(time.Second).String()
	}
	attempt := ""
	if operation.MaxAttempts > 0 {
		attempt = fmt.Sprintf(" · attempt %d/%d", operation.Attempt, operation.MaxAttempts)
	}
	return fmt.Sprintf("%s%3d%%  %-10s  %s · %s%s", symbol, operation.Progress, strings.ToUpper(operation.Status), operation.Message, elapsed, attempt)
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
