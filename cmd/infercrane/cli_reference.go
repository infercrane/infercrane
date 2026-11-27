package main

// commandActions lists what a command can do, in the order help shows it.
// Each entry is "ACTION ARGUMENTS" and a short description. Help renders
// them under an Actions heading so a command that takes an ACTION says which.
var commandActions = map[string][][2]string{
	"models": {
		{"[QUERY]", "browse supported models"},
		{"inspect NAME", "show one model's details"},
		{"check OWNER/NAME", "check any Hugging Face model and what it needs"},
		{"adapter-check OWNER/NAME", "check a LoRA adapter against its base model"},
		{"import s3://BUCKET/PREFIX --from DIR", "upload your own weights or adapter"},
	},
	"rollout": {
		{"inspect DEPLOYMENT", "show the active and staged revisions"},
		{"create DEPLOYMENT --file SPEC", "stage a new revision from a spec file"},
		{"provision DEPLOYMENT REVISION", "start servers for a staged revision"},
		{"validate DEPLOYMENT REVISION", "send test traffic to a staged revision"},
		{"evaluate DEPLOYMENT REVISION", "compare a staged revision with the active one"},
		{"promote DEPLOYMENT REVISION", "roll out a staged revision"},
		{"reject DEPLOYMENT REVISION", "discard a staged revision"},
		{"rollback DEPLOYMENT REVISION", "roll back to an earlier revision"},
		{"policy set DEPLOYMENT", "set the checks a revision must pass"},
	},
	"operation": {
		{"list", "list recent operations, newest first"},
		{"[inspect] ID", "show one operation"},
		{"watch ID", "follow an operation until it finishes"},
		{"cancel ID", "stop an operation"},
	},
	"explain": {
		{"DEPLOYMENT", "say why a deployment is in its current state"},
		{"scaling DEPLOYMENT", "explain the last scaling decision"},
		{"rollout DEPLOYMENT", "explain the last rollout decision"},
		{"cold-start DEPLOYMENT", "explain how long startup takes and why"},
	},
	"finops": {
		{"DEPLOYMENT", "show what a deployment costs"},
		{"collect opencost DEPLOYMENT", "import cost from OpenCost"},
	},
	"request": {
		{"ENDPOINT", "send a test request"},
		{"inspect REQUEST_ID", "show one recorded request"},
	},
	"context": {
		{"list", "list saved contexts"},
		{"show", "show the context in use"},
		{"use NAME", "switch to another saved context"},
	},
	"target": {
		{"add NAME --url URL", "register a GPU server you already run"},
		{"list", "list registered servers"},
	},
	"provider": {
		{"connect NAME --model MODEL", "connect a hosted model API"},
		{"list", "list connected APIs"},
		{"delete NAME", "remove a connected API"},
	},
	"secret": {
		{"create NAME", "save a secret: sealed, environment, Kubernetes, or AWS"},
		{"list", "list secrets"},
		{"delete ID --yes", "delete a secret"},
	},
	"principal": {
		{"create NAME --role ROLE", "create an API key"},
		{"rotate ID", "replace a key with a new one"},
		{"revoke ID", "revoke a key"},
	},
	"tenant": {
		{"create ID", "create an isolated tenant"},
	},
	"system": {
		{"instances", "list the running control-plane instances"},
	},
	"integrations": {
		{"[list]", "list integrations and what each supports"},
	},
	"discover": {
		{"local", "list the GPUs on this machine"},
	},
	"telemetry": {
		{"collect dcgm DEPLOYMENT", "read NVIDIA GPU metrics from dcgm-exporter"},
		{"collect amd DEPLOYMENT", "read AMD GPU metrics from AMD's exporter"},
	},
	"workload": {
		{"init [PATH]", "start a project for a model"},
		{"validate [PATH]", "check the project"},
		{"build [PATH]", "build its image"},
		{"dev [PATH]", "run it on this machine"},
		{"plan [PATH]", "preview the deployment"},
		{"deploy [PATH]", "deploy it"},
	},
	"adopt": {
		{"endpoint NAME --url URL --model MODEL", "watch an endpoint you already run"},
		{"promote NAME --ownership traffic-managed", "start routing its traffic"},
	},
	"endpoint": {
		{"list", "list endpoints"},
		{"create NAME --model MODEL", "create an endpoint"},
		{"inspect NAME", "show an endpoint and its serving plan"},
		{"bind ENDPOINT --name NAME", "attach a deployment, server, or API"},
		{"plan ENDPOINT --policy POLICY", "stage a new serving plan"},
		{"stage ENDPOINT PLAN_ID", "stage an existing plan"},
		{"promote ENDPOINT PLAN_ID", "roll out a staged plan"},
		{"guard ENDPOINT", "check a staged plan before rollout"},
		{"delete ENDPOINT --yes", "delete an endpoint"},
	},
	"environment": {
		{"create NAME", "create an environment"},
		{"promote SOURCE --to DESTINATION", "copy a serving plan to another environment"},
	},
	"logical-model": {
		{"create NAME", "create a stable model name"},
	},
	"admission": {
		{"get ENDPOINT", "show request limits"},
		{"set ENDPOINT", "set concurrency, queue, and size limits"},
	},
	"slo": {
		{"get DEPLOYMENT", "show latency, error, and cost targets"},
		{"set DEPLOYMENT", "set targets"},
		{"delete DEPLOYMENT", "remove targets"},
	},
	"alert": {
		{"list ENDPOINT", "list alert webhooks"},
		{"configure ENDPOINT --webhook URL", "send signed alerts to a webhook"},
		{"evaluate ENDPOINT", "check alerts now"},
	},
	"external": {
		{"configure DEPLOYMENT", "set when traffic may fall back to an external API"},
		{"inspect DEPLOYMENT", "show the fallback policy"},
	},
	"async": {
		{"submit ENDPOINT --file REQUEST", "submit a long request"},
		{"get JOB_ID", "fetch its result"},
		{"cancel JOB_ID", "cancel it"},
	},
	"sandbox": {
		{"connect", "give a sandbox short-lived access to one endpoint"},
		{"list", "list sandbox credentials"},
		{"rotate REFERENCE_ID", "replace a credential"},
		{"revoke REFERENCE_ID --yes", "revoke a credential"},
	},
	"evaluation": {
		{"keygen", "create a signing key"},
		{"sign DEPLOYMENT REVISION", "sign an answer-quality result"},
		{"verify FILE", "verify a signed result"},
		{"ingest DEPLOYMENT REVISION", "sign a result from your own evaluator"},
		{"attach DEPLOYMENT --file FILE", "attach a signed result to a deployment"},
		{"list DEPLOYMENT", "list attached results"},
		{"set upload FILE", "upload a question set for answer checks"},
	},
	"passport": {
		{"keygen", "create a signing key"},
		{"issue DEPLOYMENT", "issue a signed release record"},
		{"list DEPLOYMENT", "list issued records"},
		{"verify FILE", "verify a record"},
	},
	"report": {
		{"proof RUN [RUN...]", "the signed before-and-after Proof Report of finished runs"},
		{"proof verify FILE", "verify a saved Proof Report"},
	},
	"results": {
		{"import FILE...", "keep measured results as records"},
		{"list", "list kept records"},
		{"show RECORD_ID", "show one record"},
		{"export", "write kept records as JSON"},
		{"rank --query FILE", "rank setups from similar measured results"},
	},
	"training": {
		{"keygen", "create a signing key"},
		{"sign DEPLOYMENT REVISION", "sign a checkpoint record"},
		{"verify FILE", "verify a checkpoint record"},
		{"attach DEPLOYMENT FILE", "attach a checkpoint to a deployment"},
		{"list DEPLOYMENT", "list attached checkpoints"},
	},
	"artifact": {
		{"inspect ARTIFACT_ID", "show a model cache"},
		{"observe ARTIFACT_ID", "record where a cache is warm"},
		{"prefetch ARTIFACT_ID", "warm a cache ahead of a deployment"},
		{"optimize plan BASE_ARTIFACT --preset PRESET", "plan a quantized or speculative build"},
		{"optimize list", "list optimized builds"},
	},
	"model-api": {
		{"publish TYPE --file MANIFEST", "publish a Model API manifest"},
	},
}

// moreExamples covers commands whose help had no example. Model names follow
// the catalog: popular open models first.
var moreExamples = map[string]string{
	"request": `  # Send one chat message and print the answer
  infercrane request qwen-chat --message "Summarize this in one line"

  # Stream the answer as it arrives
  infercrane request qwen-chat --stream`,
	"doctor": `  # Check the CLI, sign-in, and control plane
  infercrane doctor

  # Diagnose one endpoint over the last hour
  infercrane doctor qwen-chat --window 1h`,
	"deployments": `  infercrane deployments
  infercrane ls --output json`,
	"endpoints": `  infercrane endpoints`,
	"status": `  # Health and traffic now
  infercrane status qwen-chat

  # Keep refreshing
  infercrane status qwen-chat --watch`,
	"delete": `  # See what would be removed
  infercrane delete qwen-chat --plan

  # Delete it and wait until the servers are gone
  infercrane delete qwen-chat --yes --wait`,
	"benchmark": `  # 100 requests at 10 concurrent against the active revision
  infercrane benchmark qwen-chat

  # A heavier run with a time-to-first-token target
  infercrane benchmark qwen-chat --requests 500 --concurrency 32 --ttft-slo-ms 300`,
	"inbox": `  infercrane inbox`,
	"observe": `  # Health, traffic, and recent events for an endpoint
  infercrane observe qwen-chat

  # Run a fresh diagnosis, then keep refreshing
  infercrane observe qwen-chat --diagnose --watch`,
	"explain": `  infercrane explain qwen-chat
  infercrane explain scaling qwen-chat`,
	"finops": `  # Cost over the last 30 days
  infercrane finops qwen-chat --window 720h`,
	"ui":     `  infercrane ui`,
	"whoami": `  infercrane whoami`,
	"context": `  infercrane context list
  infercrane context use staging`,
	"target": `  # Register a vLLM server you already run
  infercrane target add gpu-a --url http://10.0.0.12:8000
  infercrane target list`,
	"provider": `  # Connect OpenRouter as a fallback API
  infercrane provider connect openrouter-qwen --model qwen/qwen3-8b --from-env OPENROUTER_API_KEY
  infercrane provider list`,
	"secret": `  # Save a Hugging Face token, sealed in the control plane
  infercrane secret create hf-token --sealed-from-env HF_TOKEN
  infercrane secret list`,
	"principal": `  # A key for CI that can deploy
  infercrane principal create ci-deployer --role operator
  infercrane principal rotate PRINCIPAL_ID`,
	"logout": `  infercrane logout`,
	"version": `  infercrane version
  infercrane version --output json`,
	"apply": `  # Make the deployment match a spec file you keep in git
  infercrane apply infercrane.yaml --wait`,
	"connect": `  # Put a vLLM server you already run behind InferCrane
  infercrane connect http://10.0.0.12:8000 --as qwen-chat`,
	"adopt": `  # Watch an endpoint first, then let InferCrane route its traffic
  infercrane adopt endpoint qwen-chat --url http://10.0.0.12:8000 --model qwen-chat
  infercrane adopt promote qwen-chat --ownership traffic-managed`,
	"discover": `  infercrane discover local`,
	"workload": `  # Start a project for a model, check it, and deploy it
  infercrane workload init --model Qwen/Qwen3-8B
  infercrane workload validate
  infercrane workload deploy --wait`,
	"endpoint": `  # One stable endpoint in front of a deployment
  infercrane endpoint create qwen-chat --model qwen-chat
  infercrane endpoint bind qwen-chat --name primary --deployment qwen-prod
  infercrane endpoint inspect qwen-chat`,
	"environment": `  # Copy staging's serving plan to production
  infercrane environment promote qwen-chat-staging --to qwen-chat --yes`,
	"logical-model": `  infercrane logical-model create qwen-chat --description "Main chat model"`,
	"route":         `  infercrane route qwen-prod --strategy cache-aware`,
	"admission": `  # At most 64 requests at once, with a queue of 256
  infercrane admission set qwen-chat --max-concurrency 64 --max-queue 256
  infercrane admission get qwen-chat`,
	"slo": `  # p95 time to first token under 300 ms and errors under 1%
  infercrane slo set qwen-prod --ttft-p95 300 --error-rate 0.01
  infercrane slo get qwen-prod`,
	"alert": `  infercrane alert configure qwen-chat --webhook https://hooks.example.com/infercrane --secret-reference SECRET_ID
  infercrane alert list qwen-chat`,
	"external": `  # Fall back to OpenRouter only when your servers are unhealthy
  infercrane external configure qwen-prod --target openrouter-qwen --enable --cost-limit-usd 50 --acknowledge-external-data
  infercrane external inspect qwen-prod`,
	"async": `  infercrane async submit qwen-chat --file request.json --webhook https://hooks.example.com/done
  infercrane async get JOB_ID`,
	"sandbox": `  infercrane sandbox connect --provider e2b --external-id SANDBOX_ID --endpoint qwen-chat --ttl 30m
  infercrane sandbox list`,
	"inspect": `  infercrane inspect qwen-prod --output json`,
	"evaluation": `  infercrane evaluation keygen --file evaluator.key
  infercrane evaluation list qwen-prod`,
	"passport": `  infercrane passport issue qwen-prod --file qwen-prod.passport.json
  infercrane passport verify qwen-prod.passport.json`,
	"report": `  # One workload at two loads, printable, and saved to verify later
  infercrane report proof RUN_A RUN_B --prepared-for "Example Labs" --html proof.html
  infercrane report proof RUN_A RUN_B --output json > proof.json
  infercrane report proof verify proof.json`,
	"results": `  infercrane results import benchmark-report.json
  infercrane results rank --query query.json --output json`,
	"replay": `  # Record the last day of traffic's shape, without prompts or answers
  infercrane replay qwen-prod --window 24h`,
	"capacity": `  infercrane capacity --window 168h`,
	"telemetry": `  # Read NVIDIA GPU metrics from dcgm-exporter every 15 seconds
  infercrane telemetry collect dcgm qwen-prod --url http://127.0.0.1:9400/metrics --interval 15s`,
	"artifact": `  infercrane artifact inspect ARTIFACT_ID
  infercrane artifact optimize list`,
	"training": `  infercrane training attach qwen-prod checkpoint.signed.json
  infercrane training list qwen-prod`,
	"orphans":      `  infercrane orphans`,
	"integrations": `  infercrane integrations`,
	"tenant":       `  infercrane tenant create acme --name "Acme Inc"`,
	"model-api":    `  infercrane model-api publish product --file product.json`,
	"system":       `  infercrane system instances`,
	"mcp": `  # Let an AI agent read deployments, endpoints, and operations
  infercrane mcp`,
	"serve": `  INFERCRANE_DATABASE_URL=postgres://... infercrane serve`,
}

// legacyReplacements names the command to use instead of an older one.
// Older commands still run; help lists them last and says what replaces them.
var legacyReplacements = map[string]string{
	"auth":      "infercrane whoami, login, or logout",
	"init":      "infercrane login, or set INFERCRANE_API_KEY",
	"events":    "infercrane logs",
	"recommend": "infercrane optimize propose",
	"autopilot": "infercrane optimize",
	"recipe":    "infercrane optimize",
	"recipes":   "infercrane optimize propose",
	"lab":       "infercrane optimize results",
	"burst":     "infercrane external",
	"session":   "",
}
