package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

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

const operationProgressHeartbeat = 30 * time.Second

func shouldRenderOperationProgress(previous *domain.Operation, current domain.Operation, lastPrinted, now time.Time) bool {
	if previous == nil {
		return true
	}
	if current.Status == "succeeded" || current.Status == "failed" || current.Status == "cancelled" {
		return true
	}
	message := strings.TrimSpace(strings.ToLower(current.Message))
	if (current.Status == "running" || current.Status == "leased") && (message == "" || message == "running") {
		return false
	}
	if operationProgressSignature(*previous) != operationProgressSignature(current) {
		return true
	}
	return !lastPrinted.IsZero() && now.Sub(lastPrinted) >= operationProgressHeartbeat
}

func operationProgressSignature(operation domain.Operation) string {
	return fmt.Sprintf("%s:%d:%s", operationPhase(operation), operation.Progress, operation.Message)
}

func operationPhase(operation domain.Operation) string {
	if operation.Status == "succeeded" {
		if isDeleteOperation(operation) {
			return "DELETED"
		}
		return "READY"
	}
	if operation.Status == "failed" || operation.Status == "cancelled" {
		return strings.ToUpper(operation.Status)
	}
	if phase := operationStepPhase(operation.CurrentStep); phase != "" {
		return phase
	}
	message := strings.ToLower(operation.Message)
	switch {
	case isDeleteOperation(operation):
		return "DELETING"
	case strings.Contains(message, "runtime") || strings.Contains(message, "worker reachable") || strings.Contains(message, "provider endpoint is assigned") || strings.Contains(message, "restarting"):
		return "STARTING RUNTIME"
	case strings.Contains(message, "artifact") || strings.Contains(message, "model identity"):
		return "PREPARING ARTIFACT"
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

func operationStepPhase(step string) string {
	step = strings.ToLower(strings.TrimSpace(step))
	switch {
	case step == "":
		return ""
	case strings.Contains(step, "delete") || strings.Contains(step, "drain"):
		return "DELETING"
	case step == "model.artifact":
		return "PREPARING ARTIFACT"
	case strings.HasSuffix(step, ".intent"):
		return "REPLICA RECORDED"
	case strings.HasSuffix(step, ".availability"):
		return "CHECKING CAPACITY"
	case strings.HasSuffix(step, ".ensure") || strings.HasSuffix(step, ".ready"):
		return "WAITING FOR CAPACITY"
	case strings.HasSuffix(step, ".runtime"):
		return "STARTING RUNTIME"
	case strings.Contains(step, "route") || step == "endpoint.publish":
		return "PUBLISHING ROUTE"
	case step == "serverless.endpoint":
		return "STARTING SERVERLESS"
	case strings.Contains(step, "observe"):
		return "OBSERVING"
	case strings.Contains(step, "candidate"):
		return "PREPARING CANDIDATE"
	default:
		return ""
	}
}

func isDeleteOperation(operation domain.Operation) bool {
	return strings.HasSuffix(strings.ToLower(operation.Kind), ".delete")
}
