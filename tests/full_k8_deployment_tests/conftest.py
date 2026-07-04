"""
Fixtures for the full-Kubernetes-deployment portal E2E tests.

These tests drive a **real, running codearmory deployment** through the browser —
the Kubernetes runtime with the **kata** sandbox backend (the platform's primary
runtime). They create an account, build a workflow in the portal's visual
builder, trigger it, approve its gate and watch it complete, all via Selenium.

Point them at a deployment with:

  PORTAL_URL   base URL of the portal (SPA + BFF).   default http://localhost:3001
               e.g. `kubectl -n codearmory port-forward svc/<release>-portal 3001:3001`
               then PORTAL_URL=http://localhost:3001, or the ingress host.

Other knobs (all optional):

  HEADLESS            "1"/"true" (default) run Chrome headless; "0" to watch it.
  FORGE_TEST_IMAGE    container image the forge/run steps use. default alpine:3.19
                      Must be permitted by the deployment's forge ALLOWED_IMAGES.
  FORGE_TEST_VOLUME_MB shared-volume size request (Mi). default 128
  FORGE_TEST_MEDIUM   volume medium ("disk"/"memory"; a no-op on k8s — both are
                      PVCs). default disk
  E2E_ARTIFACTS_DIR   where to drop screenshots/HTML on failure.
                      default $CLAUDE_JOB_DIR/tmp or ./_artifacts
  CHROME_BIN          explicit Chrome/Chromium binary (for containers).
  CHROMEDRIVER        explicit chromedriver path (else Selenium Manager resolves).

Run (stack must be reachable):
  pip install -r tests/full_k8_deployment_tests/requirements.txt
  pytest tests/full_k8_deployment_tests -v
"""

import os
import uuid

import pytest
from selenium import webdriver
from selenium.webdriver.chrome.options import Options as ChromeOptions
from selenium.webdriver.chrome.service import Service as ChromeService


def _bool_env(name: str, default: bool) -> bool:
    v = os.getenv(name)
    if v is None:
        return default
    return v.strip().lower() in ("1", "true", "yes", "on")


@pytest.fixture(scope="session")
def portal_url() -> str:
    return os.getenv("PORTAL_URL", "http://localhost:3001").rstrip("/")


@pytest.fixture(scope="session")
def forge_image() -> str:
    return os.getenv("FORGE_TEST_IMAGE", "alpine:3.19")


@pytest.fixture(scope="session")
def volume_size_mb() -> int:
    return int(os.getenv("FORGE_TEST_VOLUME_MB", "128"))


@pytest.fixture(scope="session")
def volume_medium() -> str:
    return os.getenv("FORGE_TEST_MEDIUM", "disk")


@pytest.fixture(scope="session")
def artifacts_dir() -> str:
    default = os.path.join(os.getenv("CLAUDE_JOB_DIR", "."), "tmp") \
        if os.getenv("CLAUDE_JOB_DIR") else "_artifacts"
    d = os.getenv("E2E_ARTIFACTS_DIR", default)
    os.makedirs(d, exist_ok=True)
    return d


@pytest.fixture
def creds() -> dict:
    """A unique, valid account for one test (strong password so signup's strength
    gate passes; username matches /^[a-z0-9_-]{1,64}$/)."""
    uid = uuid.uuid4().hex[:10]
    return {
        "email": f"k8s_e2e_{uid}@example.com",
        "username": f"k8s_e2e_{uid}",
        "password": "TestPassword1!",
    }


@pytest.fixture
def driver(artifacts_dir):
    opts = ChromeOptions()
    if _bool_env("HEADLESS", True):
        opts.add_argument("--headless=new")
    opts.add_argument("--no-sandbox")
    opts.add_argument("--disable-dev-shm-usage")
    opts.add_argument("--disable-gpu")
    opts.add_argument("--window-size=1600,1100")
    if os.getenv("CHROME_BIN"):
        opts.binary_location = os.environ["CHROME_BIN"]

    driver_path = os.getenv("CHROMEDRIVER")
    if not driver_path and os.path.exists("/usr/bin/chromedriver"):
        driver_path = "/usr/bin/chromedriver"
    service = ChromeService(executable_path=driver_path) if driver_path else ChromeService()

    drv = webdriver.Chrome(options=opts, service=service)
    drv.set_page_load_timeout(45)
    # Rely on explicit WebDriverWaits only — an implicit wait would slow every
    # "is this gone?" check by its full duration.
    drv.implicitly_wait(0)
    yield drv
    drv.quit()


@pytest.hookimpl(hookwrapper=True)
def pytest_runtest_makereport(item, call):
    outcome = yield
    rep = outcome.get_result()
    setattr(item, f"rep_{rep.when}", rep)


@pytest.fixture(autouse=True)
def _capture_on_failure(request, artifacts_dir):
    """Dump a screenshot + page HTML for any test that fails, to make a browser
    failure debuggable after the fact."""
    yield
    rep = getattr(request.node, "rep_call", None)
    if rep is None or not rep.failed:
        return
    drv = request.node.funcargs.get("driver")
    if drv is None:
        return
    base = os.path.join(artifacts_dir, request.node.name)
    try:
        drv.save_screenshot(f"{base}.png")
        with open(f"{base}.html", "w", encoding="utf-8") as fh:
            fh.write(drv.page_source)
        print(f"\n[e2e] saved failure artifacts: {base}.png / {base}.html")
    except Exception as e:  # pragma: no cover - best effort
        print(f"\n[e2e] could not capture failure artifacts: {e}")
