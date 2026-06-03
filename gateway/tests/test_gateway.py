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


def _hello_agent(ws, agent_id="win-dev-01", project_id=1234, runner_version="0.4.0"):
    ws.send_json({
        "type": "hello", "role": "agent", "token": "agent-tok", "agent_id": agent_id,
        "projects": [{"project_id": project_id, "name": "Proj X", "path": "C:/dev/x"}],
        "runner_version": runner_version,
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


def _prov_app(tmp_path: Path):
    cfg = Config(
        orchestrator_token="orch-tok",
        agent_tokens={"runner-env-01": "env-secret"},
        agent_tokens_file=tmp_path / "agent_tokens.json",
        blob_dir=tmp_path / "blobs",
    )
    return cfg, create_app(cfg)


def test_provision_requires_orch_token(tmp_path: Path):
    _cfg, app = _prov_app(tmp_path)
    client = TestClient(app)
    assert client.post("/provision", json={"agent_id": "runner-x"}).status_code == 401
    assert client.post("/revoke", json={"agent_id": "runner-x"}).status_code == 401


def test_provision_creates_working_token(tmp_path: Path):
    _cfg, app = _prov_app(tmp_path)
    client = TestClient(app)
    r = client.post("/provision", json={"agent_id": "runner-pcmarcello-01"},
                    headers={"Authorization": "Bearer orch-tok"})
    assert r.status_code == 200
    token = r.json()["token"]
    assert r.json()["agent_id"] == "runner-pcmarcello-01" and token

    # Il token appena creato autentica davvero un agente sulla WS.
    with client.websocket_connect("/ws") as ws:
        ws.send_json({"type": "hello", "role": "agent", "token": token,
                      "agent_id": "runner-pcmarcello-01", "projects": []})
        assert ws.receive_json()["type"] == "welcome"


def test_provision_autogenerates_agent_id(tmp_path: Path):
    _cfg, app = _prov_app(tmp_path)
    client = TestClient(app)
    r = client.post("/provision", json={}, headers={"Authorization": "Bearer orch-tok"})
    assert r.status_code == 200 and r.json()["agent_id"].startswith("runner-")


def test_provision_conflict_and_rotate(tmp_path: Path):
    _cfg, app = _prov_app(tmp_path)
    client = TestClient(app)
    h = {"Authorization": "Bearer orch-tok"}
    first = client.post("/provision", json={"agent_id": "runner-dup"}, headers=h).json()["token"]
    # Stesso id senza rotate -> 409
    assert client.post("/provision", json={"agent_id": "runner-dup"}, headers=h).status_code == 409
    # Con rotate -> nuovo token diverso
    rot = client.post("/provision", json={"agent_id": "runner-dup", "rotate": True}, headers=h)
    assert rot.status_code == 200 and rot.json()["token"] != first


def test_provision_blocks_env_id(tmp_path: Path):
    _cfg, app = _prov_app(tmp_path)
    client = TestClient(app)
    h = {"Authorization": "Bearer orch-tok"}
    assert client.post("/provision", json={"agent_id": "runner-env-01"}, headers=h).status_code == 409
    assert client.post("/revoke", json={"agent_id": "runner-env-01"}, headers=h).status_code == 409


def test_provision_persists_across_restart(tmp_path: Path):
    cfg1, app1 = _prov_app(tmp_path)
    token = TestClient(app1).post(
        "/provision", json={"agent_id": "runner-keep"}, headers={"Authorization": "Bearer orch-tok"}
    ).json()["token"]
    # Nuova istanza dell'app (= restart) sullo stesso file: il token resta valido.
    _cfg2, app2 = _prov_app(tmp_path)
    client2 = TestClient(app2)
    with client2.websocket_connect("/ws") as ws:
        ws.send_json({"type": "hello", "role": "agent", "token": token,
                      "agent_id": "runner-keep", "projects": []})
        assert ws.receive_json()["type"] == "welcome"


def test_revoke_invalidates_token(tmp_path: Path):
    _cfg, app = _prov_app(tmp_path)
    client = TestClient(app)
    h = {"Authorization": "Bearer orch-tok"}
    token = client.post("/provision", json={"agent_id": "runner-tmp"}, headers=h).json()["token"]
    r = client.post("/revoke", json={"agent_id": "runner-tmp"}, headers=h)
    assert r.status_code == 200 and r.json()["revoked"] is True
    with client.websocket_connect("/ws") as ws:
        ws.send_json({"type": "hello", "role": "agent", "token": token,
                      "agent_id": "runner-tmp", "projects": []})
        frame = ws.receive_json()
        assert frame["type"] == "error" and frame["code"] == "unauthorized"


def test_list_agents_reports_runner_version(client: TestClient):
    with client.websocket_connect("/ws") as agent, client.websocket_connect("/ws") as orch:
        _hello_agent(agent, runner_version="0.4.0")
        _hello_orch(orch)
        orch.send_json({"type": "list_agents", "req_id": "la1"})
        res = orch.receive_json()
        assert res["type"] == "agents"
        assert res["agents"][0]["agent_id"] == "win-dev-01"
        assert res["agents"][0]["runner_version"] == "0.4.0"
