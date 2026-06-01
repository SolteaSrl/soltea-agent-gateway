"""Hub: gestisce le sessioni e instrada i frame tra orchestratrice e agente.

Una sessione lega un orchestratore e un agente attorno a un ticket. I frame
chat.*/task.* portano un session_id; l'hub li inoltra al capo opposto.
"""
from __future__ import annotations

import uuid
from dataclasses import dataclass

from .connection import Connection
from .registry import Registry


@dataclass
class Session:
    session_id: str
    orchestrator: Connection
    agent_id: str
    ticket_id: int | None
    project_id: int


class Hub:
    def __init__(self, registry: Registry) -> None:
        self.registry = registry
        self._sessions: dict[str, Session] = {}
        # Per pulire le sessioni quando una connessione cade.
        self._by_orchestrator: dict[str, set[str]] = {}
        self._by_agent: dict[str, set[str]] = {}

    def create_session(
        self,
        orchestrator: Connection,
        agent_id: str,
        project_id: int,
        ticket_id: int | None,
    ) -> Session:
        session_id = "sess-" + uuid.uuid4().hex[:16]
        sess = Session(session_id, orchestrator, agent_id, ticket_id, project_id)
        self._sessions[session_id] = sess
        self._by_orchestrator.setdefault(orchestrator.peer_id, set()).add(session_id)
        self._by_agent.setdefault(agent_id, set()).add(session_id)
        return sess

    def get(self, session_id: str) -> Session | None:
        return self._sessions.get(session_id)

    def close_session(self, session_id: str) -> None:
        sess = self._sessions.pop(session_id, None)
        if sess is None:
            return
        self._by_orchestrator.get(sess.orchestrator.peer_id, set()).discard(session_id)
        self._by_agent.get(sess.agent_id, set()).discard(session_id)

    def sessions_for_agent(self, agent_id: str) -> list[Session]:
        return [self._sessions[s] for s in self._by_agent.get(agent_id, set()) if s in self._sessions]

    def sessions_for_orchestrator(self, peer_id: str) -> list[Session]:
        return [self._sessions[s] for s in self._by_orchestrator.get(peer_id, set()) if s in self._sessions]

    def drop_connection(self, conn: Connection) -> list[Session]:
        """Chiude tutte le sessioni che toccano una connessione caduta.

        Ritorna le sessioni chiuse, cosi' il chiamante puo' avvisare l'altro capo.
        """
        affected: list[Session]
        if conn.role == "agent":
            affected = self.sessions_for_agent(conn.peer_id)
        else:
            affected = self.sessions_for_orchestrator(conn.peer_id)
        for sess in affected:
            self.close_session(sess.session_id)
        return affected
