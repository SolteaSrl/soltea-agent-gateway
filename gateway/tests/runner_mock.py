"""MockRunner: fake agent-runner in-process per i test E2E del gateway.

Si connette all'endpoint /ws come "agent", risponde ai ping di sistema, e
mette in scena un task riproducendo il protocollo come farebbe l'agent-runner
Go: `task.start` -> `task.started` -> N `chat.delta` -> `chat.result`,
poi `task.done` chiude.

Usata dai test E2E per verificare il path completo orch <-> gateway <-> runner
senza dover accendere una VM Windows e senza dipendere da claude.exe.

Non usa thread: ogni metodo guida esplicitamente il flusso (i test sono
sincroni col TestClient di Starlette, che si aspetta send/receive interleaved).
"""
from __future__ import annotations

from dataclasses import dataclass


DEFAULT_AGENT_ID = "mock-runner-01"
DEFAULT_PROJECT_ID = 4242
DEFAULT_RUNNER_VERSION = "0.5.0"


@dataclass
class MockRunner:
    """Wrapper su una connessione ws di test che imita un agent-runner.

    Esempio d'uso:
        with client.websocket_connect("/ws") as agent_ws:
            mock = MockRunner(agent_ws)
            mock.hello()
            task = mock.wait_for_task_start()
            mock.send_task_started(task["session_id"], task.get("ticket_id"))
            mock.send_delta(task["session_id"], 1, "Sto leggendo... ")
            mock.send_delta(task["session_id"], 2, "Trovato. ")
            mock.send_result(task["session_id"], "Fatto.", is_error=False)
    """

    ws: object  # WebSocketTestSession di Starlette
    agent_id: str = DEFAULT_AGENT_ID
    project_id: int = DEFAULT_PROJECT_ID
    runner_version: str = DEFAULT_RUNNER_VERSION
    token: str = "agent-tok"

    def hello(self) -> dict:
        """Manda il frame hello e ritorna il welcome del gateway."""
        self.ws.send_json({
            "type": "hello", "role": "agent", "token": self.token,
            "agent_id": self.agent_id, "runner_version": self.runner_version,
            "protocol_version": 1,
            "projects": [{"project_id": self.project_id, "name": "Mock", "path": "/mock"}],
        })
        return self.ws.receive_json()

    def wait_for_task_start(self) -> dict:
        """Aspetta il prossimo `task.start` ignorando ping/pong/altro."""
        while True:
            frame = self.ws.receive_json()
            if frame.get("type") == "task.start":
                return frame
            # Eventuali altri frame (ping interno o errori) restano fuori scope
            # del runner-mock: in test E2E gli interferenti non dovrebbero esserci.

    def wait_for_chat_send(self) -> dict:
        while True:
            frame = self.ws.receive_json()
            if frame.get("type") == "chat.send":
                return frame

    def wait_for_task_done(self) -> dict:
        while True:
            frame = self.ws.receive_json()
            if frame.get("type") == "task.done":
                return frame

    # ----- frame in uscita (verso il gateway) -----

    def send_task_started(self, session_id: str, ticket_id: int | None = None,
                          claude_session_id: str = "cs-mock", workdir: str = "/mock/workdir") -> None:
        self.ws.send_json({
            "type": "task.started", "session_id": session_id, "ticket_id": ticket_id,
            "claude_session_id": claude_session_id, "workdir": workdir,
        })

    def send_delta(self, session_id: str, seq: int, text: str) -> None:
        self.ws.send_json({"type": "chat.delta", "session_id": session_id, "seq": seq, "text": text})

    def send_result(self, session_id: str, text: str, *,
                    is_error: bool = False, cost_usd: float = 0.0, duration_ms: int = 0,
                    claude_session_id: str = "cs-mock") -> None:
        self.ws.send_json({
            "type": "chat.result", "session_id": session_id, "text": text,
            "is_error": is_error, "cost_usd": cost_usd, "duration_ms": duration_ms,
            "claude_session_id": claude_session_id, "runner_version": self.runner_version,
        })

    def send_error(self, session_id: str, code: str, message: str) -> None:
        self.ws.send_json({"type": "error", "session_id": session_id, "code": code, "message": message})


@dataclass
class MockOrchestrator:
    """Helper speculare per la parte orchestratrice nei test E2E."""

    ws: object
    client_id: str = "claudia-test"
    token: str = "orch-tok"

    def hello(self) -> dict:
        self.ws.send_json({"type": "hello", "role": "orchestrator",
                           "token": self.token, "client_id": self.client_id})
        return self.ws.receive_json()

    def start_task(self, project_id: int, ticket_id: int | None = None,
                   instructions: str = "Risolvi.", blob_id: str | None = None,
                   req_id: str = "t1") -> None:
        frame = {"type": "task.start", "req_id": req_id, "project_id": project_id,
                 "ticket_id": ticket_id, "instructions": instructions}
        if blob_id:
            frame["blob_id"] = blob_id
        self.ws.send_json(frame)

    def chat_send(self, session_id: str, text: str) -> None:
        self.ws.send_json({"type": "chat.send", "session_id": session_id, "text": text})

    def task_done(self, session_id: str) -> None:
        self.ws.send_json({"type": "task.done", "session_id": session_id})

    def attach(self, session_id: str, req_id: str = "a1") -> None:
        self.ws.send_json({"type": "session_attach", "req_id": req_id, "session_id": session_id})

    def expect(self, frame_type: str) -> dict:
        """Aspetta uno specifico tipo di frame; salta i frame non interessanti."""
        while True:
            frame = self.ws.receive_json()
            if frame.get("type") == frame_type:
                return frame
            # Se arriva un errore inatteso quando ci si aspetta altro, fail-loud.
            if frame.get("type") == "error":
                raise AssertionError(f"errore inatteso dal gateway: {frame}")

    def expect_in(self, *types: str) -> dict:
        """Aspetta il prossimo frame fra una lista di tipi attesi."""
        wanted = set(types)
        while True:
            frame = self.ws.receive_json()
            if frame.get("type") in wanted:
                return frame
            if frame.get("type") == "error":
                raise AssertionError(f"errore inatteso dal gateway: {frame}")
