"""Argo app sync: the full command/app-state round-trip with the test standing
in for the outpost, plus app discovery and auth."""
import time
import uuid

import requests
from conftest import ARGO_URL, poll_sync


def test_list_apps_unauthorized():
    res = requests.get(f"{ARGO_URL}/apps")
    assert res.status_code == 401


def test_sync_unknown_app_requires_outpost(bearer):
    # No outpost_id and no prior app-state for the app → 400.
    res = requests.post(f"{ARGO_URL}/apps/ghost/sync", headers=bearer, json={})
    assert res.status_code == 400


def test_full_sync_flow(bearer, outpost):
    app = f"guestbook-{uuid.uuid4().hex[:6]}"
    # Trigger a sync, passing the outpost explicitly (app not yet discovered).
    res = requests.post(f"{ARGO_URL}/apps/{app}/sync", headers=bearer,
                       json={"outpost_id": outpost.outpost_id, "revision": "HEAD"})
    assert res.status_code == 202, res.text
    sync_id = res.json()["sync_id"]
    assert res.json()["status"] == "pending"

    # The outpost receives a sync command for this app.
    cmd = outpost.wait_for_command("sync")
    assert cmd is not None, "no sync command delivered"
    assert cmd["payload"]["app_name"] == app

    # Outpost reports the operation started, then a healthy/synced state.
    outpost.post_event("argo", "sync-started", {"sync_id": sync_id})
    outpost.post_event("argo", "app-state", {
        "app_name": app, "sync_status": "Synced", "health_status": "Healthy",
        "operation_phase": "Succeeded", "revision": "abc123",
    })

    final = poll_sync(bearer, sync_id, until={"Synced", "Failed"})
    assert final is not None and final["status"] == "Synced", final


def test_failed_sync_flow(bearer, outpost):
    app = f"broken-{uuid.uuid4().hex[:6]}"
    res = requests.post(f"{ARGO_URL}/apps/{app}/sync", headers=bearer,
                       json={"outpost_id": outpost.outpost_id})
    sync_id = res.json()["sync_id"]
    outpost.wait_for_command("sync")
    outpost.post_event("argo", "app-state", {
        "app_name": app, "sync_status": "OutOfSync", "health_status": "Degraded",
        "operation_phase": "Failed",
    })
    final = poll_sync(bearer, sync_id, until={"Synced", "Failed"})
    assert final is not None and final["status"] == "Failed", final


def test_app_discovered_from_events(bearer, outpost):
    # An app-state event makes the app visible to its owner via GET /apps.
    app = f"discovered-{uuid.uuid4().hex[:6]}"
    outpost.post_event("argo", "app-state", {
        "app_name": app, "sync_status": "Synced", "health_status": "Healthy", "operation_phase": "",
    })
    # Allow the dispatcher to deliver.
    import time
    found = None
    for _ in range(20):
        apps = requests.get(f"{ARGO_URL}/apps", headers=bearer)
        assert apps.status_code == 200
        match = [a for a in apps.json() if a["name"] == app]
        if match:
            found = match[0]
            break
        time.sleep(1)
    assert found is not None, "app should be discoverable after an app-state event"
    assert found["sync_status"] == "Synced"

    got = requests.get(f"{ARGO_URL}/apps/{app}", headers=bearer)
    assert got.status_code == 200


def test_cross_tenant_isolation(bearer, other_bearer, outpost):
    # Owner creates a sync and discovers an app via an app-state event.
    app = f"isolated-{uuid.uuid4().hex[:6]}"
    res = requests.post(f"{ARGO_URL}/apps/{app}/sync", headers=bearer,
                       json={"outpost_id": outpost.outpost_id, "revision": "HEAD"})
    assert res.status_code == 202, res.text
    sync_id = res.json()["sync_id"]

    outpost.post_event("argo", "app-state", {
        "app_name": app, "sync_status": "Synced", "health_status": "Healthy",
        "operation_phase": "Succeeded", "revision": "abc123",
    })
    # Wait for the dispatcher to deliver so the app becomes visible to the owner.
    found = False
    for _ in range(20):
        apps = requests.get(f"{ARGO_URL}/apps", headers=bearer)
        if apps.status_code == 200 and any(a["name"] == app for a in apps.json()):
            found = True
            break
        time.sleep(1)
    assert found, "owner should see their own app before isolation checks"

    # A second tenant must not see the owner's sync, app, or app listing entry.
    got_sync = requests.get(f"{ARGO_URL}/syncs/{sync_id}", headers=other_bearer)
    assert got_sync.status_code == 404, "another user must not see the sync"

    got_app = requests.get(f"{ARGO_URL}/apps/{app}", headers=other_bearer)
    assert got_app.status_code == 404, "another user must not see the app"

    other_apps = requests.get(f"{ARGO_URL}/apps", headers=other_bearer)
    assert other_apps.status_code == 200
    assert not any(a["name"] == app for a in other_apps.json()), \
        "another user's app list must not include the owner's app"
