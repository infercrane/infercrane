# Dependencies

| Project | Repository | Purpose | License | Integration | Modified/forked | Obligations |
|---|---|---|---|---|---|---|
| vLLM Router | https://github.com/vllm-project/router | Request routing | Apache-2.0 | External pinned process | No | Preserve license/notices; audit Rust transitive dependencies |
| vLLM | https://github.com/vllm-project/vllm | Inference runtime | Apache-2.0 | External worker runtime | No | Preserve license/notices |
| SkyPilot | https://github.com/skypilot-org/skypilot | Stage 2 GPU provisioning | Apache-2.0 | Optional supported SDK adapter | No | Preserve license/notices |
| AIBrix | https://github.com/vllm-project/aibrix | Future Kubernetes backend | Apache-2.0 | Optional external backend | No | Preserve license/notices |
| KServe | https://github.com/kserve/kserve | Future Kubernetes backend | Apache-2.0 | Optional external backend | No | Preserve license/notices |
| FastAPI | https://github.com/fastapi/fastapi | HTTP API and gateway | MIT | Python library | No | Retain license |
| HTTPX | https://github.com/encode/httpx | Streaming HTTP client | BSD-3-Clause | Python library | No | Retain copyright/license |
| Pydantic | https://github.com/pydantic/pydantic | Validation | MIT | Python library | No | Retain license |
| SQLAlchemy | https://github.com/sqlalchemy/sqlalchemy | Persistence | MIT | Python library | No | Retain license |
| Typer | https://github.com/fastapi/typer | CLI | MIT | Python library | No | Retain license |
| Uvicorn | https://github.com/encode/uvicorn | ASGI server | BSD-3-Clause | Python library | No | Retain copyright/license |
| Rich | https://github.com/Textualize/rich | CLI presentation | MIT | Python library | No | Retain license |
| SQLite | https://sqlite.org | Stage 1 database | Public domain | Python stdlib/system | No | None |
| uv | https://github.com/astral-sh/uv | Development/package workflow | MIT OR Apache-2.0 | Tooling only | No | License choice applies on redistribution |

No AGPL, SSPL, BSL, Commons Clause, or other source-available dependency is approved for
the core. Lockfile and container-image transitive dependencies require automated license and
vulnerability review before a release.

