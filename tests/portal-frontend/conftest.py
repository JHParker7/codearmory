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
    # Conductor routes everything under a service-name prefix, so this is
    # /gatekeeper/signup, not /signup — the bare path is a 404.
    resp = requests.post(f"{api_url}/gatekeeper/signup", json=payload, timeout=10)
    assert resp.status_code == 201, f"fixture signup failed: {resp.status_code} {resp.text}"
    payload["user_id"] = resp.json()["user_id"]
    return payload


@pytest.fixture(scope="session")
def token(api_url: str, user: dict) -> str:
    """JWT for the session user."""
    resp = requests.post(
        f"{api_url}/gatekeeper/login",
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


def _portal_login(request_ctx, portal_url: str, user: dict) -> str:
    """Log in through the PORTAL's own origin and return the user id.

    Going through `{portal_url}/api/gatekeeper/login` rather than straight to
    conductor is the whole point: gatekeeper sets the `armory_session` cookie on
    whatever origin served the response, and the SPA only ever calls same-origin
    `/api/*`. A cookie set on the conductor origin would never be sent by the app.

    The returned id is what the SPA persists (`ca_uid`); the token in the response
    body is deliberately NOT stored anywhere — the cookie is the credential.
    """
    resp = request_ctx.post(
        f"{portal_url}/api/gatekeeper/login",
        data={"email": user["email"], "password": user["password"]},
    )
    assert resp.ok, f"portal-origin login failed: {resp.status} {resp.text()}"
    return user["user_id"]


@pytest.fixture
def pw_auth_page(page, portal_url, user):
    """Playwright page holding a real, cookie-backed session.

    Authenticates the way the app does: log in over the portal origin so the
    HttpOnly `armory_session` cookie lands in the browser context, then seed
    `ca_uid` so a load resumes the session. Nothing secret is written to storage —
    injecting a token into localStorage (as this fixture used to) authenticates
    nothing now, because the app ignores and purges that key.
    """
    page.goto(portal_url)
    user_id = _portal_login(page.request, portal_url, user)
    page.evaluate(f"localStorage.setItem('ca_uid', {json.dumps(user_id)})")
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
def sel_auth_driver(portal_url, api_url, user):
    """Selenium Chrome driver holding a real, cookie-backed session.

    Logs in through the browser's own form so the `armory_session` cookie is set by
    the server exactly as it is in production, then seeds `ca_uid` on every new
    document (via CDP) so a reload resumes rather than bouncing to /login.

    The seeding is CDP-based for the same reason it always was: it has to run before
    React reads storage, including after a full page reload.
    """
    driver = _chrome_driver()
    driver.get(f"{portal_url}/login")

    # Real form login — this is what puts the HttpOnly cookie in the browser. It
    # cannot be injected the way a localStorage token could: JS cannot write an
    # HttpOnly cookie, which is precisely why the app moved to one.
    WebDriverWait(driver, 20).until(
        EC.presence_of_element_located((By.CSS_SELECTOR, "input[type='email']"))
    ).send_keys(user["email"])
    driver.find_element(By.CSS_SELECTOR, "input[type='password']").send_keys(user["password"])
    driver.find_element(By.CSS_SELECTOR, "button[type='submit']").click()
    WebDriverWait(driver, 20).until(lambda d: "/login" not in d.current_url)

    driver.execute_cdp_cmd(
        "Page.addScriptToEvaluateOnNewDocument",
        {"source": f"localStorage.setItem('ca_uid', {json.dumps(user['user_id'])})"},
    )
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
