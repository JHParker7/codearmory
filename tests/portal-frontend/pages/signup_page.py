from playwright.sync_api import Page, expect


class SignupPage:
    def __init__(self, page: Page, base_url: str):
        self.page = page
        self.base_url = base_url

    def navigate(self):
        self.page.goto(f"{self.base_url}/signup")
        self.page.wait_for_load_state("domcontentloaded")

    # ── Locators ──────────────────────────────────────────────────────────────

    @property
    def email_input(self):
        return self.page.locator('input[type="email"]')

    @property
    def username_input(self):
        return self.page.get_by_placeholder("janedoe")

    @property
    def password_input(self):
        return self.page.locator('input[type="password"]')

    @property
    def terms_toggle(self):
        # The terms label contains "I accept the"
        return self.page.locator('label:has-text("I accept the")')

    @property
    def submit_button(self):
        return self.page.locator('button[type="submit"]')

    @property
    def error_banner(self):
        return self.page.locator('text=ERR ·').first

    @property
    def strength_bar(self):
        return self.page.locator('text=strength')

    @property
    def login_link(self):
        return self.page.get_by_role("link", name="./login")

    # ── Actions ───────────────────────────────────────────────────────────────

    def fill_email(self, email: str):
        self.email_input.fill(email)

    def fill_username(self, username: str):
        self.username_input.fill(username)

    def fill_password(self, password: str):
        self.password_input.fill(password)

    def accept_terms(self):
        self.terms_toggle.click()

    def submit(self):
        self.submit_button.click()

    def signup(self, email: str, username: str, password: str, accept_terms=True):
        self.fill_email(email)
        self.fill_username(username)
        self.fill_password(password)
        if accept_terms:
            self.accept_terms()
        self.submit()

    # ── Assertions ────────────────────────────────────────────────────────────

    def assert_loaded(self):
        expect(self.page).to_have_url(f"{self.base_url}/signup")
        expect(self.email_input).to_be_visible()
        expect(self.password_input).to_be_visible()
        expect(self.strength_bar).to_be_visible()

    def assert_submit_disabled(self):
        expect(self.submit_button).to_be_disabled()

    def assert_submit_enabled(self):
        expect(self.submit_button).to_be_enabled()

    def assert_error_visible(self):
        expect(self.error_banner).to_be_visible()

    def assert_redirected_to_app(self):
        self.page.wait_for_url("**/app/blueprints", timeout=15_000)
