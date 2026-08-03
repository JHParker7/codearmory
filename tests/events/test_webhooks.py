"""Integration tests for the public webhook adapters and the event log.

Every endpoint here is reachable from the public internet and a matched trigger starts
pipeline runs, so the first thing each test class establishes is that an unsigned or
wrongly-signed payload is rejected outright.
"""
import json
import time
import uuid

import requests

from conftest import EVENTS_URL, post_signed, sign_webhook


# ── Signature enforcement ──────────────────────────────────────────────────────

def test_generic_hook_rejects_unsigned():
    res = requests.post(f"{EVENTS_URL}/hooks", json={"source": "ci/app", "event": "build.failed"})
    assert res.status_code == 401

def test_generic_hook_rejects_bad_signature():
    body = json.dumps({"source": "ci/app", "event": "build.failed"}).encode()
    res = requests.post(f"{EVENTS_URL}/hooks", data=body, headers={
        "Content-Type": "application/json",
        "X-Hub-Signature-256": "sha256=" + "0" * 64,
    })
    assert res.status_code == 401

def test_git_hook_rejects_unsigned():
    res = requests.post(f"{EVENTS_URL}/hooks/git", json={"repo": "a/b", "event": "push"})
    assert res.status_code == 401

def test_gitea_hook_rejects_unsigned():
    res = requests.post(f"{EVENTS_URL}/hooks/gitea",
                        json={"repository": {"full_name": "a/b"}},
                        headers={"X-Gitea-Event": "push"})
    assert res.status_code == 401

def test_signature_covers_the_body():
    """A payload signed and then altered must not be accepted."""
    body = json.dumps({"source": "ci/app", "event": "build.failed"}).encode()
    headers = sign_webhook(body)
    tampered = json.dumps({"source": "ci/other", "event": "build.failed"}).encode()
    res = requests.post(f"{EVENTS_URL}/hooks", data=tampered, headers=headers)
    assert res.status_code == 401


# ── Generic webhook ────────────────────────────────────────────────────────────

def test_generic_hook_requires_source(user_id):
    res = post_signed(f"/hooks?user_id={user_id}", {"event": "build.failed"})
    assert res.status_code == 400

def test_generic_hook_requires_event(user_id):
    res = post_signed(f"/hooks?user_id={user_id}", {"source": "ci/app"})
    assert res.status_code == 400

def test_generic_hook_requires_a_tenant():
    """With neither org_id nor user_id the event belongs to no one and is rejected."""
    res = post_signed("/hooks", {"source": "ci/app", "event": "build.failed"})
    assert res.status_code == 400

def test_generic_hook_accepts_and_namespaces_the_type(user_id, bearer):
    source = f"ci/app-{uuid.uuid4().hex[:6]}"
    res = post_signed(f"/hooks?user_id={user_id}", {
        "source": source,
        "event": "build.failed",
        "payload": {"branch": "main", "build": "42"},
    })
    assert res.status_code == 202, res.text

    ev = _await_event(bearer, lambda e: e["subject"] == source)
    # The "ci." prefix keeps a generic sender from minting a repo.push that would look like
    # it came from a verified git adapter.
    assert ev["type"] == "ci.build.failed"
    assert ev["source"] == source
    assert ev["data"]["branch"] == "main"

def test_generic_hook_header_overrides_body_event(user_id, bearer):
    source = f"ci/hdr-{uuid.uuid4().hex[:6]}"
    res = post_signed(f"/hooks?user_id={user_id}",
                      {"source": source, "event": "ignored"},
                      {"X-Hook-Event": "deploy.started"})
    assert res.status_code == 202
    ev = _await_event(bearer, lambda e: e["subject"] == source)
    assert ev["type"] == "ci.deploy.started"


# ── Normalized git webhook ─────────────────────────────────────────────────────

def test_git_hook_requires_repo_and_event(user_id):
    assert post_signed("/hooks/git", {"event": "push", "user_id": user_id}).status_code == 400
    assert post_signed("/hooks/git", {"repo": "a/b", "user_id": user_id}).status_code == 400

def test_git_hook_becomes_a_repo_event(user_id, bearer):
    repo = f"acme/app-{uuid.uuid4().hex[:6]}"
    res = post_signed("/hooks/git", {
        "repo": repo, "event": "push", "ref": "main",
        "commit": "abc123", "pusher": "alice", "message": "ship it",
        "user_id": user_id,
    })
    assert res.status_code == 202, res.text
    ev = _await_event(bearer, lambda e: e["subject"] == repo)
    assert ev["type"] == "repo.push"
    assert ev["data"]["ref"] == "main"
    assert ev["data"]["commit"] == "abc123"


# ── Forgejo / Gitea webhook ────────────────────────────────────────────────────

def test_gitea_ping_is_acknowledged(user_id):
    res = post_signed(f"/hooks/gitea?user_id={user_id}", {}, {"X-Gitea-Event": "ping"})
    assert res.status_code == 200

def test_gitea_requires_an_event_header(user_id):
    res = post_signed(f"/hooks/gitea?user_id={user_id}", {"repository": {"full_name": "a/b"}})
    assert res.status_code == 400

def test_gitea_untranslated_event_is_ignored(user_id):
    """An event type we do not translate is acknowledged so the provider does not retry."""
    res = post_signed(f"/hooks/gitea?user_id={user_id}",
                      {"repository": {"full_name": "a/b"}},
                      {"X-Gitea-Event": "release"})
    assert res.status_code == 200

def test_gitea_push_is_normalized(user_id, bearer):
    repo = f"forge/app-{uuid.uuid4().hex[:6]}"
    res = post_signed(f"/hooks/gitea?user_id={user_id}", {
        "ref": "refs/heads/main",
        "head_commit": {"id": "deadbeef", "message": "fix"},
        "pusher": {"username": "bob"},
        "repository": {"full_name": repo},
    }, {"X-Gitea-Event": "push"})
    assert res.status_code == 202, res.text

    ev = _await_event(bearer, lambda e: e["subject"] == repo)
    assert ev["type"] == "repo.push"
    assert ev["source"] == "gitea"
    # refs/heads/ is stripped so a filter compares against the bare branch name.
    assert ev["data"]["ref"] == "main"
    assert ev["data"]["commit"] == "deadbeef"
    assert ev["data"]["pusher"] == "bob"

def test_gitea_pull_request_carries_the_action(user_id, bearer):
    repo = f"forge/pr-{uuid.uuid4().hex[:6]}"
    res = post_signed(f"/hooks/gitea?user_id={user_id}", {
        "action": "opened",
        "pull_request": {
            "head": {"ref": "feature", "sha": "cafe01"},
            "user": {"username": "carol"},
            "title": "Add thing",
        },
        "repository": {"full_name": repo},
    }, {"X-Gitea-Event": "pull_request"})
    assert res.status_code == 202, res.text
    ev = _await_event(bearer, lambda e: e["subject"] == repo)
    assert ev["type"] == "repo.pull_request.opened"
    assert ev["data"]["ref"] == "feature"


# ── Event log ──────────────────────────────────────────────────────────────────

def test_list_events_requires_auth():
    assert requests.get(f"{EVENTS_URL}/events").status_code == 401

def test_get_event_requires_auth():
    assert requests.get(f"{EVENTS_URL}/events/does-not-matter").status_code == 401

def test_get_unknown_event_is_404(bearer):
    res = requests.get(f"{EVENTS_URL}/events/00000000-0000-0000-0000-000000000000", headers=bearer)
    assert res.status_code == 404

def test_list_events_filters_by_type(user_id, bearer):
    repo = f"acme/typed-{uuid.uuid4().hex[:6]}"
    post_signed("/hooks/git", {"repo": repo, "event": "push", "ref": "main", "user_id": user_id})
    _await_event(bearer, lambda e: e["subject"] == repo)

    res = requests.get(f"{EVENTS_URL}/events?type=repo.push", headers=bearer)
    assert res.status_code == 200
    assert all(e["type"] == "repo.push" for e in res.json())

def test_get_event_returns_the_envelope(user_id, bearer):
    repo = f"acme/detail-{uuid.uuid4().hex[:6]}"
    post_signed("/hooks/git", {
        "repo": repo, "event": "push", "ref": "main", "commit": "f00d", "user_id": user_id,
    })
    ev = _await_event(bearer, lambda e: e["subject"] == repo)

    res = requests.get(f"{EVENTS_URL}/events/{ev['id']}", headers=bearer)
    assert res.status_code == 200, res.text
    got = res.json()
    assert got["id"] == ev["id"]
    assert got["data"]["commit"] == "f00d"

def test_other_tenant_cannot_read_the_event(user_id, bearer, other_bearer):
    """A wrong-tenant id must be indistinguishable from a missing one."""
    repo = f"acme/private-{uuid.uuid4().hex[:6]}"
    post_signed("/hooks/git", {"repo": repo, "event": "push", "ref": "main", "user_id": user_id})
    ev = _await_event(bearer, lambda e: e["subject"] == repo)

    res = requests.get(f"{EVENTS_URL}/events/{ev['id']}", headers=other_bearer)
    assert res.status_code == 404


# ── Helpers ────────────────────────────────────────────────────────────────────

def _await_event(bearer, pred, timeout=10.0):
    """Poll the event log until an event satisfying pred appears.

    Ingestion stores synchronously but trigger evaluation is async, and the list is served
    from the read handle — so a freshly accepted event can lag the 202 by a moment.
    """
    deadline = time.time() + timeout
    while time.time() < deadline:
        res = requests.get(f"{EVENTS_URL}/events", headers=bearer)
        if res.status_code == 200:
            for e in res.json():
                if pred(e):
                    return e
        time.sleep(0.3)
    raise AssertionError("event never appeared in the log")
