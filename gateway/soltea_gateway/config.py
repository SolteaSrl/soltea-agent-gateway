"""Configurazione del gateway, letta dall'ambiente.

Niente DB: i token e i parametri arrivano da variabili d'ambiente (o da un file
.env caricato dal servizio systemd). Vedi gateway/README.md.
"""
from __future__ import annotations

import os
from dataclasses import dataclass, field
from pathlib import Path


def _split_tokens(raw: str) -> dict[str, str]:
    """Parsa "id1:token1,id2:token2" in {id: token}.

    Usato per i token degli agenti, così ogni agent_id ha il suo segreto.
    """
    out: dict[str, str] = {}
    for chunk in raw.split(","):
        chunk = chunk.strip()
        if not chunk:
            continue
        if ":" not in chunk:
            raise ValueError(f"Token agente malformato (manca ':'): {chunk!r}")
        agent_id, token = chunk.split(":", 1)
        out[agent_id.strip()] = token.strip()
    return out


@dataclass
class Config:
    host: str = "127.0.0.1"
    port: int = 8182
    # Token dell'orchestratrice (Claudia). Obbligatorio in produzione.
    orchestrator_token: str = ""
    # Mappa agent_id -> token. Un agente puo' registrarsi solo se il suo token combacia.
    agent_tokens: dict[str, str] = field(default_factory=dict)
    # Se True, accetta qualsiasi agent_id con un unico token condiviso (solo dev).
    shared_agent_token: str = ""
    # Directory per i blob (zip dei ticket).
    blob_dir: Path = Path("/var/lib/soltea-gateway/blobs")
    # TTL dei blob in secondi (GC pigro).
    blob_ttl_seconds: int = 24 * 3600
    # Dimensione massima di un blob (byte).
    blob_max_bytes: int = 200 * 1024 * 1024
    heartbeat_seconds: int = 30

    @classmethod
    def from_env(cls, env: dict[str, str] | None = None) -> "Config":
        e = os.environ if env is None else env
        return cls(
            host=e.get("GW_HOST", "127.0.0.1"),
            port=int(e.get("GW_PORT", "8182")),
            orchestrator_token=e.get("GW_ORCH_TOKEN", ""),
            agent_tokens=_split_tokens(e.get("GW_AGENT_TOKENS", "")),
            shared_agent_token=e.get("GW_SHARED_AGENT_TOKEN", ""),
            blob_dir=Path(e.get("GW_BLOB_DIR", "/var/lib/soltea-gateway/blobs")),
            blob_ttl_seconds=int(e.get("GW_BLOB_TTL_SECONDS", str(24 * 3600))),
            blob_max_bytes=int(e.get("GW_BLOB_MAX_BYTES", str(200 * 1024 * 1024))),
            heartbeat_seconds=int(e.get("GW_HEARTBEAT_SECONDS", "30")),
        )

    def check_agent_token(self, agent_id: str, token: str) -> bool:
        """True se (agent_id, token) e' autorizzato a registrarsi come agente."""
        if self.shared_agent_token and token == self.shared_agent_token:
            return True
        expected = self.agent_tokens.get(agent_id)
        return bool(expected) and token == expected

    def check_orchestrator_token(self, token: str) -> bool:
        # In dev, se non e' configurato alcun token, accetta tutto.
        if not self.orchestrator_token:
            return True
        return token == self.orchestrator_token
