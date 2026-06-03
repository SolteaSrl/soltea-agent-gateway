"""Store persistente dei token per-agente creati a runtime.

I token statici arrivano dall'ambiente (GW_AGENT_TOKENS); quelli creati al volo
via l'endpoint /provision vengono invece salvati qui, su un file JSON, così
sopravvivono al restart del gateway senza bisogno di un DB.

Il formato del file e' un semplice oggetto {agent_id: token}. La scrittura e'
atomica (file temporaneo + rename) per non corrompere lo store in caso di crash.
"""
from __future__ import annotations

import contextlib
import json
import logging
import os
import secrets
from pathlib import Path

log = logging.getLogger("soltea_gateway")


class AgentTokenStore:
    """Persistenza su file JSON dei token agente provisionati a runtime."""

    def __init__(self, path: Path) -> None:
        self.path = Path(path)
        self._tokens: dict[str, str] = {}
        self._load()

    def _load(self) -> None:
        if not self.path.exists():
            self._tokens = {}
            return
        try:
            raw = json.loads(self.path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            log.exception("Token store illeggibile: %s (riparto vuoto)", self.path)
            self._tokens = {}
            return
        if not isinstance(raw, dict):
            log.error("Token store malformato (atteso oggetto): %s", self.path)
            self._tokens = {}
            return
        self._tokens = {str(k): str(v) for k, v in raw.items()}

    def _flush(self) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        tmp = self.path.with_suffix(self.path.suffix + ".tmp")
        tmp.write_text(json.dumps(self._tokens, indent=2, sort_keys=True), encoding="utf-8")
        # chmod best-effort: su filesystem che non lo supportano non e' fatale.
        with contextlib.suppress(OSError):
            os.chmod(tmp, 0o600)
        os.replace(tmp, self.path)

    def tokens(self) -> dict[str, str]:
        """Copia della mappa {agent_id: token} persistita."""
        return dict(self._tokens)

    def has(self, agent_id: str) -> bool:
        return agent_id in self._tokens

    def provision(self, agent_id: str, *, rotate: bool = False) -> str:
        """Crea (o ruota) il token di un agente e lo persiste.

        Se l'agente esiste gia' e rotate=False, solleva KeyError per non
        sovrascrivere silenziosamente un segreto in uso.
        """
        if agent_id in self._tokens and not rotate:
            raise KeyError(agent_id)
        token = secrets.token_urlsafe(32)
        self._tokens[agent_id] = token
        self._flush()
        return token

    def revoke(self, agent_id: str) -> bool:
        """Rimuove il token di un agente. True se esisteva."""
        if agent_id not in self._tokens:
            return False
        del self._tokens[agent_id]
        self._flush()
        return True
