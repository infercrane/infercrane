"""Laptop broker for the Modal GPU PoC (development only).

The PoC's model server pods (internal/testtools/modal-gpu-proxy) ask this
process for a remote GPU. It holds the developer's existing Modal login,
starts one Modal container per lease (modal_gpu.py), and enforces the run's
spend cap:

  - at most --max-gpus containers at once and --max-starts in total,
  - the estimated spend (container seconds x the stated rate, counted from the
    moment a container is requested) never passes --budget-usd: new leases are
    refused near the cap and every container is stopped at it,
  - a lease whose pod stops renewing it is stopped.

The Modal app is ephemeral and lives exactly as long as this process, so
stopping the broker stops every container it started. The ledger it writes is
the run's GPU-time record; Modal's billing report is the authority for cost.

    uv run --no-project --with modal==1.6.4 python tools/poc-modal/broker.py \
        --token-file .infercrane/poc-modal/state.json --budget-usd 9
"""

from __future__ import annotations

import argparse
import json
import re
import secrets
import signal
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import modal_gpu  # noqa: E402

import modal  # noqa: E402

# Modal list prices (modal.com/pricing): the GPU's rate is the run's accelerator's
# (models.ACCELERATORS), plus $0.0000131 per physical core per second and $0.00000222 per
# GiB per second (read 2026-11-20).
GPU_PER_SECOND = modal_gpu.ACCELERATOR.usd_per_second
CORE_PER_SECOND = 0.0000131
GIB_PER_SECOND = 0.00000222
POD_SILENCE_SECONDS = 120
START_MARGIN_SECONDS = 120
STOP_GRACE_SECONDS = 45
LIVE = ("requested", "starting", "ready", "stopping")
# A container on a host whose NVIDIA driver differs from the run's pinned driver stops
# before loading the model; the lease is started again on another host this many times.
MAX_DRIVER_RETRIES = 3


def rate_per_second(dry: bool) -> float:
    if dry:
        return 0.5 * CORE_PER_SECOND + 1 * GIB_PER_SECOND
    return GPU_PER_SECOND + modal_gpu.GPU_CPU * CORE_PER_SECOND + modal_gpu.GPU_MEMORY_MIB / 1024 * GIB_PER_SECOND


class Broker:
    def __init__(self, token: str, budget: float, max_gpus: int, max_starts: int, ledger: Path, dry: bool, driver: str | None = None) -> None:
        self.token, self.budget, self.max_gpus, self.max_starts = token, budget, max_gpus, max_starts
        # Every setup of a run must serve on the same NVIDIA driver; set from --driver or
        # from the first GPU host that reports one (the baseline, which runs alone first).
        self.required_driver = driver
        self.ledger_path, self.dry = ledger, dry
        self.rate = rate_per_second(dry)
        self.leases: dict[str, dict] = {}
        self.calls: dict[str, modal.FunctionCall] = {}
        self.lock = threading.RLock()
        self.modal_lock = threading.Lock()  # one Modal call at a time
        self.exhausted = False
        self.started = time.time()

    # ---------------------------------------------------------- accounting

    def lease_seconds(self, lease: dict, now: float) -> float:
        """Seconds to bill. Modal bills a container from its start, not from the request:
        a request can wait many minutes for an H100. Until the container reports its start,
        the whole wait counts; after that, the start minus START_MARGIN_SECONDS for image
        loading before the function runs."""
        start = lease["requested_at"]
        if lease.get("container_started_at"):
            start = max(start, float(lease["container_started_at"]) - START_MARGIN_SECONDS)
        end = lease.get("ended_at") or now
        # Containers this lease already discarded on a mismatched host stay billed.
        return float(lease.get("prior_seconds", 0.0)) + max(0.0, end - start)

    def spend(self, now: float | None = None) -> float:
        now = now or time.time()
        with self.lock:
            return sum(self.lease_seconds(lease, now) for lease in self.leases.values()) * self.rate

    def live(self) -> list[dict]:
        return [lease for lease in self.leases.values() if lease["state"] in LIVE]

    # ---------------------------------------------------------- lease operations

    def create(self, pod: str, model: str, args: list[str]) -> tuple[int, dict]:
        try:
            modal_gpu.check_args(args, model)
        except ValueError as error:
            return 400, {"error": str(error)}
        with self.lock:
            spent = self.spend()
            reserve = 10 * 60 * self.rate  # a new server needs minutes before it can do useful work
            if self.exhausted or spent + reserve > self.budget:
                return 402, {"error": f"budget: ${spent:.2f} of ${self.budget:.2f} estimated; no new GPU"}
            if len(self.live()) >= self.max_gpus:
                return 429, {"error": f"{self.max_gpus} remote GPUs are already in use"}
            if len(self.leases) >= self.max_starts:
                return 409, {"error": f"{self.max_starts} remote servers were already started in this run"}
            lease_id = re.sub(r"[^a-z0-9-]", "-", pod.lower())[-40:].strip("-") + "-" + secrets.token_hex(3)
            now = time.time()
            lease = {"lease_id": lease_id, "pod": pod, "model": model, "args": list(args), "state": "requested",
                     "requested_at": now, "last_heartbeat": now, "api_key": secrets.token_urlsafe(24), "dry": self.dry}
            self.leases[lease_id] = lease
        deadline = now + (self.budget - spent) / self.rate  # this lease alone could not pass the cap after this
        lease["deadline"] = deadline
        with self.modal_lock:
            modal_gpu.leases.put(f"ctl:{lease_id}", self.control(now, deadline))
            function = modal_gpu.serve_dry if self.dry else modal_gpu.serve_gpu
            self.calls[lease_id] = function.spawn(lease_id, list(args), lease["api_key"], model)
        with self.lock:
            lease["state"], lease["call_id"] = "starting", self.calls[lease_id].object_id
        log(f"lease {lease_id} for pod {pod}: {' '.join(args)}")
        self.save()
        return 201, self.public(lease) | {"api_key": lease["api_key"]}

    def control(self, now: float, deadline: float) -> dict:
        control = {"expires_at": now + modal_gpu.STARTUP_SECONDS, "deadline": deadline}
        if self.required_driver:
            control["required_driver"] = self.required_driver
        return control

    def respawn(self, lease: dict, why: str) -> bool:
        """Starts a lease again after its host refused it. False when it may not: out of
        retries, or the budget cannot cover another start."""
        now = time.time()
        with self.lock:
            retries = int(lease.get("driver_retries", 0))
            if retries >= MAX_DRIVER_RETRIES or self.exhausted or self.spend(now) + 10 * 60 * self.rate > self.budget:
                return False
            lease["prior_seconds"] = self.lease_seconds(lease, now)
            for key in ("container_started_at", "ready_at", "url", "error", "reason", "log_tail", "startup_seconds", "stopped_at"):
                lease.pop(key, None)
            lease["requested_at"], lease["last_heartbeat"], lease["driver_retries"] = now, now, retries + 1
            lease.setdefault("refused_hosts", []).append(why)
        with self.modal_lock:
            modal_gpu.leases.put(f"st:{lease['lease_id']}", {"state": "requested"})
            modal_gpu.leases.put(f"ctl:{lease['lease_id']}", self.control(now, float(lease.get("deadline", now + modal_gpu.GPU_TIMEOUT_SECONDS))))
            function = modal_gpu.serve_dry if self.dry else modal_gpu.serve_gpu
            self.calls[lease["lease_id"]] = function.spawn(lease["lease_id"], list(lease["args"]), lease["api_key"], lease["model"])
        with self.lock:
            lease["state"], lease["call_id"] = "starting", self.calls[lease["lease_id"]].object_id
        log(f"lease {lease['lease_id']}: {why}; starting again on another host ({retries + 1} of {MAX_DRIVER_RETRIES})")
        return True

    def refresh(self, lease: dict) -> dict:
        if lease["state"] not in LIVE:
            return lease
        if time.time() - lease.get("refreshed_at", 0) < 2:
            return lease
        with self.modal_lock:
            remote = modal_gpu.leases.get(f"st:{lease['lease_id']}", {}) or {}
        with self.lock:
            lease["refreshed_at"] = time.time()
            for key in ("url", "error", "gpu", "gpu_facts", "vllm_version", "startup_seconds", "container_started_at", "ready_at", "stopped_at", "reason", "runtime_image", "server_env", "log_tail"):
                if key in remote:
                    lease[key] = remote[key]
            state = remote.get("state")
            driver = (remote.get("gpu_facts") or {}).get("driver")
            if self.required_driver is None and not self.dry and driver and driver != "unknown" and state != "failed":
                self.required_driver = driver
                log(f"every setup of this run must serve on NVIDIA driver {driver} (pinned by lease {lease['lease_id']})")
        if state == "failed" and remote.get("reason") == "driver_mismatch" and self.respawn(lease, str(remote.get("error", "driver mismatch"))):
            return lease
        with self.lock:
            if state == "ready" and lease["state"] == "starting":
                lease["state"] = "ready"
                log(f"lease {lease['lease_id']} ready after {remote.get('startup_seconds')} s on {remote.get('gpu', 'unknown GPU')}")
            elif state in ("stopped", "failed"):
                lease["state"], lease["ended_at"] = state, time.time()
                log(f"lease {lease['lease_id']} {state}: {remote.get('reason', '')} {remote.get('error', '')[:300]}")
        return lease

    def heartbeat(self, lease_id: str) -> tuple[int, dict]:
        with self.lock:
            lease = self.leases.get(lease_id)
            if lease is None:
                return 404, {"error": "unknown lease"}
            lease["last_heartbeat"] = time.time()
            renew = lease["state"] in ("starting", "ready") and time.time() - lease.get("renewed_at", 0) > 10
        if renew:
            with self.modal_lock:
                control = modal_gpu.leases.get(f"ctl:{lease_id}", {}) or {}
                control["expires_at"] = time.time() + modal_gpu.LEASE_SECONDS
                modal_gpu.leases.put(f"ctl:{lease_id}", control)
            lease["renewed_at"] = time.time()
        return 200, self.public(self.refresh(lease))

    def stop(self, lease_id: str, reason: str) -> None:
        with self.lock:
            lease = self.leases.get(lease_id)
            if lease is None or lease["state"] not in ("requested", "starting", "ready"):
                return
            lease["state"], lease["stop_requested_at"], lease["stop_reason"] = "stopping", time.time(), reason
        with self.modal_lock:
            control = modal_gpu.leases.get(f"ctl:{lease_id}", {}) or {}
            control["stop"] = True
            modal_gpu.leases.put(f"ctl:{lease_id}", control)
        log(f"lease {lease_id}: stop ({reason})")
        self.save()

    def cancel(self, lease_id: str) -> None:
        call = self.calls.get(lease_id)
        if call is not None:
            with self.modal_lock:
                try:
                    call.cancel()
                except Exception as error:  # noqa: BLE001 - already finished
                    log(f"lease {lease_id}: cancel: {error}")
        with self.lock:
            lease = self.leases[lease_id]
            if lease["state"] in LIVE:
                lease["state"], lease["ended_at"] = "stopped", time.time()
                lease.setdefault("reason", "cancelled")

    def stop_all(self, reason: str) -> None:
        for lease in list(self.leases.values()):
            self.stop(lease["lease_id"], reason)

    # ---------------------------------------------------------- watchdog

    def watch(self) -> None:
        while True:
            time.sleep(5)
            try:
                self.tick()
            except Exception as error:  # noqa: BLE001 - keep enforcing the cap
                log(f"watchdog: {type(error).__name__}: {error}")

    def tick(self) -> None:
        now = time.time()
        for lease in list(self.leases.values()):
            self.refresh(lease)
            if lease["state"] in ("starting", "ready") and now - lease["last_heartbeat"] > POD_SILENCE_SECONDS:
                self.stop(lease["lease_id"], f"pod silent for {POD_SILENCE_SECONDS} s")
            if lease["state"] == "stopping" and now - lease["stop_requested_at"] > STOP_GRACE_SECONDS:
                self.cancel(lease["lease_id"])
            call = self.calls.get(lease["lease_id"])
            if lease["state"] in LIVE and call is not None:
                try:
                    with self.modal_lock:
                        call.get(timeout=0)
                    finished = True
                except Exception as error:  # noqa: BLE001 - still running, or ended with an error
                    # Anything that looks like "not finished yet" keeps the lease counted:
                    # a wrong guess here must cost accounting accuracy, never the cap.
                    finished = not ("timeout" in type(error).__name__.lower() or isinstance(error, TimeoutError))
                if finished:
                    lease["refreshed_at"] = 0  # read the container's final state before calling it failed
                    self.refresh(lease)
                    if lease["state"] in LIVE and self.calls.get(lease["lease_id"]) is not call:
                        continue  # started again on another host
                    with self.lock:
                        lease["state"], lease["ended_at"] = ("failed" if lease["state"] != "stopping" else "stopped"), time.time()
                    self.refresh(lease)
        if not self.exhausted and self.spend(now) >= self.budget:
            self.exhausted = True
            log(f"budget reached: ${self.spend(now):.2f} of ${self.budget:.2f}; stopping every remote GPU")
            self.stop_all("budget reached")
        self.save()

    # ---------------------------------------------------------- views

    def public(self, lease: dict) -> dict:
        return {key: value for key, value in lease.items() if key not in ("api_key",)}

    def ledger(self) -> dict:
        now = time.time()
        with self.lock:
            rows = []
            for lease in self.leases.values():
                row = self.public(lease)
                row["seconds"] = round(self.lease_seconds(lease, now), 1)
                row["estimated_usd"] = round(row["seconds"] * self.rate, 4)
                rows.append(row)
            return {"schema_version": "infercrane.poc-modal-ledger/v1", "dry": self.dry, "budget_usd": self.budget,
                    "rate_usd_per_second": self.rate, "rate_basis": f"Modal list price: {modal_gpu.ACCELERATOR.description} + container CPU and memory requests",
                    "engine": modal_gpu.ENGINE.key, "runtime_image": modal_gpu.VLLM_IMAGE, "gpu_type": modal_gpu.ACCELERATOR.key,
                    "billing_basis": f"from the container's reported start minus {START_MARGIN_SECONDS} s (from the request until the start is known) to its end",
                    "estimated_usd": round(self.spend(now), 4), "exhausted": self.exhausted, "started_at": self.started,
                    "updated_at": now, "leases": rows}

    def save(self) -> None:
        self.ledger_path.parent.mkdir(parents=True, exist_ok=True)
        temporary = self.ledger_path.with_suffix(".tmp")
        temporary.write_text(json.dumps(self.ledger(), indent=2))
        temporary.replace(self.ledger_path)


def log(message: str) -> None:
    print(f"{time.strftime('%H:%M:%S')} broker  {message}", flush=True)


def handler(broker: Broker):
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *args) -> None:  # noqa: ANN002 - quiet access log
            pass

        def reply(self, code: int, body: dict | None = None) -> None:
            data = json.dumps(body).encode() if body is not None else b""
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def authorized(self) -> bool:
            presented = self.headers.get("Authorization", "")
            return secrets.compare_digest(presented, f"Bearer {broker.token}")

        def body(self) -> dict:
            length = int(self.headers.get("Content-Length", "0") or 0)
            return json.loads(self.rfile.read(length) or b"{}") if length else {}

        def do_GET(self) -> None:  # noqa: N802
            if not self.authorized():
                return self.reply(401, {"error": "unauthorized"})
            if self.path == "/v1/ledger":
                return self.reply(200, broker.ledger())
            match = re.fullmatch(r"/v1/leases/([a-z0-9-]+)", self.path)
            lease = broker.leases.get(match.group(1)) if match else None
            if lease is None:
                return self.reply(404, {"error": "unknown lease"})
            return self.reply(200, broker.public(broker.refresh(lease)))

        def do_POST(self) -> None:  # noqa: N802
            if not self.authorized():
                return self.reply(401, {"error": "unauthorized"})
            if self.path == "/v1/leases":
                body = self.body()
                code, payload = broker.create(str(body.get("pod", "pod")), str(body.get("model", "")), [str(arg) for arg in body.get("args", [])])
                return self.reply(code, payload)
            if self.path == "/v1/stop-all":
                broker.stop_all("stopped by the harness")
                return self.reply(200, broker.ledger())
            match = re.fullmatch(r"/v1/leases/([a-z0-9-]+)/heartbeat", self.path)
            if match:
                code, payload = broker.heartbeat(match.group(1))
                return self.reply(code, payload)
            self.reply(404, {"error": "not found"})

        def do_DELETE(self) -> None:  # noqa: N802
            if not self.authorized():
                return self.reply(401, {"error": "unauthorized"})
            match = re.fullmatch(r"/v1/leases/([a-z0-9-]+)", self.path)
            if not match or match.group(1) not in broker.leases:
                return self.reply(404, {"error": "unknown lease"})
            broker.stop(match.group(1), "released by the pod")
            self.reply(204)

    return Handler


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--token-file", required=True, help="PoC state file holding the worker API key the pods present")
    parser.add_argument("--budget-usd", type=float, required=True)
    parser.add_argument("--max-gpus", type=int, default=modal_gpu.MAX_GPUS)
    parser.add_argument("--max-starts", type=int, default=6)
    parser.add_argument("--listen", default="127.0.0.1:18095")
    parser.add_argument("--ledger", required=True)
    parser.add_argument("--dry", action="store_true", help="CPU stand-in servers, no GPU")
    parser.add_argument("--driver", help="NVIDIA driver every setup must serve on (default: the first GPU host's)")
    args = parser.parse_args()
    token = json.loads(Path(args.token_file).read_text())["api_key"]
    if args.max_gpus > modal_gpu.MAX_GPUS:
        raise SystemExit(f"--max-gpus may not exceed {modal_gpu.MAX_GPUS}")
    broker = Broker(token, args.budget_usd, args.max_gpus, args.max_starts, Path(args.ledger), args.dry, args.driver)
    host, port = args.listen.rsplit(":", 1)
    server = ThreadingHTTPServer((host, int(port)), handler(broker))
    kind = "CPU stand-in servers" if args.dry else f"{modal_gpu.ACCELERATOR.key} with {modal_gpu.ENGINE.key}"
    log(f"{kind}: cap ${args.budget_usd:.2f}, at most {args.max_gpus} at once and {args.max_starts} in total, ${broker.rate * 3600:.4f}/h each; listening on {args.listen}")
    def interrupt(*_: object) -> None:
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, interrupt)
    with modal.enable_output(), modal_gpu.app.run():
        threading.Thread(target=broker.watch, daemon=True).start()
        try:
            server.serve_forever()
        except KeyboardInterrupt:
            pass
        finally:
            broker.stop_all("broker stopping")
            broker.save()
            log(f"stopped; estimated ${broker.spend():.2f}; the ephemeral app stops every container now")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
