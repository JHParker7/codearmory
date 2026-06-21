from playwright.sync_api import Page, expect


class BlueprintsPage:
    def __init__(self, page: Page, base_url: str):
        self.page = page
        self.base_url = base_url

    def navigate(self):
        self.page.goto(f"{self.base_url}/app/blueprints")
        self.page.wait_for_load_state("domcontentloaded")

    # ── Locators ──────────────────────────────────────────────────────────────

    @property
    def sidebar_header(self):
        return self.page.get_by_role("main").locator('text=blueprints/')

    @property
    def add_button(self):
        return self.page.get_by_role("button", name="+ add", exact=True)

    @property
    def empty_state_add_button(self):
        return self.page.get_by_role("button", name="[ + add workspace ]").first

    @property
    def modal(self):
        return self.page.locator('text=add workspace').first

    @property
    def modal_path_input(self):
        return self.page.locator('input[placeholder$="/prod"]')

    @property
    def modal_add_button(self):
        return self.page.get_by_role("button", name="[ ↵ add workspace ]")

    @property
    def modal_cancel_button(self):
        return self.page.get_by_role("button", name="cancel")

    @property
    def no_workspaces_message(self):
        return self.page.locator('text=no workspaces yet')

    # ── Actions ───────────────────────────────────────────────────────────────

    def click_add(self):
        self.add_button.click()

    def add_workspace(self, path: str):
        self.click_add()
        self.modal_path_input.fill(path)
        self.modal_add_button.click()

    def workspace_item(self, path: str):
        return self.page.locator(f'button:has-text("{path}")')

    def click_workspace(self, path: str):
        self.workspace_item(path).click()

    def remove_workspace(self, path: str):
        self.click_workspace(path)
        self.page.get_by_role("button", name="[ remove ]").click()
        self.page.get_by_role("button", name="[ confirm ]").click()

    # ── Assertions ────────────────────────────────────────────────────────────

    def assert_loaded(self):
        expect(self.sidebar_header).to_be_visible(timeout=10_000)

    def assert_empty_state(self):
        expect(self.no_workspaces_message).to_be_visible()

    def assert_workspace_in_list(self, path: str):
        expect(self.workspace_item(path)).to_be_visible()

    def assert_workspace_not_in_list(self, path: str):
        expect(self.workspace_item(path)).not_to_be_visible()

    def assert_modal_visible(self):
        expect(self.modal_path_input).to_be_visible()

    def assert_modal_not_visible(self):
        expect(self.modal_path_input).not_to_be_visible()
