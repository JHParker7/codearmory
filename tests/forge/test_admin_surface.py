"""Integration tests for Forge's admin/security surface: the image allowlist,
runner-class & runtime-backend read endpoints, and the create-authz boundary.

These cover gaps the execution tests don't: that disallowed images are rejected
(sandbox image policy), that the catalog endpoints work for ordinary users, and
that mutating the runtime config requires admin permission a signup user lacks.
"""
import uuid

import requests

from conftest import FORGE_URL


# ── Image allowlist (sandbox image policy) ─────────────────────────────────────

def test_images_endpoint_lists_allowlist(bearer):
    res = requests.get(f"{FORGE_URL}/images", headers=bearer)
    assert res.status_code == 200, res.text
    images = res.json()
    assert isinstance(images, list)
    # compose configures ALLOWED_IMAGES=alpine:3.19,ubuntu:22.04,python:3.12-slim,golang:1.25
    assert "alpine:3.19" in images


def test_submit_disallowed_image_rejected(bearer):
    res = requests.post(f"{FORGE_URL}/executions", headers=bearer, json={
        "image": "nginx:latest",  # not in the allowlist
        "command": ["echo", "hi"],
        "timeout": 30,
    })
    assert res.status_code == 400, f"expected 400 for disallowed image, got {res.status_code}: {res.text}"
    assert "image not allowed" in res.text.lower()


def test_submit_allowed_image_accepted(bearer):
    res = requests.post(f"{FORGE_URL}/executions", headers=bearer, json={
        "image": "alpine:3.19",
        "command": ["true"],
        "timeout": 30,
    })
    assert res.status_code == 202, res.text


# ── Runner-class catalog (read paths granted to ordinary users) ────────────────

def test_list_runner_classes(bearer):
    res = requests.get(f"{FORGE_URL}/runner-classes", headers=bearer)
    assert res.status_code == 200, res.text
    assert isinstance(res.json(), list)


def test_get_runner_class_not_found(bearer):
    res = requests.get(f"{FORGE_URL}/runner-classes/does-not-exist-{uuid.uuid4().hex[:6]}", headers=bearer)
    assert res.status_code == 404


def test_list_runner_classes_unauthorized():
    assert requests.get(f"{FORGE_URL}/runner-classes").status_code == 401


# ── Runtime-backend catalog ────────────────────────────────────────────────────

def test_list_runtime_backends(bearer):
    res = requests.get(f"{FORGE_URL}/runtime-backends", headers=bearer)
    assert res.status_code == 200, res.text
    assert isinstance(res.json(), list)


def test_get_runtime_backend_not_found(bearer):
    res = requests.get(f"{FORGE_URL}/runtime-backends/does-not-exist-{uuid.uuid4().hex[:6]}", headers=bearer)
    assert res.status_code == 404


# ── Create-authz boundary: mutating runtime config needs admin ─────────────────

def test_create_runner_class_forbidden_for_regular_user(bearer):
    res = requests.post(f"{FORGE_URL}/runner-classes", headers=bearer, json={
        "name": "rc-" + uuid.uuid4().hex[:6], "memory_mb": 256, "cpu_millicores": 500,
    })
    # A signup user holds the default forge grants (createExecution + list/get) but
    # NOT createRunnerClass, so this must be denied, not created.
    assert res.status_code in (401, 403), f"expected authz denial, got {res.status_code}: {res.text}"


def test_create_runtime_backend_forbidden_for_regular_user(bearer):
    res = requests.post(f"{FORGE_URL}/runtime-backends", headers=bearer, json={
        "name": "rb-" + uuid.uuid4().hex[:6], "type": "docker", "enabled": True,
    })
    assert res.status_code in (401, 403), f"expected authz denial, got {res.status_code}: {res.text}"


def test_create_runner_class_unauthorized():
    res = requests.post(f"{FORGE_URL}/runner-classes", json={"name": "x", "memory_mb": 1, "cpu_millicores": 1})
    assert res.status_code == 401
