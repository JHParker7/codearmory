"""
Blueprints page browser tests.

These tests verify the workspace browser UI: empty state, add-workspace
modal, sidebar list, and workspace detail panels (loading, data, locked).

Playwright: rich assertions with page.route() for state/lock scenarios.
Selenium:   smoke tests against the real authenticated UI.
"""

import json
import uuid
import pytest
from playwright.sync_api import expect
from selenium.webdriver.common.by import By
from selenium.webdriver.support.ui import WebDriverWait
from selenium.webdriver.support import expected_conditions as EC

from pages.blueprints_page import BlueprintsPage
from conftest import wait_for_url


MOCK_STATE = {
    "version": 4,
    "terraform_version": "1.9.0",
    "serial": 7,
    "lineage": "abc12345-0000-0000-0000-000000000000",
    "outputs": {"bucket_name": {"value": "my-bucket", "type": "string"}},
    "resources": [
        {"type": "aws_s3_bucket", "name": "main", "mode": "managed"},
        {"type": "aws_s3_bucket", "name": "logs", "mode": "managed"},
        {"type": "aws_iam_role", "name": "lambda", "mode": "managed"},
    ],
}

MOCK_LOCK = {
    "ID": "lock-id-9999",
    "Operation": "OperationTypePlan",
    "Who": "alice@example.com",
    "Info": "",
    "Version": "1.9.0",
    "Created": "2024-06-01T10:00:00Z",
    "Path": "alice/prod",
}

# BFF-normalized WorkspaceView shapes (what the SPA actually receives)
MOCK_STATE_VIEW = {
    "isEmpty": False,
    "locked": False,
    "lock": None,
    "state": {
        "serial": 7,
        "terraform_version": "1.9.0",
        "lineage": "abc12345-0000-0000-0000-000000000000",
        "resource_count": 3,
        "resource_types": [
            {"type": "aws_s3_bucket", "count": 2},
            {"type": "aws_iam_role", "count": 1},
        ],
        "outputs": {"bucket_name": {"value": "my-bucket", "type": "string"}},
    },
}

MOCK_LOCK_VIEW = {
    "isEmpty": False,
    "locked": True,
    "lock": {
        "id": "lock-id-9999",
        "operation": "OperationTypePlan",
        "who": "alice@example.com",
        "info": "",
        "version": "1.9.0",
        "created": "2024-06-01T10:00:00Z",
        "path": "alice/prod",
    },
    "state": None,
}

MOCK_EMPTY_VIEW = {
    "isEmpty": True,
    "locked": False,
    "lock": None,
    "state": None,
}


def _auth_page_with_user_mock(pw_auth_page, portal_url, user):
    """Set up user mock and navigate to blueprints. Returns the page."""
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
    return pw_auth_page


# ---------------------------------------------------------------------------
# Playwright — Blueprints
# ---------------------------------------------------------------------------

@pytest.mark.playwright
class TestBlueprintsPlaywright:
    def test_blueprints_page_loads_when_authenticated(
        self, pw_auth_page, portal_url, user
    ):
        page = _auth_page_with_user_mock(pw_auth_page, portal_url, user)
        bp = BlueprintsPage(page, portal_url)
        bp.navigate()
        bp.assert_loaded()

    def test_empty_state_shown_when_no_workspaces(
        self, pw_auth_page, portal_url, user
    ):
        page = _auth_page_with_user_mock(pw_auth_page, portal_url, user)
        # Ensure localStorage has no workspaces for this test
        page.evaluate("localStorage.removeItem('ca_workspaces')")
        bp = BlueprintsPage(page, portal_url)
        bp.navigate()
        bp.assert_loaded()
        bp.assert_empty_state()

    def test_add_button_opens_modal(self, pw_auth_page, portal_url, user):
        page = _auth_page_with_user_mock(pw_auth_page, portal_url, user)
        page.evaluate("localStorage.removeItem('ca_workspaces')")
        bp = BlueprintsPage(page, portal_url)
        bp.navigate()
        bp.assert_loaded()
        bp.click_add()
        bp.assert_modal_visible()

    def test_empty_state_add_button_opens_modal(self, pw_auth_page, portal_url, user):
        page = _auth_page_with_user_mock(pw_auth_page, portal_url, user)
        page.evaluate("localStorage.removeItem('ca_workspaces')")
        bp = BlueprintsPage(page, portal_url)
        bp.navigate()
        bp.assert_loaded()
        bp.empty_state_add_button.click()
        bp.assert_modal_visible()

    def test_modal_cancel_closes_modal(self, pw_auth_page, portal_url, user):
        page = _auth_page_with_user_mock(pw_auth_page, portal_url, user)
        page.evaluate("localStorage.removeItem('ca_workspaces')")
        bp = BlueprintsPage(page, portal_url)
        bp.navigate()
        bp.assert_loaded()
        bp.click_add()
        bp.assert_modal_visible()
        bp.modal_cancel_button.click()
        bp.assert_modal_not_visible()

    def test_modal_add_button_disabled_for_invalid_path(
        self, pw_auth_page, portal_url, user
    ):
        page = _auth_page_with_user_mock(pw_auth_page, portal_url, user)
        page.evaluate("localStorage.removeItem('ca_workspaces')")
        bp = BlueprintsPage(page, portal_url)
        bp.navigate()
        bp.assert_loaded()
        bp.click_add()
        # A single-segment path (no slash) should keep add disabled
        bp.modal_path_input.fill("singlepart")
        expect(bp.modal_add_button).to_be_disabled()

    def test_adding_workspace_shows_in_sidebar(
        self, pw_auth_page, portal_url, user
    ):
        page = _auth_page_with_user_mock(pw_auth_page, portal_url, user)
        page.evaluate("localStorage.removeItem('ca_workspaces')")
        # Mock state fetch so the detail panel doesn't fail
        page.route("**/api/state/**", lambda r: r.fulfill(
            status=200,
            content_type="application/json",
            body=json.dumps(MOCK_STATE),
        ))
        bp = BlueprintsPage(page, portal_url)
        bp.navigate()
        bp.assert_loaded()

        ws_path = f"{user['username']}/test-ws"
        bp.add_workspace(ws_path)
        bp.assert_workspace_in_list(ws_path)

    def test_workspace_detail_shows_state_data(
        self, pw_auth_page, portal_url, user
    ):
        page = _auth_page_with_user_mock(pw_auth_page, portal_url, user)
        page.evaluate("localStorage.removeItem('ca_workspaces')")
        page.route("**/api/state/**", lambda r: r.fulfill(
            status=200,
            content_type="application/json",
            body=json.dumps(MOCK_STATE_VIEW),
        ))
        bp = BlueprintsPage(page, portal_url)
        bp.navigate()
        bp.assert_loaded()

        ws_path = f"{user['username']}/detail-ws"
        bp.add_workspace(ws_path)
        bp.click_workspace(ws_path)

        # Detail panel shows resource count and serial from MOCK_STATE_VIEW
        expect(page.locator('text=3 resources').first).to_be_visible(timeout=8_000)
        expect(page.locator('text=serial 7').first).to_be_visible(timeout=8_000)

    def test_workspace_detail_shows_lock_indicator(
        self, pw_auth_page, portal_url, user
    ):
        page = _auth_page_with_user_mock(pw_auth_page, portal_url, user)
        page.evaluate("localStorage.removeItem('ca_workspaces')")
        page.route("**/api/state/**", lambda r: r.fulfill(
            status=200,
            content_type="application/json",
            body=json.dumps(MOCK_LOCK_VIEW),
        ))
        bp = BlueprintsPage(page, portal_url)
        bp.navigate()
        bp.assert_loaded()

        ws_path = f"{user['username']}/locked-ws"
        bp.add_workspace(ws_path)
        bp.click_workspace(ws_path)

        expect(page.locator('text=workspace locked')).to_be_visible(timeout=8_000)
        expect(page.locator('text=alice').first).to_be_visible(timeout=8_000)

    def test_workspace_no_content_shows_empty_message(
        self, pw_auth_page, portal_url, user
    ):
        page = _auth_page_with_user_mock(pw_auth_page, portal_url, user)
        page.evaluate("localStorage.removeItem('ca_workspaces')")
        page.route("**/api/state/**", lambda r: r.fulfill(
            status=200,
            content_type="application/json",
            body=json.dumps(MOCK_EMPTY_VIEW),
        ))
        bp = BlueprintsPage(page, portal_url)
        bp.navigate()
        bp.assert_loaded()

        ws_path = f"{user['username']}/empty-ws"
        bp.add_workspace(ws_path)
        bp.click_workspace(ws_path)

        expect(page.locator('text=204 No Content')).to_be_visible(timeout=8_000)

    def test_workspace_api_error_shows_retry_button(
        self, pw_auth_page, portal_url, user
    ):
        page = _auth_page_with_user_mock(pw_auth_page, portal_url, user)
        page.evaluate("localStorage.removeItem('ca_workspaces')")
        page.route("**/api/state/**", lambda r: r.fulfill(
            status=403,
            content_type="application/json",
            body='{"error":"forbidden"}',
        ))
        bp = BlueprintsPage(page, portal_url)
        bp.navigate()
        bp.assert_loaded()

        ws_path = f"{user['username']}/forbidden-ws"
        bp.add_workspace(ws_path)
        bp.click_workspace(ws_path)

        expect(page.get_by_role("button", name="[ retry ]")).to_be_visible(timeout=8_000)

    def test_remove_workspace_shows_confirmation(
        self, pw_auth_page, portal_url, user
    ):
        page = _auth_page_with_user_mock(pw_auth_page, portal_url, user)
        page.evaluate("localStorage.removeItem('ca_workspaces')")
        page.route("**/api/state/**", lambda r: r.fulfill(
            status=200,
            content_type="application/json",
            body=json.dumps(MOCK_STATE),
        ))
        bp = BlueprintsPage(page, portal_url)
        bp.navigate()
        bp.assert_loaded()

        ws_path = f"{user['username']}/remove-ws"
        bp.add_workspace(ws_path)
        bp.click_workspace(ws_path)
        page.get_by_role("button", name="[ remove ]").wait_for(timeout=8_000)
        page.get_by_role("button", name="[ remove ]").click()

        expect(page.get_by_role("button", name="[ confirm ]")).to_be_visible()
        expect(page.get_by_role("button", name="[ cancel ]")).to_be_visible()

    def test_confirm_remove_workspace_removes_from_sidebar(
        self, pw_auth_page, portal_url, user
    ):
        page = _auth_page_with_user_mock(pw_auth_page, portal_url, user)
        page.evaluate("localStorage.removeItem('ca_workspaces')")
        page.route("**/api/state/**", lambda r: r.fulfill(
            status=200,
            content_type="application/json",
            body=json.dumps(MOCK_STATE),
        ))
        bp = BlueprintsPage(page, portal_url)
        bp.navigate()
        bp.assert_loaded()

        ws_path = f"{user['username']}/to-remove-ws"
        bp.add_workspace(ws_path)
        bp.click_workspace(ws_path)
        page.get_by_role("button", name="[ remove ]").wait_for(timeout=8_000)
        bp.remove_workspace(ws_path)

        bp.assert_workspace_not_in_list(ws_path)

    def test_conductor_header_shows_in_blueprints(
        self, pw_auth_page, portal_url, user
    ):
        page = _auth_page_with_user_mock(pw_auth_page, portal_url, user)
        bp = BlueprintsPage(page, portal_url)
        bp.navigate()
        bp.assert_loaded()
        expect(page.locator('text=conductor :8082')).to_be_visible()


# ---------------------------------------------------------------------------
# Selenium — Blueprints
# ---------------------------------------------------------------------------

@pytest.mark.selenium
class TestBlueprintsSelenium:
    def test_blueprints_page_renders_when_authenticated(
        self, sel_auth_driver, portal_url
    ):
        sel_auth_driver.get(f"{portal_url}/app/blueprints")
        WebDriverWait(sel_auth_driver, 15).until(
            EC.presence_of_element_located((By.XPATH, "//*[contains(text(), 'blueprints/')]"))
        )
        header = sel_auth_driver.find_element(By.XPATH, "//*[contains(text(), 'blueprints/')]")
        assert header.is_displayed()

    def test_blueprints_url_is_correct(self, sel_auth_driver, portal_url):
        sel_auth_driver.get(f"{portal_url}/app/blueprints")
        wait_for_url(sel_auth_driver, "/app/blueprints", timeout=15)
        assert "/app/blueprints" in sel_auth_driver.current_url

    def test_add_button_present(self, sel_auth_driver, portal_url):
        sel_auth_driver.get(f"{portal_url}/app/blueprints")
        WebDriverWait(sel_auth_driver, 15).until(
            EC.presence_of_element_located((By.XPATH, "//*[contains(text(), 'blueprints/')]"))
        )
        add_btn = sel_auth_driver.find_elements(
            By.XPATH, "//button[contains(text(), '+ add')]"
        )
        assert len(add_btn) > 0

    def test_add_workspace_modal_opens(self, sel_auth_driver, portal_url):
        sel_auth_driver.get(f"{portal_url}/app/blueprints")
        # Clear any persisted workspaces
        sel_auth_driver.execute_script("localStorage.removeItem('ca_workspaces')")
        sel_auth_driver.refresh()

        WebDriverWait(sel_auth_driver, 15).until(
            EC.presence_of_element_located((By.XPATH, "//*[contains(text(), 'blueprints/')]"))
        )
        add_btn = WebDriverWait(sel_auth_driver, 10).until(
            EC.element_to_be_clickable((By.XPATH, "//button[contains(text(), '+ add')]"))
        )
        add_btn.click()
        WebDriverWait(sel_auth_driver, 5).until(
            EC.presence_of_element_located(
                (By.XPATH, "//*[contains(text(), 'add workspace')]")
            )
        )
        modal = sel_auth_driver.find_element(
            By.XPATH, "//*[contains(text(), 'add workspace')]"
        )
        assert modal.is_displayed()

    def test_adding_workspace_persists_in_list(self, sel_auth_driver, portal_url, user):
        sel_auth_driver.get(f"{portal_url}/app/blueprints")
        sel_auth_driver.execute_script("localStorage.removeItem('ca_workspaces')")
        sel_auth_driver.refresh()

        WebDriverWait(sel_auth_driver, 15).until(
            EC.presence_of_element_located((By.XPATH, "//*[contains(text(), 'blueprints/')]"))
        )
        add_btn = WebDriverWait(sel_auth_driver, 10).until(
            EC.element_to_be_clickable((By.XPATH, "//button[contains(text(), '+ add')]"))
        )
        add_btn.click()

        ws_path = f"{user['username']}/sel-ws"
        modal_input = WebDriverWait(sel_auth_driver, 5).until(
            EC.presence_of_element_located((By.CSS_SELECTOR, "[autofocus]"))
        )
        modal_input.clear()
        modal_input.send_keys(ws_path)

        confirm_btn = WebDriverWait(sel_auth_driver, 5).until(
            EC.element_to_be_clickable(
                (By.XPATH, "//button[contains(text(), '↵ add workspace')]")
            )
        )
        confirm_btn.click()

        # The path should appear in the sidebar
        WebDriverWait(sel_auth_driver, 8).until(
            EC.presence_of_element_located(
                (By.XPATH, f"//button[contains(text(), '{ws_path}')]")
            )
        )
        ws_item = sel_auth_driver.find_element(
            By.XPATH, f"//button[contains(text(), '{ws_path}')]"
        )
        assert ws_item.is_displayed()

    def test_blueprint_unauthenticated_redirects_to_login(self, sel_driver, portal_url):
        sel_driver.get(f"{portal_url}/app/blueprints")
        wait_for_url(sel_driver, "/login", timeout=10)
        assert "/login" in sel_driver.current_url
