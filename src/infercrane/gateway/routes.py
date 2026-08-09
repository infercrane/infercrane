from __future__ import annotations

from dataclasses import dataclass
from threading import Lock


@dataclass(frozen=True)
class RouteSnapshot:
    deployment_id: str
    alias: str
    upstream_model: str
    router_url: str


class RouteDirectory:
    """Atomic, in-memory data-plane snapshot independent of live database reads."""

    def __init__(self):
        self._lock = Lock()
        self._routes: dict[str, RouteSnapshot] = {}

    def replace(self, routes: list[RouteSnapshot]) -> None:
        replacement = {route.alias: route for route in routes}
        with self._lock:
            self._routes = replacement

    def put(self, route: RouteSnapshot) -> None:
        with self._lock:
            self._routes = {**self._routes, route.alias: route}

    def remove(self, alias: str) -> None:
        with self._lock:
            self._routes = {key: value for key, value in self._routes.items() if key != alias}

    def get(self, alias: str) -> RouteSnapshot | None:
        return self._routes.get(alias)

    def list(self) -> tuple[RouteSnapshot, ...]:
        return tuple(sorted(self._routes.values(), key=lambda route: route.alias))
