from pathlib import Path

from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_prefix="INFERCRANE_", extra="ignore")

    state_dir: Path = Path.home() / ".infercrane"
    database_url: str | None = None
    host: str = "127.0.0.1"
    port: int = 8080
    health_interval_seconds: float = 10.0
    upstream_connect_timeout_seconds: float = 10.0
    upstream_read_timeout_seconds: float = 300.0
    router_binary: str = "vllm-router"
    router_start_port: int = 18080
    api_key: str = "infercrane"

    @property
    def resolved_database_url(self) -> str:
        return self.database_url or f"sqlite:///{self.state_dir / 'state.db'}"
