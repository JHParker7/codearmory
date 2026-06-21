"""
Fixtures for the application (portal-app) frontend browser integration tests:
login, signup, and the authenticated /app/* pages. The marketing home page is a
separate deployment, tested under tests/portal-www-frontend/. The app root (/)
redirects to /login when unauthenticated.

Requires a running stack:
  PORTAL_URL  http://localhost:3001   the application SPA + BFF (Vite dev or nginx)
  API_URL     http://localhost:8082   conductor backend

Run:
  pytest tests/portal-frontend/ -v
  pytest tests/portal-frontend/ -v -m playwright
  pytest tests/portal-frontend/ -v -m selenium
"""

import os
import uuid
import json

import pytest
import requests
from selenium import webdriver
from selenium.webdriver.chrome.options import Options as ChromeOptions
from selenium.webdriver.support.ui import WebDriverWait
from selenium.webdriver.support import expected_conditions as EC
from selenium.webdriver.common.by import By


# ---------------------------------------------------------------------------
# URLs and real-stack fixtures (shared with tests/portal/)
# ---------------------------------------------------------------------------

@pytest.fixture(scope="session")
def portal_url() -> str:
    return os.getenv("PORTAL_URL", "http://localhost:3001")


@pytest.fixture(scope="session")
def api_url() -> str:
    return os.getenv("API_URL", "http://localhost:8082")


@pytest.fixture(scope="session")
def user(api_url: str) -> dict:
    """Create a unique test user via conductor; reused across the session."""
    uid = uuid.uuid4().hex[:8]
    payload = {
        "email": f"fe_test_{uid}@example.com",
        "username": f"fe_{uid}",
        "password": "TestPassword1!",
    }
    resp = requests.post(f"{api_url}/signup", json=payload, timeout=10)
    assert resp.status_code == 201, f"fixture signup failed: {resp.status_code} {resp.text}"
    payload["user_id"] = resp.json()["user_id"]
    return payload


@pytest.fixture(scope="session")
def token(api_url: str, user: dict) -> str:
    """JWT for the session user."""
    resp = requests.post(
        f"{api_url}/login",
        json={"email": user["email"], "password": user["password"]},
        timeout=10,
    )
    assert resp.status_code == 200, f"fixture login failed: {resp.status_code} {resp.text}"
    return resp.json()["token"]


# ---------------------------------------------------------------------------
# Playwright fixtures
# ---------------------------------------------------------------------------

@pytest.fixture
def pw_page(page, portal_url):
    """Playwright page pointed at the portal root (unauthenticated)."""
    page.goto(portal_url)
    return page


@pytest.fixture
def pw_auth_page(page, portal_url, token):
    """Playwright page with the auth token pre-loaded in localStorage.

    Navigates to the portal root first (establishes the origin), injects the
    token, then returns the page for the test to drive to a specific route.
    """
    page.goto(portal_url)
    page.evaluate(f"localStorage.setItem('ca_token', {json.dumps(token)})")
    return page


# ---------------------------------------------------------------------------
# Selenium fixtures
# ---------------------------------------------------------------------------

def _chrome_driver() -> webdriver.Chrome:
    opts = ChromeOptions()
    opts.add_argument("--headless=new")
    opts.add_argument("--no-sandbox")
    opts.add_argument("--disable-dev-shm-usage")
    opts.add_argument("--disable-gpu")
    opts.add_argument("--window-size=1280,900")
    driver = webdriver.Chrome(options=opts)
    driver.set_page_load_timeout(30)
    driver.implicitly_wait(10)
    return driver


@pytest.fixture
def sel_driver(portal_url):
    """Unauthenticated Selenium Chrome driver."""
    driver = _chrome_driver()
    driver.get(portal_url)
    yield driver
    driver.quit()


@pytest.fixture
def sel_auth_driver(portal_url, token):
    """Selenium Chrome driver with auth token injected via CDP.

    Using addScriptToEvaluateOnNewDocument ensures the token is set before
    React's useEffect reads localStorage, even after client-side navigations
    that cause a full page reload.
    """
    driver = _chrome_driver()

    # Inject token into localStorage on every new document (before React runs).
    driver.execute_cdp_cmd(
        "Page.addScriptToEvaluateOnNewDocument",
        {"source": f"localStorage.setItem('ca_token', {json.dumps(token)})"},
    )

    driver.get(portal_url)
    yield driver
    driver.quit()


def sel_wait(driver, timeout=15):
    """Return a WebDriverWait instance for the given driver."""
    return WebDriverWait(driver, timeout)


def wait_for_url(driver, fragment: str, timeout=15):
    """Block until `fragment` appears in the browser URL."""
    WebDriverWait(driver, timeout).until(
        lambda d: fragment in d.current_url
    )
