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
		attempt = fmt.Sprintf(" · check %d/%d", operation.Attempt, operation.MaxAttempts)
	}
	next := ""
	if operation.NextAttemptAt != nil && operation.NextAttemptAt.After(now) {
		next = " · next " + operation.NextAttemptAt.Sub(now).Round(time.Second).String()
	}
	return fmt.Sprintf("%s%3d%%  %-18s  %s · %s%s%s", symbol, operation.Progress, operationPhase(operation), operation.Message, elapsed, attempt, next)
}

func operationPhase(operation domain.Operation) string {
	if operation.Status == "succeeded" {
		return "READY"
	}
	if operation.Status == "failed" || operation.Status == "cancelled" {
		return strings.ToUpper(operation.Status)
	}
	message := strings.ToLower(operation.Message)
	switch {
	case strings.Contains(message, "artifact") || strings.Contains(message, "model identity"):
		return "PREPARING ARTIFACT"
	case strings.Contains(message, "runtime") || strings.Contains(message, "worker reachable"):
		return "STARTING RUNTIME"
	case strings.Contains(message, "capacity") || strings.Contains(message, "allocat") || strings.Contains(message, "provider"):
		return "WAITING FOR CAPACITY"
	case strings.Contains(message, "replica identity"):
		return "REPLICA RECORDED"
	case operation.Progress == 0:
		return "QUEUED"
	default:
		return strings.ToUpper(operation.Status)
	}
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
