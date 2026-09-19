"""End-to-end tests for the platform ingress — the single front door.

These exercise the routing a browser actually hits, which no service-level test covers:
the split between the web app and the API happens in nginx, not in any Go handler, so a
misconfigured Ingress is invisible to every other test in this tree and total to a user.

  /       → 308 redirect to /app
  /app/…  → portal   (prefix KEPT — the SPA is built with base=/app/)
  /api/…  → conductor (prefix STRIPPED — conductor's routes are /gatekeeper/… etc.)

The /app case is the subtle one and the reason these exist. Vite bakes the base path
into every asset URL at BUILD time, so serving the SPA under a sub-path is not something
an ingress rewrite can do on its own: index.html loads, then every bundle 404s because
the browser asks for the path the build was made with. The test therefore follows the
script tag and fetches the bundle, which is the only way to catch that.

Environment:
  INGRESS_URL  Origin the platform ingress answers on (default: http://localhost:8080)
  INGRESS_HOST Optional Host header, when the ingress is host-scoped rather than catch-all

Skipped entirely when INGRESS_URL is not serving, so the suite stays runnable against a
plain compose stack that has no ingress at all.
"""

import os
import re

import pytest
import requests

INGRESS_URL = os.getenv("INGRESS_URL", "http://localhost:8080").rstrip("/")
INGRESS_HOST = os.getenv("INGRESS_HOST", "")

_HEADERS = {"Host": INGRESS_HOST} if INGRESS_HOST else {}


@pytest.fixture(scope="module", autouse=True)
def ingress_available():
    try:
        res = requests.get(f"{INGRESS_URL}/app", headers=_HEADERS,
                           timeout=5, allow_redirects=False)
    except requests.RequestException as exc:
        pytest.skip(f"no platform ingress at {INGRESS_URL}: {exc}")
    if res.status_code == 404:
        pytest.skip(f"{INGRESS_URL} is serving, but /app is not routed — ingress disabled")


def test_root_redirects_to_the_app():
    res = requests.get(f"{INGRESS_URL}/", headers=_HEADERS,
                       allow_redirects=False, timeout=10)
    assert res.status_code in (307, 308), \
        f"/ answered {res.status_code}, want a permanent redirect"
    # 308 rather than 301/302 so a redirected POST keeps its method.
    assert res.headers.get("Location", "").endswith("/app"), \
        f"/ redirected to {res.headers.get('Location')!r}"


def test_app_serves_the_spa():
    res = requests.get(f"{INGRESS_URL}/app", headers=_HEADERS, timeout=10)
    assert res.status_code == 200, res.text[:200]
    assert "text/html" in res.headers.get("Content-Type", "")
    assert "<div id=\"root\"" in res.text or "<script" in res.text


def test_the_spa_bundle_actually_loads():
    """The regression a status-code check would miss.

    A base-path mismatch between the build and the runtime serves a perfectly good
    index.html whose every asset 404s — a blank page for the user, 200 OK for a naive
    test. So follow the script tag and fetch what it points at.
    """
    page = requests.get(f"{INGRESS_URL}/app", headers=_HEADERS, timeout=10)
    assert page.status_code == 200

    srcs = re.findall(r'<script[^>]+src="([^"]+)"', page.text)
    assert srcs, "the SPA shell references no script at all"
    bundle = next((s for s in srcs if s.endswith(".js")), srcs[0])

    # Built for the sub-path it is served from, or the browser will ask for the wrong URL.
    assert bundle.startswith("/app/"), \
        f"bundle URL {bundle!r} is not under /app — the SPA was built for a different base"

    res = requests.get(f"{INGRESS_URL}{bundle}", headers=_HEADERS, timeout=30)
    assert res.status_code == 200, f"{bundle} answered {res.status_code}"
    assert "javascript" in res.headers.get("Content-Type", ""), \
        f"{bundle} served as {res.headers.get('Content-Type')!r}"


def test_api_reaches_conductor_with_the_prefix_stripped():
    res = requests.get(f"{INGRESS_URL}/api/health", headers=_HEADERS, timeout=15)
    assert res.status_code == 200, res.text[:300]
    body = res.json()
    # Conductor's own health shape — proof the request reached conductor rather than
    # being answered by the portal's SPA fallback, which would also return 200.
    assert "services" in body, f"/api/health did not answer conductor's payload: {body}"
    assert body["services"], "conductor reported no services at all"


def test_api_routes_through_to_a_backend_service():
    """One hop further: conductor must actually route, not just answer for itself."""
    res = requests.get(f"{INGRESS_URL}/api/services", headers=_HEADERS, timeout=15)
    assert res.status_code == 200, res.text[:300]
    names = [s["name"] for s in res.json().get("services", [])]
    assert "gatekeeper" in names, f"registered services look wrong: {names}"


def test_unauthenticated_api_call_is_rejected_not_swallowed():
    """The API path must reach real auth — a 401 proves it, a 404 would mean bad routing."""
    res = requests.get(f"{INGRESS_URL}/api/codearmory_git_factory/repos",
                       headers=_HEADERS, timeout=15)
    assert res.status_code == 401, \
        f"an unauthenticated API call answered {res.status_code}, want 401"
