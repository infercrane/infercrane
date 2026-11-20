package apicontract

// agentSchemas describe agent management for people and the outbound-only
// agent protocol. The protocol bodies mirror internal/agentprotocol.
func agentSchemas() map[string]any {
	text := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	nullableText := map[string]any{"type": []string{"string", "null"}}
	nullableTime := map[string]any{"type": []string{"string", "null"}, "format": "date-time"}
	name := map[string]any{"type": "string", "pattern": `^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$`}
	serverKey := map[string]any{"type": "string", "pattern": `^[a-z0-9][a-z0-9._:-]{0,127}$`}
	engine := map[string]any{"type": "string", "enum": []string{"vllm", "sglang", "llama.cpp", "ollama", "lm-studio", "openai-compatible"}}
	health := map[string]any{"type": "string", "enum": []string{"healthy", "unhealthy"}}
	free := map[string]any{"type": "object", "additionalProperties": true}
	return map[string]any{
		"AgentCreate": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name"}, "properties": map[string]any{"name": name}},
		"Agent": map[string]any{"type": "object", "additionalProperties": false,
			"required": []string{"id", "name", "state", "connection", "created_by", "agent_version", "protocol", "os", "arch", "hardware", "servers", "gateway_server_key", "last_seen_at", "created_at", "updated_at", "revoked_at"},
			"properties": map[string]any{
				"id": map[string]any{"type": "string"}, "name": name,
				"state":      map[string]any{"type": "string", "enum": []string{"active", "revoked"}},
				"connection": map[string]any{"type": "string", "enum": []string{"never_connected", "connected", "silent", "revoked"}, "description": "silent means no sync for three maximum poll intervals."},
				"created_by": map[string]any{"type": "string"}, "agent_version": nullableText, "protocol": nullableText, "os": nullableText, "arch": nullableText,
				"hardware":           map[string]any{"description": "Content-free accelerator inventory the agent last reported; unknown fields are named, never guessed."},
				"servers":            map[string]any{"description": "Local servers the agent last reported, with engine, version, models, and health."},
				"gateway_server_key": nullableText, "last_seen_at": nullableTime,
				"created_at": map[string]any{"type": "string", "format": "date-time"}, "updated_at": map[string]any{"type": "string", "format": "date-time"}, "revoked_at": nullableTime,
			}},
		"AgentCreated": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"agent", "key", "run_command", "credential_cache_synchronized"}, "properties": map[string]any{
			"agent": ref("Agent"), "key": text("Agent key, returned once. It can call only the agent API."), "run_command": text("Command to start the agent with this key on the server's machine."),
			"credential_cache_synchronized": map[string]any{"type": "boolean", "description": "False when this control plane could not refresh its credential snapshot at once; the key works within a second."},
		}},
		"AgentEnvelope": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"agent"}, "properties": map[string]any{"agent": ref("Agent")}},
		"AgentList":     map[string]any{"type": "object", "additionalProperties": false, "required": []string{"data"}, "properties": map[string]any{"data": map[string]any{"type": "array", "items": ref("Agent")}}},
		"AgentObservedServer": map[string]any{"type": "object", "additionalProperties": false,
			"required": []string{"key", "endpoint", "target_id", "address", "engine", "model", "health", "reported_at", "ownership", "traffic_source", "traffic_detail"},
			"properties": map[string]any{
				"key": serverKey, "endpoint": name, "target_id": map[string]any{"type": "string"}, "address": text("The server's address as the agent reaches it; the control plane never dials it."),
				"engine": map[string]any{"type": "string"}, "model": map[string]any{"type": "string"}, "health": map[string]any{"type": "string"}, "reported_at": nullableTime,
				"ownership":      map[string]any{"type": "string", "enum": []string{"observe-only", "agent-managed"}},
				"traffic_source": map[string]any{"type": "string", "enum": []string{"none", "agent-gateway", "agent-data-plane"}}, "traffic_detail": map[string]any{"type": "string"},
			}},
		"AgentDetail": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"agent", "observed_servers"}, "properties": map[string]any{"agent": ref("Agent"), "observed_servers": map[string]any{"type": "array", "items": ref("AgentObservedServer")}}},
		"AgentServerRegistration": map[string]any{"type": "object", "additionalProperties": true, "required": []string{"server_key", "endpoint", "model", "engine", "address"}, "properties": map[string]any{
			"server_key": serverKey, "endpoint": name, "model": map[string]any{"type": "string", "minLength": 1, "maxLength": 256}, "engine": engine, "address": map[string]any{"type": "string", "minLength": 1, "maxLength": 256},
		}},
		"AgentRegisteredServer": map[string]any{"type": "object", "additionalProperties": true, "required": []string{"key", "endpoint", "target_id", "model", "ownership", "health", "traffic_source", "traffic_detail"}, "properties": map[string]any{
			"key": serverKey, "endpoint": name, "target_id": map[string]any{"type": "string"}, "model": map[string]any{"type": "string"}, "ownership": map[string]any{"type": "string"}, "health": map[string]any{"type": "string"},
			"traffic_source": map[string]any{"type": "string", "enum": []string{"none", "agent-gateway", "agent-data-plane"}}, "traffic_detail": map[string]any{"type": "string"},
		}},
		"AgentServerRegistrationResponse": map[string]any{"type": "object", "additionalProperties": true, "required": []string{"server", "created"}, "properties": map[string]any{"server": ref("AgentRegisteredServer"), "created": map[string]any{"type": "boolean"}}},
		"AgentSyncRequest": map[string]any{"type": "object", "additionalProperties": true,
			"description": "One agent sync. May be gzip-encoded; at most 8 MiB decompressed. Unknown fields are ignored within one protocol version.",
			"required":    []string{"protocol", "agent_version", "os", "arch", "sent_at", "hardware", "servers"},
			"properties": map[string]any{
				"protocol":      map[string]any{"type": "string", "const": "infercrane.agent/v1"},
				"agent_version": map[string]any{"type": "string", "maxLength": 256}, "os": map[string]any{"type": "string", "maxLength": 256}, "arch": map[string]any{"type": "string", "maxLength": 256},
				"sent_at":  map[string]any{"type": "string", "format": "date-time", "description": "Agent clock; used to correct request-shape times for clock skew."},
				"hardware": free,
				"servers": map[string]any{"type": "array", "maxItems": 32, "items": map[string]any{"type": "object", "additionalProperties": true, "required": []string{"key", "engine", "address", "models", "health", "metrics_available", "authenticated"}, "properties": map[string]any{
					"key": serverKey, "engine": engine, "engine_version": map[string]any{"type": "string"}, "address": map[string]any{"type": "string"}, "models": map[string]any{"type": "array", "maxItems": 64, "items": map[string]any{"type": "string"}},
					"active_model": map[string]any{"type": "string"}, "health": health, "health_detail": map[string]any{"type": "string"}, "metrics_available": map[string]any{"type": "boolean"}, "metrics_detail": map[string]any{"type": "string"}, "authenticated": map[string]any{"type": "boolean"},
				}}},
				"metrics": map[string]any{"type": "array", "maxItems": 32, "description": "Per server: the newest filtered engine scrape and, when available, the oldest scrape of the same engine process in the trailing five minutes.", "items": free},
				"traffic": map[string]any{"type": "array", "maxItems": 1000, "description": "Content-free request shapes from the agent's local gateway or data plane (source data-plane, with the binding it routed to).", "items": free},
				"gateway": free,
			}},
		"AgentSyncResponse": map[string]any{"type": "object", "additionalProperties": true, "required": []string{"agent_id", "agent_name", "poll_interval_seconds", "servers", "accepted", "rejected", "upgrade", "commands"}, "properties": map[string]any{
			"agent_id": map[string]any{"type": "string"}, "agent_name": map[string]any{"type": "string"},
			"poll_interval_seconds": map[string]any{"type": "integer", "minimum": 5, "maximum": 60},
			"servers":               map[string]any{"type": "array", "items": ref("AgentRegisteredServer")},
			"accepted":              map[string]any{"type": "object", "required": []string{"measurements", "requests"}, "properties": map[string]any{"measurements": map[string]any{"type": "integer"}, "requests": map[string]any{"type": "integer"}}},
			"rejected":              map[string]any{"type": "array", "items": free},
			"upgrade":               free,
			"commands":              map[string]any{"type": "array", "maxItems": 0, "description": "Always empty: commands are delivered by GET /agent/commands.", "items": free},
		}},
		"AgentDataPlaneSnapshot": map[string]any{"type": "object", "additionalProperties": true,
			"description": "What one agent needs to serve its agent-managed endpoints. No request content and no secret: API keys appear only as the SHA-256 digests the control plane stores, and only keys that may call one of these endpoints.",
			"required":    []string{"generation", "valid_until", "tenant_id"},
			"properties": map[string]any{
				"generation": text("sha256 digest of the content; send it back as ?generation= to receive unchanged: true"), "unchanged": map[string]any{"type": "boolean"},
				"valid_until": map[string]any{"type": "string", "format": "date-time", "description": "The agent refuses requests (503 data_plane_stale) if it cannot refresh before this time."},
				"tenant_id":   map[string]any{"type": "string"},
				"routes": map[string]any{"type": "array", "maxItems": 32, "items": map[string]any{"type": "object", "additionalProperties": true, "properties": map[string]any{
					"endpoint": name, "endpoint_id": map[string]any{"type": "string"}, "logical_model_id": map[string]any{"type": "string"}, "environment_id": map[string]any{"type": "string"}, "serving_plan_id": map[string]any{"type": "string"},
					"routing_policy": map[string]any{"type": "string", "enum": []string{"manual", "weighted", "primary-fallback"}},
					"bindings": map[string]any{"type": "array", "maxItems": 16, "items": map[string]any{"type": "object", "additionalProperties": true, "properties": map[string]any{
						"binding_id": map[string]any{"type": "string"}, "target_id": map[string]any{"type": "string"}, "server_key": serverKey,
						"upstream_model": map[string]any{"type": "string"}, "runtime": map[string]any{"type": "string"}, "weight": map[string]any{"type": "integer", "minimum": 0, "maximum": 100}, "capabilities": free,
					}}},
				}}},
				"credentials": map[string]any{"type": "array", "maxItems": 10000, "items": map[string]any{"type": "object", "additionalProperties": true, "properties": map[string]any{
					"hash": map[string]any{"type": "string", "pattern": "^[a-f0-9]{64}$"}, "principal_id": map[string]any{"type": "string"}, "role": map[string]any{"type": "string"},
					"scopes": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "endpoint_names": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "expires_at": nullableTime,
				}}},
				"admission":     map[string]any{"type": "array", "maxItems": 32, "items": free},
				"request_quota": map[string]any{"type": []string{"object", "null"}, "properties": map[string]any{"max_requests_per_minute": map[string]any{"type": "integer", "minimum": 0}}},
			}},
		"AgentQuotaLeaseRequest": map[string]any{"type": "object", "additionalProperties": true, "required": []string{"window", "requested"}, "properties": map[string]any{
			"window": map[string]any{"type": "string", "format": "date-time", "description": "The current UTC minute."}, "requested": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000},
		}},
		"AgentQuotaLease": map[string]any{"type": "object", "additionalProperties": true, "required": []string{"window", "granted"}, "properties": map[string]any{
			"window": map[string]any{"type": "string", "format": "date-time"}, "granted": map[string]any{"type": "integer", "minimum": 0}, "unlimited": map[string]any{"type": "boolean"},
		}},
		"AgentCommands": map[string]any{"type": "object", "additionalProperties": true, "required": []string{"commands"}, "properties": map[string]any{
			"commands": map[string]any{"type": "array", "maxItems": 4, "items": map[string]any{"type": "object", "additionalProperties": true, "required": []string{"id", "kind"}, "properties": map[string]any{
				"id": map[string]any{"type": "string"}, "kind": map[string]any{"type": "string", "enum": []string{"run_candidate"}}, "deadline_at": map[string]any{"type": "string", "format": "date-time"},
				"run_candidate": ref("CandidateRunCreate"), "control": map[string]any{"type": "object", "description": "The current setup, by the agent's own server key.", "properties": map[string]any{"endpoint": name, "server_key": serverKey, "model": map[string]any{"type": "string"}}},
				"attempt": map[string]any{"type": "integer"},
			}}},
		}},
		"AgentCommandUpdate": map[string]any{"type": "object", "additionalProperties": true, "required": []string{"state"}, "properties": map[string]any{
			"state": map[string]any{"type": "string", "enum": []string{"running", "succeeded", "failed"}}, "phase": map[string]any{"type": "string", "pattern": "^[a-z][a-z0-9_]{0,63}$"},
			"failure_code": map[string]any{"type": "string", "pattern": "^[a-z][a-z0-9_]{0,63}$"}, "failure_detail": map[string]any{"type": "string", "maxLength": 256},
			"result": ref("CandidateRunResult"),
		}},
		"AgentCommandUpdateResponse": map[string]any{"type": "object", "additionalProperties": true, "required": []string{"state", "cancelled"}, "properties": map[string]any{
			"state": map[string]any{"type": "string"}, "cancelled": map[string]any{"type": "boolean", "description": "Stop and remove the candidate."}, "lease_expires_at": map[string]any{"type": "string", "format": "date-time"},
		}},
		"CandidateRunCreate": map[string]any{"type": "object", "additionalProperties": false,
			"description": "A candidate setup to try in the agent's own cluster. The image must be pinned by digest and allowed by the agent's operator; the entry point cannot be a shell.",
			"required":    []string{"endpoint", "label", "model", "workload", "accelerator", "gpu_count", "load"},
			"properties": map[string]any{
				"endpoint": name, "label": map[string]any{"type": "string", "maxLength": 128}, "model": map[string]any{"type": "string", "maxLength": 256}, "model_revision": map[string]any{"type": "string", "maxLength": 256},
				"workload": ref("RuntimeWorkload"), "runtime_args": map[string]any{"type": "array", "maxItems": 64, "items": map[string]any{"type": "string", "maxLength": 512}},
				"accelerator": map[string]any{"type": "string", "maxLength": 256}, "gpu_count": map[string]any{"type": "integer", "minimum": 1, "maximum": 16},
				"load": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"requests", "concurrency", "input_tokens", "output_tokens"}, "properties": map[string]any{
					"requests": map[string]any{"type": "integer", "minimum": 1, "maximum": 5000}, "concurrency": map[string]any{"type": "integer", "minimum": 1, "maximum": 256},
					"input_tokens": map[string]any{"type": "integer", "minimum": 1, "maximum": 32768}, "output_tokens": map[string]any{"type": "integer", "minimum": 1, "maximum": 32768},
					"streaming": map[string]any{"type": "boolean"}, "tokenizer": map[string]any{"type": "string", "maxLength": 256},
				}},
				"quality": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
					"eval_set": map[string]any{"type": "string", "pattern": `^(default|local:[a-z0-9][a-z0-9._-]{0,62})$`, "description": "default is the built-in public suite; local:NAME is an eval set kept beside the agent, whose prompts never leave the cluster."},
					"skip":     map[string]any{"type": "boolean"},
				}},
				"timeout_seconds": map[string]any{"type": "integer", "minimum": 60, "maximum": 21600},
			}},
		"CandidateRunResult": map[string]any{"type": "object", "additionalProperties": true, "description": "Numbers only: no prompt, answer, header, log line, or address.", "properties": map[string]any{
			"control": free, "candidate": free, "control_after": free, "quality": free, "noise": map[string]any{"type": "number"},
			"ready_seconds": map[string]any{"type": "number"}, "image_digest": map[string]any{"type": "string"}, "cleaned_up": map[string]any{"type": "boolean"},
		}},
		"CandidateRun": map[string]any{"type": "object", "additionalProperties": true, "required": []string{"id", "agent", "agent_id", "state", "spec", "control", "result", "created_at"}, "properties": map[string]any{
			"id": map[string]any{"type": "string"}, "agent": name, "agent_id": map[string]any{"type": "string"},
			"state": map[string]any{"type": "string", "enum": []string{"queued", "running", "succeeded", "failed", "cancelled", "expired"}}, "phase": nullableText,
			"spec": ref("CandidateRunCreate"), "control": free, "result": map[string]any{"oneOf": []any{ref("CandidateRunResult"), map[string]any{"type": "null"}}},
			"attempts": map[string]any{"type": "integer"}, "failure_code": nullableText, "failure_detail": nullableText,
			"deadline_at": map[string]any{"type": "string", "format": "date-time"}, "created_by": map[string]any{"type": "string"},
			"created_at": map[string]any{"type": "string", "format": "date-time"}, "updated_at": map[string]any{"type": "string", "format": "date-time"}, "started_at": nullableTime, "finished_at": nullableTime,
			"runs_in": map[string]any{"type": "string"},
		}},
		"CandidateRunEnvelope": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"candidate_run"}, "properties": map[string]any{"candidate_run": ref("CandidateRun")}},
		"CandidateRunList":     map[string]any{"type": "object", "additionalProperties": false, "required": []string{"data"}, "properties": map[string]any{"data": map[string]any{"type": "array", "items": ref("CandidateRun")}}},
	}
}
