# Local end-to-end optimization PoC

Runs one real optimization campaign through the real InferCrane control plane on a laptop:
a kind cluster, Postgres and `infercrane serve` in the cluster, a llama.cpp model server, real
traffic through the gateway, a Replay capture, a campaign that measures each candidate against the
current setup in turns, the control plane's built-in answer check, ranking, and activation.

The laptop has no GPU. Pods schedule on an invented `infercrane.dev/simulated-accelerator`
resource labeled `SIMULATED-CPU` and run on the CPU. Every number is a CPU number; the run proves
the product path, not GPU performance. Costs use a stated nominal rate card ($0.25 per hour per
node slot), not a market price. Nothing here spends money.

```bash
make poc-local-up     # cluster, registry, model image, control plane (first run: several minutes)
make poc-local-run    # baseline, traffic, capture, campaign, quality, ranking, activation
make poc-local-resume # keep watching the same campaign after the watcher or laptop stopped
make poc-monitor      # live engine view of every serving pod
make poc-local-down   # delete everything and check nothing is left
```

`make poc-local` runs `up` and `run` together. `tools/poc-local/record.sh` (macOS, iTerm2 and
Chrome) opens the four recording windows: run log, engine monitor, pods, and the dashboard
pointed at the local control plane.

Campaigns are approved for 24 hours. A CPU campaign runs every candidate in turns and can take
hours, and an approval that expires mid-run stops the campaign.

The campaign ranks on `cost-efficiency`: output tokens per second per the stated rate card, which is
what the result card reports. It replays the capture on the `buffered` profile, 8 requests at a time
without streaming. Throughput is also the metric the control-drift check watches, so it is the
steadiest one on a shared CPU. Earlier versions of the harness asked for `interactive` on
`buffered`, which then ranked on p95 time to first token. Buffered runs never measure it, so those
campaigns ended `measured_ranking_inconclusive` whatever they measured.

A loaded laptop restarts model servers and drops the `kubectl port-forward` to the control plane.
The harness then restarts the port-forward and repeats the call for up to 10 minutes, but only a
call that cannot apply twice: a read, a write with an idempotency key, or a request the control
plane never received. If the connection drops after an approval or activation was sent, the harness
reads the campaign back instead of sending it again.

Postgres keeps its data on a volume in the kind node, so a database container restarted by memory
or disk pressure keeps the campaign; `make poc-local-down` deletes it with the cluster. If the
control plane no longer knows the campaign, `make poc-local-resume` stops at once and says to start a
fresh run instead of waiting.

Requirements: Docker, kubectl, Go, curl, Python 3.9+. With 7.7 GB of Docker memory, the first run
had model servers killed for running out of memory and the control plane lost its database for
hours; give Docker more memory than that. Keep free disk space too: when the Mac's disk filled,
Docker's disk turned read-only mid-run. The working minimum is not measured yet. Pinned inputs: kind node image, llama.cpp
server image, Qwen3-0.6B revision and its Q8_0 GGUF (checked by sha256), Postgres and registry
images. State, the API key, and outputs live in `.infercrane/poc/` (git-ignored).

On a GPU cluster the same control plane runs unchanged; only the cluster context, accelerator
resource and label, model image, price row, and workload change.
