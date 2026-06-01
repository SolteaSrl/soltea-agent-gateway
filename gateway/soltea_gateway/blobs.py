"""Blob store su filesystem per gli zip dei ticket.

L'orchestratrice carica lo zip (POST /blobs) e ottiene un blob_id; l'agente lo
scarica (GET /blobs/{id}). Gli id sono uuid4 non indovinabili. GC pigro per TTL.
"""
from __future__ import annotations

import os
import time
import uuid
from pathlib import Path


class BlobStore:
    def __init__(self, root: Path, ttl_seconds: int) -> None:
        self.root = root
        self.ttl_seconds = ttl_seconds
        self.root.mkdir(parents=True, exist_ok=True)

    def _path(self, blob_id: str) -> Path:
        # uuid4 in esadecimale: nessun separatore di percorso, ma normalizziamo comunque.
        safe = uuid.UUID(blob_id).hex
        return self.root / f"{safe}.zip"

    def put(self, data: bytes) -> str:
        blob_id = uuid.uuid4().hex
        path = self.root / f"{blob_id}.zip"
        path.write_bytes(data)
        return blob_id

    def get(self, blob_id: str) -> bytes | None:
        try:
            path = self._path(blob_id)
        except ValueError:
            return None
        if not path.exists():
            return None
        return path.read_bytes()

    def delete(self, blob_id: str) -> None:
        try:
            path = self._path(blob_id)
        except ValueError:
            return
        path.unlink(missing_ok=True)

    def gc(self) -> int:
        """Rimuove i blob piu' vecchi del TTL. Ritorna quanti ne ha cancellati."""
        now = time.time()
        removed = 0
        for f in self.root.glob("*.zip"):
            try:
                if now - f.stat().st_mtime > self.ttl_seconds:
                    f.unlink(missing_ok=True)
                    removed += 1
            except OSError:
                continue
        return removed
