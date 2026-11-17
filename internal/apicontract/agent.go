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
		"AgentCreated": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"agent", "key", "run_command"}, "properties": map[string]any{
			"agent": ref("Agent"), "key": text("Agent key, returned once. It can call only the agent API."), "run_command": text("Command to start the agent with this key on the server's machine."),
		}},
		"AgentEnvelope": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"agent"}, "properties": map[string]any{"agent": ref("Agent")}},
		"AgentList":     map[string]any{"type": "object", "additionalProperties": false, "required": []string{"data"}, "properties": map[string]any{"data": map[string]any{"type": "array", "items": ref("Agent")}}},
		"AgentObservedServer": map[string]any{"type": "object", "additionalProperties": false,
			"required": []string{"key", "endpoint", "target_id", "address", "engine", "model", "health", "reported_at", "ownership", "traffic_source", "traffic_detail"},
			"properties": map[string]any{
				"key": serverKey, "endpoint": name, "target_id": map[string]any{"type": "string"}, "address": text("The server's address as the agent reaches it; the control plane never dials it."),
				"engine": map[string]any{"type": "string"}, "model": map[string]any{"type": "string"}, "health": map[string]any{"type": "string"}, "reported_at": nullableTime,
				"ownership":      map[string]any{"type": "string", "enum": []string{"observe-only"}},
				"traffic_source": map[string]any{"type": "string", "enum": []string{"none", "agent-gateway"}}, "traffic_detail": map[string]any{"type": "string"},
			}},
		"AgentDetail": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"agent", "observed_servers"}, "properties": map[string]any{"agent": ref("Agent"), "observed_servers": map[string]any{"type": "array", "items": ref("AgentObservedServer")}}},
		"AgentServerRegistration": map[string]any{"type": "object", "additionalProperties": true, "required": []string{"server_key", "endpoint", "model", "engine", "address"}, "properties": map[string]any{
			"server_key": serverKey, "endpoint": name, "model": map[string]any{"type": "string", "minLength": 1, "maxLength": 256}, "engine": engine, "address": map[string]any{"type": "string", "minLength": 1, "maxLength": 256},
		}},
		"AgentRegisteredServer": map[string]any{"type": "object", "additionalProperties": true, "required": []string{"key", "endpoint", "target_id", "model", "ownership", "health", "traffic_source", "traffic_detail"}, "properties": map[string]any{
			"key": serverKey, "endpoint": name, "target_id": map[string]any{"type": "string"}, "model": map[string]any{"type": "string"}, "ownership": map[string]any{"type": "string"}, "health": map[string]any{"type": "string"},
			"traffic_source": map[string]any{"type": "string", "enum": []string{"none", "agent-gateway"}}, "traffic_detail": map[string]any{"type": "string"},
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
				"traffic": map[string]any{"type": "array", "maxItems": 1000, "description": "Content-free request shapes from the agent's local gateway.", "items": free},
				"gateway": free,
			}},
		"AgentSyncResponse": map[string]any{"type": "object", "additionalProperties": true, "required": []string{"agent_id", "agent_name", "poll_interval_seconds", "servers", "accepted", "rejected", "upgrade", "commands"}, "properties": map[string]any{
			"agent_id": map[string]any{"type": "string"}, "agent_name": map[string]any{"type": "string"},
			"poll_interval_seconds": map[string]any{"type": "integer", "minimum": 5, "maximum": 60},
			"servers":               map[string]any{"type": "array", "items": ref("AgentRegisteredServer")},
			"accepted":              map[string]any{"type": "object", "required": []string{"measurements", "requests"}, "properties": map[string]any{"measurements": map[string]any{"type": "integer"}, "requests": map[string]any{"type": "integer"}}},
			"rejected":              map[string]any{"type": "array", "items": free},
			"upgrade":               free,
			"commands":              map[string]any{"type": "array", "maxItems": 0, "description": "Reserved for candidate runs; protocol v1 sends none.", "items": free},
		}},
	}
}
