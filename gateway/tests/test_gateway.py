"""Test end-to-end del gateway con il TestClient di Starlette.

Simula un agente e un'orchestratrice connessi insieme e verifica registry,
risoluzione progetto, blob roundtrip e l'intero giro task.start -> chat -> done.
"""
from __future__ import annotations

from pathlib import Path

import pytest
from fastapi.testclient import TestClient

from soltea_gateway.config import Config
from soltea_gateway.server import create_app


@pytest.fixture()
def client(tmp_path: Path) -> TestClient:
    cfg = Config(
        orchestrator_token="orch-tok",
        shared_agent_token="agent-tok",
        blob_dir=tmp_path / "blobs",
        heartbeat_seconds=30,
    )
    app = create_app(cfg)
    return TestClient(app)


def _hello_agent(ws, agent_id="win-dev-01", project_id=1234):
    ws.send_json({
        "type": "hello", "role": "agent", "token": "agent-tok", "agent_id": agent_id,
        "projects": [{"project_id": project_id, "name": "Proj X", "path": "C:/dev/x"}],
    })
    return ws.receive_json()


def _hello_orch(ws):
    ws.send_json({"type": "hello", "role": "orchestrator", "token": "orch-tok", "client_id": "claudia"})
    return ws.receive_json()


def test_healthz(client: TestClient):
    assert client.get("/healthz").json()["status"] == "ok"


def test_agent_bad_token_rejected(client: TestClient):
    with client.websocket_connect("/ws") as ws:
        ws.send_json({"type": "hello", "role": "agent", "token": "nope", "agent_id": "x", "projects": []})
        frame = ws.receive_json()
        assert frame["type"] == "error"
        assert frame["code"] == "unauthorized"


def test_blob_roundtrip(client: TestClient):
    r = client.post("/blobs", content=b"PK\x03\x04zip", headers={"Authorization": "Bearer orch-tok"})
    assert r.status_code == 200
    blob_id = r.json()["blob_id"]
    g = client.get(f"/blobs/{blob_id}", headers={"Authorization": "Bearer agent-tok"})
    assert g.status_code == 200
    assert g.content == b"PK\x03\x04zip"


def test_blob_requires_auth(client: TestClient):
    assert client.post("/blobs", content=b"x").status_code == 401


def test_resolve_and_full_task_flow(client: TestClient):
    with client.websocket_connect("/ws") as agent, client.websocket_connect("/ws") as orch:
        wa = _hello_agent(agent)
        assert wa["type"] == "welcome" and wa["registered_projects"] == [1234]
        wo = _hello_orch(orch)
        assert wo["type"] == "welcome"

        # Discovery
        orch.send_json({"type": "resolve_project", "req_id": "r1", "project_id": 1234})
        res = orch.receive_json()
        assert res["type"] == "project_resolved" and res["agent_id"] == "win-dev-01" and res["online"]

        # Upload zip ticket
        blob_id = client.post(
            "/blobs", content=b"PK\x03\x04ticket", headers={"Authorization": "Bearer orch-tok"}
        ).json()["blob_id"]

        # task.start
        orch.send_json({
            "type": "task.start", "req_id": "t1", "project_id": 1234, "ticket_id": 5678,
            "blob_id": blob_id, "instructions": "Risolvi.",
        })
        # L'agente riceve il task con il session_id assegnato
        at = agent.receive_json()
        assert at["type"] == "task.start" and at["ticket_id"] == 5678 and at["blob_id"] == blob_id
        session_id = at["session_id"]
        # L'orchestratrice riceve la conferma di apertura sessione
        so = orch.receive_json()
        assert so["type"] == "session_opened" and so["session_id"] == session_id

        # L'agente avvia claude e ristreamma
        agent.send_json({"type": "task.started", "session_id": session_id, "ticket_id": 5678,
                         "claude_session_id": "cs-1", "workdir": "C:/dev/x/.tickets/5678"})
        assert orch.receive_json()["type"] == "task.started"
        agent.send_json({"type": "chat.result", "session_id": session_id, "text": "Fatto.", "is_error": False})
        r = orch.receive_json()
        assert r["type"] == "chat.result" and r["text"] == "Fatto."

        # Secondo turno
        orch.send_json({"type": "chat.send", "session_id": session_id, "text": "Aggiungi un test."})
        assert agent.receive_json()["text"] == "Aggiungi un test."
        agent.send_json({"type": "chat.result", "session_id": session_id, "text": "Test aggiunto."})
        assert orch.receive_json()["text"] == "Test aggiunto."

        # Chiusura
        orch.send_json({"type": "task.done", "session_id": session_id, "outcome": "resolved"})
        assert agent.receive_json()["type"] == "task.done"


def test_task_start_no_agent(client: TestClient):
    with client.websocket_connect("/ws") as orch:
        _hello_orch(orch)
        orch.send_json({"type": "task.start", "req_id": "t1", "project_id": 9999,
                        "ticket_id": 1, "instructions": ""})
        err = orch.receive_json()
        assert err["type"] == "error" and err["code"] == "no_agent_for_project"
