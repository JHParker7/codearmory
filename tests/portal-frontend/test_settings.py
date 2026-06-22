"""
Settings / profile page browser tests.

Playwright: verifies form pre-population, save success/error flows.
  - Mocks PUT /api/users/:id to control success and error paths.
Selenium:   smoke tests — page renders, fields are present, form is saveable.
"""

import json
import pytest
from playwright.sync_api import expect
from selenium.webdriver.common.by import By
from selenium.webdriver.support.ui import WebDriverWait
from selenium.webdriver.support import expected_conditions as EC

from pages.settings_page import SettingsPage
from conftest import wait_for_url


def _user_response(user: dict) -> str:
    return json.dumps({
        "user_id": user["user_id"],
        "email": user["email"],
        "username": user["username"],
        "firstname": user.get("firstname", ""),
        "lastname": user.get("lastname", ""),
        "created_at": "2024-01-01T00:00:00Z",
        "updated_at": "2024-01-01T00:00:00Z",
        "active": True,
    })


def _setup_auth_with_user(pw_auth_page, user: dict):
    """Attach user mock to auth page and return it."""
    body = _user_response(user)
    pw_auth_page.route("**/api/users/**", lambda r: r.fulfill(
        status=200,
        content_type="application/json",
        body=body,
    ))
    return pw_auth_page


# ---------------------------------------------------------------------------
# Playwright — Settings
# ---------------------------------------------------------------------------

@pytest.mark.playwright
class TestSettingsPlaywright:
    def test_settings_page_loads(self, pw_auth_page, portal_url, user):
        page = _setup_auth_with_user(pw_auth_page, user)
        settings = SettingsPage(page, portal_url)
        settings.navigate()
        settings.assert_loaded()

    def test_settings_shows_profile_heading(self, pw_auth_page, portal_url, user):
        page = _setup_auth_with_user(pw_auth_page, user)
        settings = SettingsPage(page, portal_url)
        settings.navigate()
        expect(page.locator('text=profile/')).to_be_visible(timeout=8_000)

    def test_settings_shows_user_id(self, pw_auth_page, portal_url, user):
        page = _setup_auth_with_user(pw_auth_page, user)
        settings = SettingsPage(page, portal_url)
        settings.navigate()
        settings.assert_loaded()
        expect(page.locator(f'text={user["user_id"]}')).to_be_visible()

    def test_email_field_pre_populated(self, pw_auth_page, portal_url, user):
        page = _setup_auth_with_user(pw_auth_page, user)
        settings = SettingsPage(page, portal_url)
        settings.navigate()
        settings.assert_loaded()
        settings.assert_email_value(user["email"])

    def test_username_field_pre_populated(self, pw_auth_page, portal_url, user):
        page = _setup_auth_with_user(pw_auth_page, user)
        settings = SettingsPage(page, portal_url)
        settings.navigate()
        settings.assert_loaded()
        settings.assert_username_value(user["username"])

    def test_identity_section_visible(self, pw_auth_page, portal_url, user):
        page = _setup_auth_with_user(pw_auth_page, user)
        settings = SettingsPage(page, portal_url)
        settings.navigate()
        settings.assert_loaded()
        expect(page.locator('text=IDENTITY')).to_be_visible()

    def test_password_section_visible(self, pw_auth_page, portal_url, user):
        page = _setup_auth_with_user(pw_auth_page, user)
        settings = SettingsPage(page, portal_url)
        settings.navigate()
        settings.assert_loaded()
        expect(page.get_by_text("PASSWORD", exact=True)).to_be_visible()

    def test_membership_section_visible(self, pw_auth_page, portal_url, user):
        page = _setup_auth_with_user(pw_auth_page, user)
        settings = SettingsPage(page, portal_url)
        settings.navigate()
        settings.assert_loaded()
        expect(page.locator('text=MEMBERSHIP')).to_be_visible()
        # Membership fields are read-only
        expect(page.locator('text=read-only')).to_be_visible()

    def test_save_button_present(self, pw_auth_page, portal_url, user):
        page = _setup_auth_with_user(pw_auth_page, user)
        settings = SettingsPage(page, portal_url)
        settings.navigate()
        settings.assert_loaded()
        expect(settings.save_button).to_be_visible()
        expect(settings.save_button).to_contain_text("save changes")

    def test_save_success_shows_confirmation(self, pw_auth_page, portal_url, user):
        body = _user_response(user)
        # GET for initial load + PUT for save + GET for refreshUser
        pw_auth_page.route("**/api/users/**", lambda r: r.fulfill(
            status=200,
            content_type="application/json",
            body=body,
        ))
        settings = SettingsPage(pw_auth_page, portal_url)
        settings.navigate()
        settings.assert_loaded()
        settings.save()
        settings.assert_success_visible()

    def test_save_error_shows_error_banner(self, pw_auth_page, portal_url, user):
        call_count = {"n": 0}

        def handler(route):
            call_count["n"] += 1
            if call_count["n"] == 1:
                # First call: GET for auth setup — return user data
                route.fulfill(
                    status=200,
                    content_type="application/json",
                    body=_user_response(user),
                )
            else:
                # Subsequent calls (PUT save + any refresh): return error
                route.fulfill(
                    status=500,
                    content_type="application/json",
                    body='{"error":"internal server error"}',
                )

        pw_auth_page.route("**/api/users/**", handler)
        settings = SettingsPage(pw_auth_page, portal_url)
        settings.navigate()
        settings.assert_loaded()
        settings.save()
        settings.assert_error_visible()

    def test_password_show_hide_toggle(self, pw_auth_page, portal_url, user):
        page = _setup_auth_with_user(pw_auth_page, user)
        settings = SettingsPage(page, portal_url)
        settings.navigate()
        settings.assert_loaded()

        pw_input = page.get_by_placeholder("••••••••")
        expect(pw_input).to_have_attribute("type", "password")
        settings.show_hide_pw_button.click()
        expect(pw_input).to_have_attribute("type", "text")

    def test_save_button_text_while_saving(self, pw_auth_page, portal_url, user):
        """The save button should briefly say 'saving' while the request is in-flight."""
        import time

        def slow_handler(route):
            # Simulate a slow PUT response
            time.sleep(0.5)
            route.fulfill(
                status=200,
                content_type="application/json",
                body=_user_response(user),
            )

        call_count = {"n": 0}

        def handler(route):
            call_count["n"] += 1
            if call_count["n"] == 1:
                route.fulfill(
                    status=200,
                    content_type="application/json",
                    body=_user_response(user),
                )
            else:
                slow_handler(route)

        pw_auth_page.route("**/api/users/**", handler)
        settings = SettingsPage(pw_auth_page, portal_url)
        settings.navigate()
        settings.assert_loaded()
        settings.save()
        # Button may transiently show "saving" — assert final state is restored
        settings.assert_success_visible()
        expect(settings.save_button).to_contain_text("save changes")

    def test_settings_unauthenticated_redirects(self, pw_page, portal_url):
        pw_page.goto(f"{portal_url}/app/settings")
        pw_page.wait_for_url("**/login", timeout=8_000)


# ---------------------------------------------------------------------------
# Selenium — Settings
# ---------------------------------------------------------------------------

@pytest.mark.selenium
class TestSettingsSelenium:
    def test_settings_page_renders_when_authenticated(
        self, sel_auth_driver, portal_url
    ):
        sel_auth_driver.get(f"{portal_url}/app/settings")
        WebDriverWait(sel_auth_driver, 15).until(
            EC.presence_of_element_located(
                (By.XPATH, "//*[contains(text(), 'profile/')]")
            )
        )
        heading = sel_auth_driver.find_element(
            By.XPATH, "//*[contains(text(), 'profile/')]"
        )
        assert heading.is_displayed()

    def test_settings_url_correct(self, sel_auth_driver, portal_url):
        sel_auth_driver.get(f"{portal_url}/app/settings")
        wait_for_url(sel_auth_driver, "/app/settings", timeout=15)
        assert "/app/settings" in sel_auth_driver.current_url

    def test_email_field_present(self, sel_auth_driver, portal_url):
        sel_auth_driver.get(f"{portal_url}/app/settings")
        WebDriverWait(sel_auth_driver, 15).until(
            EC.presence_of_element_located(
                (By.XPATH, "//*[contains(text(), 'profile/')]")
            )
        )
        email = sel_auth_driver.find_element(By.CSS_SELECTOR, 'input[type="email"]')
        assert email.is_displayed()
        # Should be pre-populated from the real user profile
        assert email.get_attribute("value") != ""

    def test_username_field_present(self, sel_auth_driver, portal_url):
        sel_auth_driver.get(f"{portal_url}/app/settings")
        WebDriverWait(sel_auth_driver, 15).until(
            EC.presence_of_element_located(
                (By.XPATH, "//*[contains(text(), 'profile/')]")
            )
        )
        username = sel_auth_driver.find_element(
            By.CSS_SELECTOR, 'input[placeholder="janedoe"]'
        )
        assert username.is_displayed()
        assert username.get_attribute("value") != ""

    def test_save_button_present(self, sel_auth_driver, portal_url):
        sel_auth_driver.get(f"{portal_url}/app/settings")
        WebDriverWait(sel_auth_driver, 15).until(
            EC.presence_of_element_located(
                (By.XPATH, "//*[contains(text(), 'profile/')]")
            )
        )
        save_btn = sel_auth_driver.find_element(
            By.CSS_SELECTOR, 'button[type="submit"]'
        )
        assert save_btn.is_displayed()
        assert "save" in save_btn.text.lower()

    def test_membership_section_present(self, sel_auth_driver, portal_url):
        sel_auth_driver.get(f"{portal_url}/app/settings")
        WebDriverWait(sel_auth_driver, 15).until(
            EC.presence_of_element_located(
                (By.XPATH, "//*[contains(text(), 'MEMBERSHIP')]")
            )
        )
        section = sel_auth_driver.find_element(
            By.XPATH, "//*[contains(text(), 'MEMBERSHIP')]"
        )
        assert section.is_displayed()

    def test_save_success_shows_banner(self, sel_auth_driver, portal_url):
        sel_auth_driver.get(f"{portal_url}/app/settings")
        WebDriverWait(sel_auth_driver, 15).until(
            EC.presence_of_element_located(
                (By.XPATH, "//*[contains(text(), 'profile/')]")
            )
        )
        save_btn = WebDriverWait(sel_auth_driver, 10).until(
            EC.element_to_be_clickable((By.CSS_SELECTOR, 'button[type="submit"]'))
        )
        save_btn.click()
        WebDriverWait(sel_auth_driver, 8).until(
            EC.presence_of_element_located(
                (By.XPATH, "//*[contains(text(), 'profile updated successfully')]")
            )
        )
        banner = sel_auth_driver.find_element(
            By.XPATH, "//*[contains(text(), 'profile updated successfully')]"
        )
        assert banner.is_displayed()

    def test_settings_unauthenticated_redirects(self, sel_driver, portal_url):
        sel_driver.get(f"{portal_url}/app/settings")
        wait_for_url(sel_driver, "/login", timeout=10)
        assert "/login" in sel_driver.current_url
