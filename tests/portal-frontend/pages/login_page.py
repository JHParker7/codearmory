from playwright.sync_api import Page, expect


class LoginPage:
    def __init__(self, page: Page, base_url: str):
        self.page = page
        self.base_url = base_url

    def navigate(self):
        self.page.goto(f"{self.base_url}/login")
        self.page.wait_for_load_state("domcontentloaded")

    # ── Locators ──────────────────────────────────────────────────────────────

    @property
    def email_input(self):
        return self.page.locator('input[type="email"]')

    @property
    def password_input(self):
        return self.page.locator('input[autocomplete="current-password"]')

    @property
    def submit_button(self):
        return self.page.locator('button[type="submit"]')

    @property
    def show_hide_button(self):
        return self.page.get_by_role("button", name="--show")

    @property
    def error_banner(self):
        return self.page.locator('text=ERR ·').first

    @property
    def success_banner(self):
        return self.page.locator('text=→').first

    @property
    def signup_link(self):
        return self.page.get_by_role("link", name="./signup")

    # ── Actions ───────────────────────────────────────────────────────────────

    def fill_email(self, email: str):
        self.email_input.fill(email)

    def fill_password(self, password: str):
        self.password_input.fill(password)

    def submit(self):
        self.submit_button.click()

    def login(self, email: str, password: str):
        self.fill_email(email)
        self.fill_password(password)
        self.submit()

    def toggle_password_visibility(self):
        self.page.get_by_role("button", name="--show").click()

    # ── Assertions ────────────────────────────────────────────────────────────

    def assert_loaded(self):
        expect(self.page).to_have_url(f"{self.base_url}/login")
        expect(self.email_input).to_be_visible()
        expect(self.password_input).to_be_visible()

    def assert_submit_disabled(self):
        expect(self.submit_button).to_be_disabled()

    def assert_submit_enabled(self):
        expect(self.submit_button).to_be_enabled()

    def assert_error_visible(self):
        expect(self.error_banner).to_be_visible()

    def assert_redirected_to_app(self):
        self.page.wait_for_url("**/app/blueprints", timeout=10_000)
