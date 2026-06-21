from playwright.sync_api import Page, expect


class SettingsPage:
    def __init__(self, page: Page, base_url: str):
        self.page = page
        self.base_url = base_url

    def navigate(self):
        self.page.goto(f"{self.base_url}/app/settings")
        self.page.wait_for_load_state("domcontentloaded")

    # ── Locators ──────────────────────────────────────────────────────────────

    @property
    def heading(self):
        return self.page.locator('text=profile/')

    @property
    def email_input(self):
        return self.page.get_by_placeholder("you@company.dev")

    @property
    def username_input(self):
        return self.page.get_by_placeholder("janedoe")

    @property
    def firstname_input(self):
        return self.page.get_by_placeholder("Jane")

    @property
    def lastname_input(self):
        return self.page.get_by_placeholder("Doe")

    @property
    def password_input(self):
        return self.page.get_by_placeholder("••••••••")

    @property
    def save_button(self):
        return self.page.locator('button[type="submit"]')

    @property
    def success_banner(self):
        return self.page.locator('text=profile updated successfully')

    @property
    def error_banner(self):
        return self.page.locator('text=ERR ·').first

    @property
    def show_hide_pw_button(self):
        return self.page.get_by_role("button", name="--show")

    # ── Actions ───────────────────────────────────────────────────────────────

    def fill_firstname(self, value: str):
        self.firstname_input.fill(value)

    def fill_lastname(self, value: str):
        self.lastname_input.fill(value)

    def save(self):
        self.save_button.click()

    # ── Assertions ────────────────────────────────────────────────────────────

    def assert_loaded(self):
        expect(self.heading).to_be_visible(timeout=10_000)
        expect(self.email_input).to_be_visible()
        expect(self.save_button).to_be_visible()

    def assert_email_value(self, email: str):
        expect(self.email_input).to_have_value(email)

    def assert_username_value(self, username: str):
        expect(self.username_input).to_have_value(username)

    def assert_success_visible(self):
        expect(self.success_banner).to_be_visible(timeout=5_000)

    def assert_error_visible(self):
        expect(self.error_banner).to_be_visible(timeout=5_000)
