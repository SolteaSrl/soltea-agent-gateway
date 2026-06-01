"""Costanti e helper per i frame del protocollo (vedi docs/PROTOCOL.md)."""
from __future__ import annotations

from typing import Any

# --- tipi di frame ---
HELLO = "hello"
WELCOME = "welcome"
PING = "ping"
PONG = "pong"
RESOLVE_PROJECT = "resolve_project"
PROJECT_RESOLVED = "project_resolved"
LIST_AGENTS = "list_agents"
AGENTS = "agents"
TASK_START = "task.start"
TASK_STARTED = "task.started"
CHAT_SEND = "chat.send"
CHAT_DELTA = "chat.delta"
CHAT_RESULT = "chat.result"
TASK_DONE = "task.done"
ERROR = "error"

ROLE_AGENT = "agent"
ROLE_ORCHESTRATOR = "orchestrator"

# --- codici d'errore ---
ERR_UNAUTHORIZED = "unauthorized"
ERR_BAD_HELLO = "bad_hello"
ERR_NO_AGENT = "no_agent_for_project"
ERR_PROJECT_NOT_DECLARED = "project_not_declared"
ERR_UNKNOWN_SESSION = "unknown_session"
ERR_BLOB_NOT_FOUND = "blob_not_found"
ERR_CLAUDE_FAILED = "claude_failed"
ERR_INTERNAL = "internal"


def error(code: str, message: str, **extra: Any) -> dict[str, Any]:
    frame = {"type": ERROR, "code": code, "message": message}
    frame.update({k: v for k, v in extra.items() if v is not None})
    return frame
