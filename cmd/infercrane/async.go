package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"

	"github.com/infercrane/infercrane/internal/config"
)

func asyncCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: infercrane async submit ENDPOINT|async get JOB_ID|async cancel JOB_ID [flags]")
	}
	action, subject := args[0], args[1]
	fs := flag.NewFlagSet("async "+action, flag.ContinueOnError)
	file := fs.String("file", "", "protocol-native JSON request file")
	protocol := fs.String("protocol", "chat", "chat, responses, embeddings, completions, or batch")
	idempotency := fs.String("idempotency-key", "", "stable submission idempotency key")
	priority := fs.Int("priority", 0, "job priority from -100 to 100")
	deadline := fs.Int("deadline-seconds", 900, "execution deadline")
	retention := fs.Int("retention-seconds", 86400, "encrypted result retention")
	webhook := fs.String("webhook", "", "HTTPS completion webhook")
	webhookSecret := fs.String("webhook-secret-reference", "", "webhook signing secret reference")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	var response map[string]any
	switch action {
	case "submit":
		if *file == "" || *idempotency == "" {
			return errors.New("async submit requires --file and --idempotency-key")
		}
		body, err := os.ReadFile(*file)
		if err != nil {
			return fmt.Errorf("read async request: %w", err)
		}
		var input any
		if err = json.Unmarshal(body, &input); err != nil {
			return fmt.Errorf("async request must be JSON: %w", err)
		}
		request := map[string]any{"protocol": *protocol, "input": input, "idempotency_key": *idempotency, "priority": *priority, "execution_deadline_seconds": *deadline, "retention_seconds": *retention, "store_encrypted_content": true}
		if *webhook != "" || *webhookSecret != "" {
			request["webhook_url"] = *webhook
			request["webhook_secret_reference_id"] = *webhookSecret
		}
		if err = controlJSON(ctx, cfg, http.MethodPost, "/api/v1/endpoints/"+url.PathEscape(subject)+"/async", "", request, &response); err != nil {
			return err
		}
	case "get":
		if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/async/jobs/"+url.PathEscape(subject), "", nil, &response); err != nil {
			return err
		}
	case "cancel":
		if err := controlJSON(ctx, cfg, http.MethodDelete, "/api/v1/async/jobs/"+url.PathEscape(subject), "", nil, nil); err != nil {
			return err
		}
		response = map[string]any{"id": subject, "status": "cancelled"}
	default:
		return errors.New("usage: infercrane async submit ENDPOINT|async get JOB_ID|async cancel JOB_ID [flags]")
	}
	if *output == "json" {
		return printJSON(response)
	}
	if action == "submit" {
		if job, ok := response["job"].(map[string]any); ok {
			fmt.Printf("async job %v queued · resume with infercrane async get %v\n", job["id"], job["id"])
			return nil
		}
	}
	fmt.Printf("async %s %s completed\n", action, subject)
	return nil
}
