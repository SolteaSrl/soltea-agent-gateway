"""Registro in RAM degli agenti e dei progetti che presidiano.

Niente DB: si ripopola alle registrazioni. Un agente dichiara i propri progetti
nel frame `hello`; teniamo l'indice inverso project_id -> agent_id per risolvere
velocemente "chi lavora su questo progetto".
"""
from __future__ import annotations

from dataclasses import dataclass, field

from .connection import Connection


@dataclass
class AgentEntry:
    agent_id: str
    conn: Connection
    projects: dict[int, str] = field(default_factory=dict)  # project_id -> nome
    runner_version: str = ""  # versione dichiarata dal runner nel frame hello


class Registry:
    def __init__(self) -> None:
        self._agents: dict[str, AgentEntry] = {}
        self._project_index: dict[int, str] = {}  # project_id -> agent_id

    def register(
        self, agent_id: str, conn: Connection, projects: list[dict], runner_version: str = ""
    ) -> AgentEntry:
        """Registra (o ri-registra) un agente e i suoi progetti.

        Se l'agent_id era gia' presente (riconnessione), rimpiazza la entry e
        riallinea l'indice dei progetti.
        """
        self._purge_agent(agent_id)
        proj_map: dict[int, str] = {}
        for p in projects:
            pid = int(p["project_id"])
            proj_map[pid] = str(p.get("name", ""))
            self._project_index[pid] = agent_id
        entry = AgentEntry(
            agent_id=agent_id, conn=conn, projects=proj_map, runner_version=str(runner_version or "")
        )
        self._agents[agent_id] = entry
        return entry

    def unregister(self, agent_id: str) -> None:
        self._purge_agent(agent_id)

    def _purge_agent(self, agent_id: str) -> None:
        old = self._agents.pop(agent_id, None)
        if old is None:
            return
        # Rimuovi dall'indice solo le voci che puntano ancora a questo agente.
        for pid in list(self._project_index):
            if self._project_index[pid] == agent_id:
                del self._project_index[pid]

    def agent_for_project(self, project_id: int) -> AgentEntry | None:
        agent_id = self._project_index.get(int(project_id))
        if agent_id is None:
            return None
        return self._agents.get(agent_id)

    def get(self, agent_id: str) -> AgentEntry | None:
        return self._agents.get(agent_id)

    def declares_project(self, agent_id: str, project_id: int) -> bool:
        entry = self._agents.get(agent_id)
        return bool(entry) and int(project_id) in entry.projects

    def snapshot(self) -> list[dict]:
        """Lista serializzabile degli agenti online (per list_agents/debug)."""
        return [
            {
                "agent_id": e.agent_id,
                "online": True,
                "projects": sorted(e.projects.keys()),
                "runner_version": e.runner_version,
            }
            for e in self._agents.values()
        ]
