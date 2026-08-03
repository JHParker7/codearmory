"""
Browser tests for how the portal holds a session.

The SPA does not keep the JWT in localStorage. Gatekeeper issues the session as an
HttpOnly/Secure/SameSite=Strict `armory_session` cookie, conductor accepts that cookie
on every non-public route, and the only thing the app persists is `ca_uid` — the user
id, an identifier rather than a credential. These tests pin both halves of that:

  * the credential is NEVER written to storage (the security property), and
  * a reload still resumes the session (the regression the change could plausibly cause).

The second is the reason this file exists. Gating the router on the token — which is
what the app used to do — makes every refresh look like a logout, and no unit test or
typecheck catches it because it is a browser-lifecycle behaviour.

Playwright covers the whole matrix; the Selenium block re-checks the two properties that
matter most, mirroring the split used across this directory.

Requires a running stack (see conftest). Note that gatekeeper must run with
COOKIE_SECURE=false over plain http, or Chrome silently refuses to store the cookie and
every test here fails on an empty session — infra/local/compose.yml sets it.
"""

import json

import pytest
from playwright.sync_api import expect
from selenium.webdriver.common.by import By
from selenium.webdriver.support.ui import WebDriverWait
from selenium.webdriver.support import expected_conditions as EC

from pages.login_page import LoginPage


LEGACY_TOKEN_KEY = "ca_token"
USER_ID_KEY = "ca_uid"


def _storage_dump(page) -> dict:
    """Every localStorage key/value visible to page scripts."""
    return page.evaluate(
        "() => Object.fromEntries(Object.entries(localStorage))"
    )


def _looks_like_a_jwt(value: str) -> bool:
    """Three base64url segments — the shape of the credential we must never store."""
    parts = value.split(".")
    return len(parts) == 3 and all(parts) and value.count(" ") == 0


# Playwright's "networkidle" is deliberately NOT used anywhere here. This SPA polls in
# the background (run/permission hydration), so idleness may never arrive; waiting on it
# burns the timeout and then cascades into every later step. Instead we wait for the
# concrete thing that means "the route guard has finished deciding": either the app
# shell rendered (its logout control exists) or we were bounced to the login form.
_SETTLED = 'input[type="email"], button:has-text("./logout")'


def _wait_until_settled(page, timeout: int = 20_000):
    """Block until the app shell or the login form is on screen."""
    page.locator(_SETTLED).first.wait_for(state="visible", timeout=timeout)


def _login_via_ui(page, portal_url: str, user: dict):
    """Sign in through the form and wait until any /app route is reached.

    Deliberately NOT LoginPage.assert_redirected_to_app(): that waits for
    /app/blueprints specifically, which only exists when the blueprints service is
    deployed. The landing module depends on what conductor has registered, and none
    of these tests care which one it is — only that we ended up inside the app.
    """
    login = LoginPage(page, portal_url)
    login.navigate()
    login.login(user["email"], user["password"])
    page.wait_for_url(lambda u: "/app" in u, timeout=20_000)
    _wait_until_settled(page)


# ---------------------------------------------------------------------------
# Playwright — the credential never reaches storage
# ---------------------------------------------------------------------------

@pytest.mark.playwright
class TestCredentialNotPersisted:
    def test_login_writes_no_token_to_localstorage(self, pw_page, portal_url, user):
        """After a real UI login, nothing JWT-shaped is in localStorage."""
        _login_via_ui(pw_page, portal_url, user)

        store = _storage_dump(pw_page)
        assert LEGACY_TOKEN_KEY not in store, (
            f"the session JWT was persisted under {LEGACY_TOKEN_KEY!r} — "
            "an XSS could read it straight back out"
        )
        offenders = {k: v for k, v in store.items() if isinstance(v, str) and _looks_like_a_jwt(v)}
        assert not offenders, f"JWT-shaped values found in localStorage: {list(offenders)}"

    def test_login_persists_only_the_user_id(self, pw_page, portal_url, user):
        """`ca_uid` is what survives — and it is exactly the user id, nothing more."""
        _login_via_ui(pw_page, portal_url, user)

        stored_uid = pw_page.evaluate(f"() => localStorage.getItem({json.dumps(USER_ID_KEY)})")
        assert stored_uid == user["user_id"], f"expected the user id, got {stored_uid!r}"

    def test_session_cookie_is_httponly_and_unreadable(self, pw_page, portal_url, user):
        """The cookie exists, is flagged HttpOnly, and page scripts cannot see it."""
        _login_via_ui(pw_page, portal_url, user)

        cookies = [c for c in pw_page.context.cookies() if c["name"] == "armory_session"]
        assert cookies, "no armory_session cookie was set by login"
        assert cookies[0]["httpOnly"] is True, "session cookie is not HttpOnly"

        # The property that actually matters: unreachable from JS.
        visible = pw_page.evaluate("() => document.cookie")
        assert "armory_session" not in visible, (
            f"session cookie is readable from document.cookie: {visible!r}"
        )

    def test_legacy_token_is_purged_on_load(self, pw_page, portal_url):
        """A token left by an older build is removed rather than left lying around."""
        pw_page.goto(portal_url)
        pw_page.evaluate(
            f"() => localStorage.setItem({json.dumps(LEGACY_TOKEN_KEY)}, 'stale.jwt.value')"
        )
        pw_page.reload()
        pw_page.wait_for_load_state("domcontentloaded")

        left = pw_page.evaluate(f"() => localStorage.getItem({json.dumps(LEGACY_TOKEN_KEY)})")
        assert left is None, f"stale token survived a load: {left!r}"


# ---------------------------------------------------------------------------
# Playwright — the session survives a reload
# ---------------------------------------------------------------------------

@pytest.mark.playwright
class TestSessionSurvivesReload:
    def test_hard_reload_keeps_the_user_signed_in(self, pw_page, portal_url, user):
        """The regression this change could most plausibly cause.

        After a reload there is no in-memory token — only the cookie. If the route
        guard consults the token (as it once did) this lands on /login instead.
        """
        _login_via_ui(pw_page, portal_url, user)
        signed_in_url = pw_page.url

        pw_page.reload()
        _wait_until_settled(pw_page)

        assert "/login" not in pw_page.url, (
            f"a refresh logged the user out (landed on {pw_page.url}) — "
            "the route guard is consulting the in-memory token, not the session"
        )
        assert "/app" in pw_page.url, f"expected to stay in /app, got {pw_page.url}"
        assert pw_page.url == signed_in_url or "/app" in pw_page.url

    def test_direct_navigation_to_deep_route_after_reload(self, pw_page, portal_url, user):
        """A resumed session can address a deep route directly, not just /app."""
        _login_via_ui(pw_page, portal_url, user)

        pw_page.goto(f"{portal_url}/app/settings")
        _wait_until_settled(pw_page)
        assert "/login" not in pw_page.url, f"deep route bounced to login: {pw_page.url}"

    def test_reload_still_holds_no_token(self, pw_page, portal_url, user):
        """Resuming must not re-introduce a stored credential."""
        _login_via_ui(pw_page, portal_url, user)
        pw_page.reload()
        _wait_until_settled(pw_page)

        store = _storage_dump(pw_page)
        assert LEGACY_TOKEN_KEY not in store
        assert not [v for v in store.values() if isinstance(v, str) and _looks_like_a_jwt(v)]


# ---------------------------------------------------------------------------
# Playwright — the id alone is not a credential
# ---------------------------------------------------------------------------

@pytest.mark.playwright
class TestUserIdIsNotACredential:
    def test_planted_user_id_without_a_cookie_does_not_authenticate(
        self, pw_page, portal_url, user
    ):
        """Writing `ca_uid` into a cookie-less browser must NOT sign anyone in.

        This is what makes persisting the id acceptable. If it were sufficient on its
        own, moving the token out of storage would have bought nothing.
        """
        pw_page.goto(portal_url)
        pw_page.context.clear_cookies()
        pw_page.evaluate(
            f"() => localStorage.setItem({json.dumps(USER_ID_KEY)}, {json.dumps(user['user_id'])})"
        )
        pw_page.goto(f"{portal_url}/app")
        pw_page.wait_for_url("**/login", timeout=15_000)
        expect(pw_page.locator('input[type="email"]')).to_be_visible()

    def test_stale_user_id_is_cleared_after_the_bounce(self, pw_page, portal_url, user):
        """A rejected session drops the id, so the next load does not retry forever."""
        pw_page.goto(portal_url)
        pw_page.context.clear_cookies()
        pw_page.evaluate(
            f"() => localStorage.setItem({json.dumps(USER_ID_KEY)}, {json.dumps(user['user_id'])})"
        )
        pw_page.goto(f"{portal_url}/app")
        pw_page.wait_for_url("**/login", timeout=15_000)

        left = pw_page.evaluate(f"() => localStorage.getItem({json.dumps(USER_ID_KEY)})")
        assert left is None, f"a rejected session left {USER_ID_KEY}={left!r} behind"


# ---------------------------------------------------------------------------
# Playwright — logout ends the session for real
# ---------------------------------------------------------------------------

@pytest.mark.playwright
class TestLogoutEndsTheSession:
    def _logout(self, page):
        # Sidebar renders "[ ./logout ]" expanded; name matching is substring-based.
        page.get_by_role("button", name="./logout").click()

    def test_logout_clears_the_cookie(self, pw_page, portal_url, user):
        _login_via_ui(pw_page, portal_url, user)

        self._logout(pw_page)
        pw_page.wait_for_url(lambda u: "/app" not in u, timeout=15_000)

        remaining = [c for c in pw_page.context.cookies() if c["name"] == "armory_session"]
        assert not remaining, f"session cookie survived logout: {remaining}"

    def test_reload_after_logout_lands_on_login(self, pw_page, portal_url, user):
        """The end-to-end property: logging out actually logs you out."""
        _login_via_ui(pw_page, portal_url, user)

        self._logout(pw_page)
        pw_page.wait_for_url(lambda u: "/app" not in u, timeout=15_000)

        pw_page.goto(f"{portal_url}/app")
        pw_page.wait_for_url("**/login", timeout=15_000)


# ---------------------------------------------------------------------------
# Selenium — the two properties that matter most
# ---------------------------------------------------------------------------

@pytest.mark.selenium
class TestSessionPersistenceSelenium:
    def _login(self, driver, portal_url, user):
        driver.get(f"{portal_url}/login")
        WebDriverWait(driver, 20).until(
            EC.presence_of_element_located((By.CSS_SELECTOR, "input[type='email']"))
        ).send_keys(user["email"])
        driver.find_element(By.CSS_SELECTOR, "input[type='password']").send_keys(user["password"])
        driver.find_element(By.CSS_SELECTOR, "button[type='submit']").click()
        WebDriverWait(driver, 20).until(lambda d: "/login" not in d.current_url)

    def test_no_token_in_localstorage_after_login(self, sel_driver, portal_url, user):
        self._login(sel_driver, portal_url, user)
        store = sel_driver.execute_script(
            "return Object.fromEntries(Object.entries(localStorage));"
        )
        assert LEGACY_TOKEN_KEY not in store, f"session JWT persisted: {store.get(LEGACY_TOKEN_KEY)!r}"
        offenders = [k for k, v in store.items() if isinstance(v, str) and _looks_like_a_jwt(v)]
        assert not offenders, f"JWT-shaped values in localStorage: {offenders}"

    def test_reload_keeps_the_session(self, sel_driver, portal_url, user):
        self._login(sel_driver, portal_url, user)
        sel_driver.refresh()
        WebDriverWait(driver=sel_driver, timeout=20).until(
            lambda d: d.execute_script("return document.readyState") == "complete"
        )
        assert "/login" not in sel_driver.current_url, (
            f"a refresh logged the user out (landed on {sel_driver.current_url})"
        )

    def test_session_cookie_is_httponly(self, sel_driver, portal_url, user):
        self._login(sel_driver, portal_url, user)
        cookie = sel_driver.get_cookie("armory_session")
        assert cookie is not None, "no armory_session cookie was set by login"
        assert cookie.get("httpOnly") is True, "session cookie is not HttpOnly"
        visible = sel_driver.execute_script("return document.cookie;")
        assert "armory_session" not in visible, f"cookie readable from JS: {visible!r}"
