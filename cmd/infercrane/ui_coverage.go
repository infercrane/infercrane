package main

// uiCapabilities is the contract between public product capabilities and
// their terminal workspace representation. Explicit CLI handoffs are deliberate:
// complex authoring and administrative workflows are clearer as reviewable
// commands/specs than as cramped terminal forms.
var uiCapabilities = []struct {
	Capability string
	Surface    string
}{
	{"stable endpoint, logical model, and environment", "Dashboard endpoint overview + CLI"},
	{"serving-plan bindings and adoption ownership", "Dashboard endpoint overview + CLI"},
	{"endpoint candidate and Release Guard evidence", "Dashboard endpoint rollout + CLI"},
	{"environment, admission, and alert policy", "Dashboard endpoint policies + CLI"},
	{"signed semantic quality evidence", "Dashboard deployment rollout + CLI"},
	{"deployment health and endpoint", "Overview"},
	{"deterministic explanation", "Overview"},
	{"durable operation and cancellation", "Operations + action"},
	{"immutable revisions", "Rollout"},
	{"Release Guard evaluate and promote", "Rollout + actions"},
	{"request telemetry and cold starts", "Performance"},
	{"benchmark history and reproduction", "Performance + CLI handoff"},
	{"provider targets, replicas, and artifacts", "Infrastructure"},
	{"autoscaling bounds and decisions", "Scaling + CLI handoff"},
	{"durable event history and payloads", "Events"},
	{"test inference request", "CLI handoff"},
	{"deployment plan, deploy, and apply", "CLI/spec handoff"},
	{"candidate authoring, rejection, and rollback", "CLI handoff"},
	{"safe deployment deletion", "CLI plan + typed confirmation"},
	{"tenant, credential, and target administration", "CLI handoff"},
	{"doctor, orphan inventory, and audit", "CLI/JSON handoff"},
}
