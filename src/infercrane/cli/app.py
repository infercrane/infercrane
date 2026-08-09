from __future__ import annotations

import asyncio
from typing import Annotated

import httpx
import typer
import uvicorn
from rich.console import Console
from rich.table import Table

from infercrane.application import ConflictError, ControlPlane, NotFoundError
from infercrane.application.reconciliation import Reconciler
from infercrane.domain.models import DeploymentCreate, RoutingStrategy, TargetCreate
from infercrane.gateway import RouteDirectory, create_gateway_app
from infercrane.metrics import VLLMMetricsCollector
from infercrane.persistence import Database
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
    name: str = typer.Option(...),
    targets: str = typer.Option(..., help="Comma-separated target names"),
) -> None:
    """Create an idempotent logical deployment over existing targets."""
    _, _, control = context()
    try:
        row = control.create_deployment(
            DeploymentCreate(
                name=name,
                model=model,
                targets=[item.strip() for item in targets.split(",") if item.strip()],
            )
        )
    except (ValueError, ConflictError, NotFoundError) as exc:
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
    _, _, control = context()
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
        collector = VLLMMetricsCollector()
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


@app.command("delete")
def delete_deployment(deployment: str) -> None:
    _, _, control = context()
    control.delete_deployment(deployment)
    console.print(f"[green]✓[/green] deployment {deployment} deleted")


@app.command()
def serve() -> None:
    """Run the stable OpenAI-compatible gateway and reconciliation loop."""
    settings, database, _ = context()
    routes = RouteDirectory()
    router = VLLMRouterBackend(settings)
    reconciler = Reconciler(settings, database, routes, router)
    gateway = create_gateway_app(settings, routes, reconciler=reconciler)
    console.print(f"InferCrane gateway listening on http://{settings.host}:{settings.port}/v1")
    uvicorn.run(gateway, host=settings.host, port=settings.port)


def _metric(value: float | None) -> str:
    return "N/A" if value is None else f"{value:g}"


def _percent(value: float | None) -> str:
    return "N/A" if value is None else f"{value:.0%}"
