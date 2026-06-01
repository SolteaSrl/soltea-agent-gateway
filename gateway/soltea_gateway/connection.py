"""Wrapper attorno a una WebSocket con invio serializzato.

Starlette permette un solo writer per volta: piu' coroutine (il loop di ricezione
di un capo e il relay dall'altro capo) possono voler scrivere sulla stessa socket,
quindi serializziamo gli invii con un lock.
"""
from __future__ import annotations

import asyncio
import time
from typing import Any

from fastapi import WebSocket


class Connection:
    def __init__(self, ws: WebSocket, role: str, peer_id: str) -> None:
        self.ws = ws
        self.role = role          # "agent" | "orchestrator"
        self.peer_id = peer_id    # agent_id oppure client_id
        self._send_lock = asyncio.Lock()
        self.last_seen = time.monotonic()

    async def send(self, frame: dict[str, Any]) -> None:
        async with self._send_lock:
            await self.ws.send_json(frame)

    def touch(self) -> None:
        self.last_seen = time.monotonic()

    def __repr__(self) -> str:  # pragma: no cover
        return f"<Connection {self.role}:{self.peer_id}>"
