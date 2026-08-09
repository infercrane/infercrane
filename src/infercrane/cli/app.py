from __future__ import annotations

import asyncio
import json
from pathlib import Path
from typing import Annotated

import httpx
import typer
import uvicorn
from rich.console import Console
from rich.table import Table

from infercrane.application import ConflictError, ControlPlane, NotFoundError
from infercrane.application.reconciliation import Reconciler
from infercrane.domain.models import DeploymentCreate, RoutingStrategy, TargetCreate
from infercrane.domain.spec import DeploymentFile
from infercrane.gateway import RouteDirectory, create_gateway_app
from infercrane.metrics import VLLMMetricsCollector
from infercrane.persistence import Database
from infercrane.provisioners import DeploymentSpec, SkyPilotProvisioner, SkyPilotUnavailable
from infercrane.routers import VLLMRouterBackend
from infercrane.settings import Settings

app = typer.Typer(name="infercrane", no_args_is_help=True, add_completion=False)
target_app = typer.Typer(help="Manage existing inference targets.")
app.add_typer(target_app, name="target")
console = Console()


def context() -> tuple[Settings, Database, ControlPlane]:
    settings = Settings()
    database = Database(settings)
    database.create_schema()
    return settings, database, ControlPlane(database)


def fail(exc: Exception) -> None:
    console.print(f"[red]Error:[/red] {exc}")
    raise typer.Exit(2)


@target_app.command("add")
def target_add(
    name: str,
    url: str = typer.Option(...),
    runtime: str = typer.Option("vllm"),
    upstream_model: str | None = typer.Option(None, "--upstream-model"),
) -> None:
    """Register an existing inference server."""
    _, _, control = context()
    try:
        row = control.add_target(
            TargetCreate(
                name=name,
                url=url,
                runtime=runtime,
                upstream_model_name=upstream_model,
            )
        )
    except (ValueError, ConflictError) as exc:
        fail(exc)
    console.print(f"[green]✓[/green] target {row.name} registered")


@target_app.command("list")
def target_list() -> None:
    _, _, control = context()
    table = Table("NAME", "URL", "RUNTIME", "HEALTH")
    for row in control.list_targets():
        table.add_row(row.name, row.url, row.runtime, row.health)
    console.print(table)


@app.command()
def deploy(
    model: str,
    name: str | None = typer.Option(None),
    targets: str | None = typer.Option(None, help="Comma-separated existing target names"),
    cloud: str | None = typer.Option(None, help="Provision with SkyPilot on this cloud"),
    gpu: str | None = typer.Option(None, help="GPU accelerator requested from SkyPilot"),
    region: str | None = typer.Option(None),
    runtime_arg: Annotated[list[str] | None, typer.Option("--runtime-arg")] = None,
) -> None:
    """Create a deployment over existing targets or provision one with SkyPilot."""
    settings, _, control = context()
    try:
        spec_file = (
            DeploymentFile.load(Path(model)) if Path(model).suffix in {".yaml", ".yml"} else None
        )
        if spec_file:
            if targets or cloud or gpu or region or runtime_arg or name:
                raise ValueError("deployment YAML cannot be combined with deployment flags")
            name = spec_file.name
            model = spec_file.model.id
            cloud = spec_file.provider.cloud
            gpu = spec_file.resources.gpu
            region = spec_file.provider.region
            runtime_arg = spec_file.runtime.args
        if not name:
            raise ValueError("--name is required")
        if targets and (cloud or gpu):
            raise ValueError("use either --targets or --cloud/--gpu, not both")
        if targets:
            target_names = [item.strip() for item in targets.split(",") if item.strip()]
        elif cloud and gpu:
            try:
                existing = control.resolve_deployment(name)
            except NotFoundError:
                existing = None
            if existing is not None:
                details = json.loads(existing.targets[0].provider_details_json or "{}")
                same = (
                    existing.deployment.model == model
                    and len(existing.targets) == 1
                    and existing.targets[0].provider == "skypilot"
                    and details.get("cloud") == cloud
                    and details.get("gpu") == gpu
                    and details.get("region") == region
                )
                if not same:
                    raise ConflictError(
                        f"deployment {name!r} already exists with different configuration"
                    )
                console.print(f"[green]✓[/green] deployment {name} already exists")
                return
            provisioned = asyncio.run(
                SkyPilotProvisioner(worker_api_key=settings.api_key).deploy(
                    DeploymentSpec(
                        name=name,
                        model=model,
                        cloud=cloud,
                        gpu=gpu,
                        region=region,
                        runtime_args=runtime_arg or [],
                    )
                )
            )
            control.add_provisioned_target(provisioned, provider="skypilot")
            target_names = [provisioned.name]
        else:
            raise ValueError("provide --targets or both --cloud and --gpu")
        row = control.create_deployment(
            DeploymentCreate(
                name=name,
                model=model,
                targets=target_names,
                routing_strategy=(spec_file.routing.strategy if spec_file else "round-robin"),
            )
        )
    except (ValueError, ConflictError, NotFoundError, SkyPilotUnavailable) as exc:
        fail(exc)
    console.print(f"[green]✓[/green] deployment {row.name} created")


@app.command()
def deployments() -> None:
    _, _, control = context()
    table = Table("NAME", "MODEL", "RUNTIME", "ROUTING", "STATUS")
    for row in control.list_deployments():
        table.add_row(row.name, row.model, row.runtime, row.routing_strategy, row.observed_state)
    console.print(table)


@app.command()
def route(deployment: str, strategy: Annotated[RoutingStrategy, typer.Option()]) -> None:
    _, _, control = context()
    try:
        row = control.set_routing(deployment, strategy)
    except NotFoundError as exc:
        fail(exc)
    console.print(f"[green]✓[/green] {row.name} routing set to {row.routing_strategy}")


@app.command()
def status(deployment: str) -> None:
    settings, _, control = context()
    try:
        resolved = control.resolve_deployment(deployment)
    except NotFoundError as exc:
        fail(exc)
    row = resolved.deployment
    healthy = sum(target.health == "healthy" for target in resolved.targets)
    console.print(f"[bold]{row.name}[/bold]  {row.observed_state.upper()}")
    console.print(f"Model       {row.model}")
    console.print(f"Runtime     {row.runtime}")
    console.print(f"Replicas    {len(resolved.targets)}")
    console.print(f"Healthy     {healthy}")
    console.print(f"Routing     {row.routing_strategy}\n")

    async def collect():
        collector = VLLMMetricsCollector(api_key=settings.api_key)
        results = []
        for item in resolved.targets:
            try:
                results.append((item, await collector.collect(item.url)))
            except (httpx.HTTPError, ValueError):
                results.append((item, None))
        return results

    table = Table("TARGET", "RUNNING", "WAITING", "KV", "CACHE")
    for item, metrics in asyncio.run(collect()):
        if not metrics:
            table.add_row(item.name, "N/A", "N/A", "N/A", "N/A")
            continue
        hit_rate = (
            metrics.prefix_cache_hits / metrics.prefix_cache_queries
            if metrics.prefix_cache_hits is not None and metrics.prefix_cache_queries
            else None
        )
        table.add_row(
            item.name,
            _metric(metrics.requests_running),
            _metric(metrics.requests_waiting),
            _percent(metrics.kv_cache_usage),
            _percent(hit_rate),
        )
    console.print(table)
    stats = control.request_stats(row.id)
    console.print(f"\nRequests/s  {stats.requests_per_second:.2f}")
    console.print(f"Error rate  {stats.error_rate:.1%}")
    console.print(f"P50 latency {_latency(stats.p50_latency_ms)}")
    console.print(f"P95 latency {_latency(stats.p95_latency_ms)}")


@app.command("delete")
def delete_deployment(
    deployment: str,
    keep_resources: bool = typer.Option(False, help="Delete control-plane state only"),
) -> None:
    _, _, control = context()
    try:
        resolved = control.resolve_deployment(deployment)
    except NotFoundError:
        resolved = None
    if resolved and not keep_resources:
        for target in resolved.targets:
            if target.provider == "skypilot" and target.provider_resource_id:
                try:
                    asyncio.run(SkyPilotProvisioner().destroy(target.provider_resource_id))
                except (SkyPilotUnavailable, RuntimeError, ValueError):
                    fail(
                        RuntimeError(
                            f"resource cleanup failed for {target.provider_resource_id}; "
                            "deployment was not deleted. Retry or use --keep-resources"
                        )
                    )
    control.delete_deployment(deployment)
    console.print(f"[green]✓[/green] deployment {deployment} deleted")


@app.command()
def inspect(deployment: str) -> None:
    """Show provider IDs, worker endpoints, runtime versions, and generated details."""
    _, _, control = context()
    try:
        resolved = control.resolve_deployment(deployment)
    except NotFoundError as exc:
        fail(exc)
    console.print(f"[bold]{resolved.deployment.name}[/bold]")
    console.print(f"Model      {resolved.deployment.model}")
    console.print(f"Runtime    {resolved.deployment.runtime}")
    for target in resolved.targets:
        console.print(f"\n[bold]{target.name}[/bold]")
        console.print(f"Provider   {target.provider}")
        console.print(f"Resource   {target.provider_resource_id or 'external'}")
        console.print(f"Endpoint   {target.url}")
        if target.provider_details_json:
            console.print_json(json=target.provider_details_json)


@app.command()
def serve() -> None:
    """Run the stable OpenAI-compatible gateway and reconciliation loop."""
    settings, database, _ = context()
    routes = RouteDirectory()
    router = VLLMRouterBackend(settings)
    reconciler = Reconciler(settings, database, routes, router)
    gateway = create_gateway_app(settings, routes, reconciler=reconciler, database=database)
    console.print(f"InferCrane gateway listening on http://{settings.host}:{settings.port}/v1")
    uvicorn.run(gateway, host=settings.host, port=settings.port)


def _metric(value: float | None) -> str:
    return "N/A" if value is None else f"{value:g}"


def _percent(value: float | None) -> str:
    return "N/A" if value is None else f"{value:.0%}"


def _latency(value: float | None) -> str:
    return "N/A" if value is None else f"{value:.1f} ms"
