"""
Authentication flow browser tests: Login, Signup, route guards.

Playwright: tests full form interaction, error states, and redirects.
  - Uses page.route() to inject specific API responses (401, 409, etc.)
    for error-path tests; success-path tests hit the real conductor backend.
Selenium: verifies form rendering and the real login/signup happy paths.
"""

import uuid
import pytest
from playwright.sync_api import expect
from selenium.webdriver.common.by import By
from selenium.webdriver.support.ui import WebDriverWait
from selenium.webdriver.support import expected_conditions as EC

from pages.login_page import LoginPage
from pages.signup_page import SignupPage
from conftest import wait_for_url


# ---------------------------------------------------------------------------
# Playwright — Login
# ---------------------------------------------------------------------------

@pytest.mark.playwright
class TestLoginPlaywright:
    def test_login_page_loads(self, pw_page, portal_url):
        login = LoginPage(pw_page, portal_url)
        login.navigate()
        login.assert_loaded()

    def test_window_chrome_shows_login_sh(self, pw_page, portal_url):
        pw_page.goto(f"{portal_url}/login")
        pw_page.wait_for_load_state("domcontentloaded")
        expect(pw_page.locator('text=login.sh')).to_be_visible()

    def test_submit_disabled_when_form_empty(self, pw_page, portal_url):
        login = LoginPage(pw_page, portal_url)
        login.navigate()
        login.assert_submit_disabled()

    def test_submit_disabled_with_only_email(self, pw_page, portal_url):
        login = LoginPage(pw_page, portal_url)
        login.navigate()
        login.fill_email("test@example.com")
        login.assert_submit_disabled()

    def test_submit_enabled_with_email_and_password(self, pw_page, portal_url):
        login = LoginPage(pw_page, portal_url)
        login.navigate()
        login.fill_email("test@example.com")
        login.fill_password("anypassword")
        login.assert_submit_enabled()

    def test_submit_button_text_reflects_state(self, pw_page, portal_url):
        login = LoginPage(pw_page, portal_url)
        login.navigate()
        # Empty — shows placeholder text
        expect(login.submit_button).to_contain_text("enter credentials")
        login.fill_email("test@example.com")
        login.fill_password("anypassword")
        # Valid — shows ready text
        expect(login.submit_button).to_contain_text("./login")

    def test_show_hide_password_toggle(self, pw_page, portal_url):
        login = LoginPage(pw_page, portal_url)
        login.navigate()
        pw_input = login.password_input
        expect(pw_input).to_have_attribute("type", "password")
        login.toggle_password_visibility()
        expect(pw_input).to_have_attribute("type", "text")
        pw_page.get_by_role("button", name="--hide").click()
        expect(pw_input).to_have_attribute("type", "password")

    def test_invalid_credentials_show_error(self, pw_page, portal_url):
        """Mock a 401 so the test is deterministic regardless of backend state."""
        pw_page.route("**/api/login", lambda r: r.fulfill(
            status=401,
            content_type="application/json",
            body='{"error":"unauthorized"}',
        ))
        login = LoginPage(pw_page, portal_url)
        login.navigate()
        login.login("bad@example.com", "wrongpassword")
        login.assert_error_visible()
        expect(pw_page.locator('text=ERR · invalid credentials')).to_be_visible()

    def test_server_error_shows_generic_error(self, pw_page, portal_url):
        pw_page.route("**/api/login", lambda r: r.fulfill(
            status=500,
            content_type="application/json",
            body='{"error":"internal server error"}',
        ))
        login = LoginPage(pw_page, portal_url)
        login.navigate()
        login.login("test@example.com", "password123")
        login.assert_error_visible()

    def test_successful_login_redirects_to_blueprints(self, pw_page, portal_url, token):
        """Use the real token fixture; mock the user fetch so no backend needed."""
        import json

        pw_page.route("**/api/login", lambda r: r.fulfill(
            status=200,
            content_type="application/json",
            body=json.dumps({"token": token}),
        ))
        # Also mock the user profile fetch that AuthContext triggers after login
        pw_page.route("**/api/users/**", lambda r: r.fulfill(
            status=200,
            content_type="application/json",
            body=json.dumps({
                "user_id": "test-uid",
                "email": "test@example.com",
                "username": "testuser",
                "created_at": "2024-01-01T00:00:00Z",
                "updated_at": "2024-01-01T00:00:00Z",
                "active": True,
            }),
        ))
        login = LoginPage(pw_page, portal_url)
        login.navigate()
        login.login("test@example.com", "password123")
        login.assert_redirected_to_app()

    def test_unauthenticated_app_route_redirects_to_login(self, pw_page, portal_url):
        pw_page.goto(f"{portal_url}/app/blueprints")
        pw_page.wait_for_url(f"{portal_url}/login", timeout=8_000)

    def test_signup_link_navigates_to_signup(self, pw_page, portal_url):
        login = LoginPage(pw_page, portal_url)
        login.navigate()
        login.signup_link.click()
        pw_page.wait_for_url(f"{portal_url}/signup", timeout=8_000)


# ---------------------------------------------------------------------------
# Playwright — Signup
# ---------------------------------------------------------------------------

@pytest.mark.playwright
class TestSignupPlaywright:
    def test_signup_page_loads(self, pw_page, portal_url):
        signup = SignupPage(pw_page, portal_url)
        signup.navigate()
        signup.assert_loaded()

    def test_window_chrome_shows_signup_sh(self, pw_page, portal_url):
        pw_page.goto(f"{portal_url}/signup")
        pw_page.wait_for_load_state("domcontentloaded")
        expect(pw_page.locator('text=signup.sh')).to_be_visible()

    def test_submit_disabled_when_form_empty(self, pw_page, portal_url):
        signup = SignupPage(pw_page, portal_url)
        signup.navigate()
        signup.assert_submit_disabled()

    def test_strength_bar_updates_on_password_input(self, pw_page, portal_url):
        signup = SignupPage(pw_page, portal_url)
        signup.navigate()
        # Weak password — bar should show "weak" or "fair"
        signup.fill_password("short")
        expect(pw_page.locator('text=weak').or_(pw_page.locator('text=fair'))).to_be_visible()

    def test_strong_password_shows_strong_label(self, pw_page, portal_url):
        signup = SignupPage(pw_page, portal_url)
        signup.navigate()
        signup.fill_password("Str0ng!Password#2024")
        # Should show "strong" or "excellent"
        expect(pw_page.locator('text=strong').or_(pw_page.locator('text=excellent'))).to_be_visible()

    def test_terms_checkbox_toggles(self, pw_page, portal_url):
        signup = SignupPage(pw_page, portal_url)
        signup.navigate()
        # Initial state: unchecked (shows "[ ]")
        expect(pw_page.get_by_text("[ ]", exact=True)).to_be_visible()
        signup.accept_terms()
        # After click: checked (shows "[x]")
        expect(pw_page.get_by_text("[x]", exact=True)).to_be_visible()

    def test_submit_remains_disabled_without_terms(self, pw_page, portal_url):
        signup = SignupPage(pw_page, portal_url)
        signup.navigate()
        signup.fill_email("test@example.com")
        signup.fill_username("testuser")
        signup.fill_password("Str0ng!Password#2024")
        # Terms not accepted — submit still disabled
        signup.assert_submit_disabled()

    def test_submit_enabled_with_all_valid_fields(self, pw_page, portal_url):
        signup = SignupPage(pw_page, portal_url)
        signup.navigate()
        signup.fill_email("test@example.com")
        signup.fill_username("testuser")
        signup.fill_password("Str0ng!Password#2024")
        signup.accept_terms()
        signup.assert_submit_enabled()

    def test_duplicate_email_shows_error(self, pw_page, portal_url):
        pw_page.route("**/api/signup", lambda r: r.fulfill(
            status=409,
            content_type="application/json",
            body='{"error":"conflict"}',
        ))
        signup = SignupPage(pw_page, portal_url)
        signup.navigate()
        signup.signup("dup@example.com", "dupuser", "Str0ng!Password#2024")
        signup.assert_error_visible()
        expect(pw_page.locator('text=already taken')).to_be_visible()

    def test_login_link_navigates_to_login(self, pw_page, portal_url):
        signup = SignupPage(pw_page, portal_url)
        signup.navigate()
        signup.login_link.click()
        pw_page.wait_for_url(f"{portal_url}/login", timeout=8_000)


# ---------------------------------------------------------------------------
# Playwright — Route guards
# ---------------------------------------------------------------------------

@pytest.mark.playwright
class TestRouteGuardsPlaywright:
    def test_app_blueprints_unauthenticated_redirects(self, pw_page, portal_url):
        pw_page.goto(f"{portal_url}/app/blueprints")
        pw_page.wait_for_url("**/login", timeout=8_000)

    def test_app_settings_unauthenticated_redirects(self, pw_page, portal_url):
        pw_page.goto(f"{portal_url}/app/settings")
        pw_page.wait_for_url("**/login", timeout=8_000)

    def test_app_index_unauthenticated_redirects(self, pw_page, portal_url):
        pw_page.goto(f"{portal_url}/app")
        pw_page.wait_for_url("**/login", timeout=8_000)

    def test_authenticated_user_visiting_login_redirects_to_app(
        self, pw_auth_page, portal_url, user
    ):
        """When a valid token is in localStorage, /login should redirect to /app/blueprints."""
        import json

        pw_auth_page.route("**/api/users/**", lambda r: r.fulfill(
            status=200,
            content_type="application/json",
            body=json.dumps({
                "user_id": user["user_id"],
                "email": user["email"],
                "username": user["username"],
                "created_at": "2024-01-01T00:00:00Z",
                "updated_at": "2024-01-01T00:00:00Z",
                "active": True,
            }),
        ))
        pw_auth_page.goto(f"{portal_url}/login")
        pw_auth_page.wait_for_url("**/app/blueprints", timeout=10_000)


# ---------------------------------------------------------------------------
# Selenium — Login
# ---------------------------------------------------------------------------

@pytest.mark.selenium
class TestLoginSelenium:
    def test_login_page_renders_form(self, sel_driver, portal_url):
        sel_driver.get(f"{portal_url}/login")
        email = sel_driver.find_element(By.CSS_SELECTOR, 'input[type="email"]')
        password = sel_driver.find_element(By.CSS_SELECTOR, 'input[type="password"]')
        assert email.is_displayed()
        assert password.is_displayed()

    def test_login_page_title(self, sel_driver, portal_url):
        sel_driver.get(f"{portal_url}/login")
        assert "codearmory" in sel_driver.title.lower()

    def test_submit_button_present(self, sel_driver, portal_url):
        sel_driver.get(f"{portal_url}/login")
        btn = sel_driver.find_element(By.CSS_SELECTOR, 'button[type="submit"]')
        assert btn.is_displayed()

    def test_submit_disabled_when_empty(self, sel_driver, portal_url):
        sel_driver.get(f"{portal_url}/login")
        btn = sel_driver.find_element(By.CSS_SELECTOR, 'button[type="submit"]')
        assert not btn.is_enabled()

    def test_real_login_redirects_to_blueprints(self, sel_driver, portal_url, user):
        sel_driver.get(f"{portal_url}/login")
        sel_driver.find_element(By.CSS_SELECTOR, 'input[type="email"]').send_keys(user["email"])
        sel_driver.find_element(By.CSS_SELECTOR, 'input[type="password"]').send_keys(user["password"])
        sel_driver.find_element(By.CSS_SELECTOR, 'button[type="submit"]').click()
        wait_for_url(sel_driver, "/app/blueprints", timeout=15)

    def test_unauthenticated_app_route_redirects(self, sel_driver, portal_url):
        sel_driver.get(f"{portal_url}/app/blueprints")
        wait_for_url(sel_driver, "/login", timeout=10)

    def test_show_hide_password_toggle(self, sel_driver, portal_url):
        sel_driver.get(f"{portal_url}/login")
        pw_input = sel_driver.find_element(By.CSS_SELECTOR, 'input[type="password"]')
        assert pw_input.get_attribute("type") == "password"
        show_btn = sel_driver.find_element(By.XPATH, "//*[contains(text(), '--show')]")
        show_btn.click()
        # After toggle the input is now type="text"
        pw_input = sel_driver.find_element(By.CSS_SELECTOR, 'input[placeholder="••••••••••••"]')
        assert pw_input.get_attribute("type") == "text"


# ---------------------------------------------------------------------------
# Selenium — Signup
# ---------------------------------------------------------------------------

@pytest.mark.selenium
class TestSignupSelenium:
    def test_signup_page_renders_form(self, sel_driver, portal_url):
        sel_driver.get(f"{portal_url}/signup")
        email = sel_driver.find_element(By.CSS_SELECTOR, 'input[type="email"]')
        password = sel_driver.find_element(By.CSS_SELECTOR, 'input[type="password"]')
        assert email.is_displayed()
        assert password.is_displayed()

    def test_submit_disabled_when_empty(self, sel_driver, portal_url):
        sel_driver.get(f"{portal_url}/signup")
        btn = sel_driver.find_element(By.CSS_SELECTOR, 'button[type="submit"]')
        assert not btn.is_enabled()

    def test_strength_bar_visible(self, sel_driver, portal_url):
        sel_driver.get(f"{portal_url}/signup")
        strength = sel_driver.find_elements(By.XPATH, "//*[contains(text(), 'strength')]")
        assert len(strength) > 0

    def test_real_signup_creates_account_and_redirects(self, sel_driver, portal_url):
        uid = uuid.uuid4().hex[:8]
        sel_driver.get(f"{portal_url}/signup")

        sel_driver.find_element(By.CSS_SELECTOR, 'input[type="email"]').send_keys(
            f"sel_{uid}@example.com"
        )
        # Username input: placeholder "janedoe"
        sel_driver.find_element(By.CSS_SELECTOR, 'input[placeholder="janedoe"]').send_keys(
            f"sel{uid}"
        )
        sel_driver.find_element(By.CSS_SELECTOR, 'input[type="password"]').send_keys(
            "Str0ng!Password#2024"
        )
        # Accept terms: click the label containing the checkbox text
        terms_label = sel_driver.find_element(By.XPATH, "//label[contains(., 'I accept the')]")
        terms_label.click()

        WebDriverWait(sel_driver, 5).until(
            EC.element_to_be_clickable((By.CSS_SELECTOR, 'button[type="submit"]'))
        ).click()

        wait_for_url(sel_driver, "/app/blueprints", timeout=20)
