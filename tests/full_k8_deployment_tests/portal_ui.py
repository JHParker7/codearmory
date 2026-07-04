"""
Selenium page-object for driving the codearmory **portal** end to end.

This is deliberately a *real UI* driver: every pipeline is built by clicking the
same palette buttons, filling the same step-definition forms, toggling the same
per-block matrix/parallel controls and volume selectors a human uses. Nothing is
mocked and the pipeline is never assembled via the JSON side-panel or the API —
if the visual builder is broken, these methods break too, which is the whole
point of testing through the browser.

The portal ships with **no** `data-testid`/`id`/`class` hooks and is entirely
inline-styled, so selectors are anchored on the durable things: input `type`,
`placeholder`, and `title` attributes, visible button text (glyphs included),
and the native `<select>`/`<textarea>` elements. Each selector below is taken
from the portal source it drives:

  pages/Signup.tsx, pages/Setup.tsx, components/AuthFields.tsx   – account creation
  pages/app/Workflows.tsx                                        – builder overlay + trigger
  pages/app/PipelineBlocks.tsx                                   – palette, block cards, matrix/gate editors
  pages/app/StepDefForm.tsx + stepSchema.ts                      – step-definition forms
  pages/app/StepInputsEditor.tsx                                 – per-occurrence volume attach
  pages/app/RunView.tsx + components/Pill.tsx                    – live run + approval controls
"""

from __future__ import annotations

import time

from selenium.common.exceptions import (
    ElementClickInterceptedException,
    NoSuchElementException,
    StaleElementReferenceException,
    TimeoutException,
)
from selenium.webdriver.common.by import By
from selenium.webdriver.common.keys import Keys
from selenium.webdriver.support import expected_conditions as EC
from selenium.webdriver.support.ui import Select, WebDriverWait

# ── Selectors, quoted from the portal source so drift is easy to trace ──────────

# Account pages (PromptField renders a bare <input type=... placeholder=...>).
EMAIL_INPUT = (By.CSS_SELECTOR, "input[type='email']")
USERNAME_INPUT = (By.CSS_SELECTOR, "input[type='text']")
PASSWORD_INPUTS = (By.CSS_SELECTOR, "input[type='password']")
TERMS_LABEL = (By.XPATH, "//label[contains(., 'I accept')]")
AUTH_SUBMIT = (By.CSS_SELECTOR, "button[type='submit']")

# Login page.
LOGIN_EMAIL = (By.CSS_SELECTOR, "input[type='email']")
LOGIN_PASSWORD = (By.CSS_SELECTOR, "input[type='password']")

# Workflows / pipelines tab.
NEW_PIPELINE_BTN = (By.CSS_SELECTOR, "button[title='new pipeline']")
PIPELINE_NAME_INPUT = (By.CSS_SELECTOR, "input[placeholder='pipeline name']")
CREATE_PIPELINE_BTN = (By.XPATH, "//button[normalize-space()='[ create ]']")
TRIGGER_BTN = (By.XPATH, "//button[contains(normalize-space(), 'trigger')]")

# Builder palette (PipelineBlocks.tsx). Match the mode buttons by their unique
# title text so they never collide with created-step or action buttons.
PARALLEL_MODE_BTN = (By.XPATH, "//button[contains(@title, 'start a parallel container')]")
MATRIX_MODE_BTN = (By.XPATH, "//button[contains(@title, 'fan a step out over a list of values')]")
GATE_BTN = (By.XPATH, "//button[contains(@title, 'add a manual-approval gate')]")
DRAG_HANDLE = (By.XPATH, "//span[text()='⠿']")  # one per editable block card

# Step-definition form (StepDefForm.tsx + stepSchema.ts placeholders).
STEP_NAME_INPUT = (By.CSS_SELECTOR, "input[placeholder='unit_tests']")
# The bracketed label distinguishes the form's submit from the parallel palette
# button, whose active label ("active — add steps, click to finish") also
# contains "add step".
ADD_STEP_BTN = (By.XPATH, "//button[contains(normalize-space(), '[ add step ]')]")
RUN_IMAGE_INPUT = (By.CSS_SELECTOR, "input[placeholder='ubuntu:22.04 (required)']")
RUN_CMD_TEXTAREA = (By.CSS_SELECTOR, "textarea[placeholder='go test ./...']")
RUN_OUTPUT_ENV_INPUT = (By.CSS_SELECTOR, "input[placeholder='BUILD_ID, VERSION']")
VOL_NAME_INPUT = (By.CSS_SELECTOR, "input[placeholder='workspace (default)']")
VOL_SIZE_INPUT = (By.CSS_SELECTOR, "input[placeholder='1024 (optional)']")
VOL_MEDIUM_INPUT = (By.CSS_SELECTOR, "input[placeholder='memory (default) | disk']")
VOL_MOUNT_INPUT = (By.CSS_SELECTOR, "input[placeholder='/workspace (default)']")
STEP_FORM_HEADER = (By.XPATH, "//div[starts-with(normalize-space(), 'new step ·')]")

# Per-occurrence inputs editor (StepInputsEditor.tsx) — only the selected card
# renders it, so these are globally unique while a forge step is selected.
VOLUME_SELECT = (By.XPATH, "//select[contains(@title, 'attach a workspace')]")

# Inline matrix editor (PipelineBlocks.tsx) — only one matrix block exists.
MATRIX_VAR_INPUT = (By.CSS_SELECTOR, "input[placeholder^='var (e.g. region)']")
MATRIX_VALUES_FROM_INPUT = (By.CSS_SELECTOR, "input[placeholder^='or values from a reference']")

# Inline gate editor (PipelineBlocks.tsx) — only one gate exists.
GATE_MESSAGE_INPUT = (By.CSS_SELECTOR, "input[placeholder^='message shown to approvers']")

# Declared pipeline inputs/outputs editors (Workflows.tsx builder header) and the
# workflows/trigger step fields.
IO_TOGGLE = (By.CSS_SELECTOR, "button[title='declare pipeline inputs and outputs']")
DECL_INPUT_ADD = (By.XPATH, "//button[normalize-space()='+ input']")
DECL_OUTPUT_ADD = (By.XPATH, "//button[normalize-space()='+ output']")
DECL_INPUT_DEFAULTS = (By.CSS_SELECTOR, "input[placeholder='default (optional)']")
DECL_OUTPUT_VALUES = (By.CSS_SELECTOR, "input[placeholder^=\"${steps.build.output\"]")
TRIGGER_PIPELINE_INPUT = (By.CSS_SELECTOR, "input[placeholder='target pipeline name or id']")
TRIGGER_INPUTS_INPUT = (By.CSS_SELECTOR, "input[placeholder^='KEY=VALUE']")

# RunView.tsx.
APPROVAL_BANNER = (By.XPATH, "//div[contains(normalize-space(), 'NEEDS YOUR APPROVAL')]")
APPROVE_BTN = (By.XPATH, "//button[contains(normalize-space(), 'approve')]")


class PortalUI:
    """Thin, explicit page-object over a Selenium driver pointed at the portal."""

    def __init__(self, driver, base_url: str, timeout: int = 20):
        self.driver = driver
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout
        self.pipeline_name: str | None = None

    # ── low-level helpers ──────────────────────────────────────────────────────

    def _wait(self, timeout: int | None = None) -> WebDriverWait:
        return WebDriverWait(self.driver, timeout or self.timeout)

    def _find(self, locator, timeout: int | None = None):
        return self._wait(timeout).until(EC.presence_of_element_located(locator))

    def _click(self, locator, timeout: int | None = None):
        el = self._wait(timeout).until(EC.element_to_be_clickable(locator))
        self._click_el(el)
        return el

    def _click_el(self, el):
        self.driver.execute_script("arguments[0].scrollIntoView({block: 'center'});", el)
        try:
            el.click()
        except (ElementClickInterceptedException, StaleElementReferenceException):
            # Inline-styled overlays occasionally sit over a target; a scripted
            # click still exercises the same React onClick handler.
            self.driver.execute_script("arguments[0].click();", el)

    def _type(self, el, text: str, clear: bool = True):
        """Type into a React-controlled input, clearing via select-all+delete so
        the onChange state actually empties first."""
        if clear:
            el.send_keys(Keys.CONTROL, "a")
            el.send_keys(Keys.DELETE)
        el.send_keys(text)

    def _type_into(self, locator, text: str, clear: bool = True):
        self._type(self._find(locator), text, clear=clear)

    def current_url(self) -> str:
        return self.driver.current_url

    def token(self) -> str | None:
        """The portal's own auth token, read from localStorage — used only as a
        verification oracle for the assertions the UI cannot surface (per-matrix
        step runs, parallel timing). It never drives the build."""
        return self.driver.execute_script("return window.localStorage.getItem('ca_token');")

    # ── account creation (Signup, or Setup on a brand-new instance) ────────────

    def bootstrap_user(self, email: str, username: str, password: str) -> None:
        """Create a fresh account through the portal and end up authenticated in
        /app. Handles both an already-initialized instance (/signup) and a
        never-initialized one (the /setup first-run gate redirects there)."""
        self.driver.get(f"{self.base_url}/signup")
        self._wait().until(lambda d: "/signup" in d.current_url or "/setup" in d.current_url)
        is_setup = "/setup" in self.current_url()
        self._find(EMAIL_INPUT)  # form mounted

        self._type_into(EMAIL_INPUT, email)
        self._type_into(USERNAME_INPUT, username)
        pw_fields = self._wait().until(
            lambda d: d.find_elements(*PASSWORD_INPUTS) if len(d.find_elements(*PASSWORD_INPUTS)) >= 2 else False
        )
        self._type(pw_fields[0], password)
        self._type(pw_fields[1], password)
        if not is_setup:
            self._click(TERMS_LABEL)  # /setup has no terms checkbox

        self._click(AUTH_SUBMIT)  # element_to_be_clickable waits out the disabled state
        self._wait(self.timeout).until(lambda d: "/app" in d.current_url)

    def login(self, email: str, password: str) -> None:
        self.driver.get(f"{self.base_url}/login")
        self._type_into(LOGIN_EMAIL, email)
        self._type_into(LOGIN_PASSWORD, password)
        self._click(AUTH_SUBMIT)
        self._wait().until(lambda d: "/app" in d.current_url)

    # ── navigation ─────────────────────────────────────────────────────────────

    def open_workflows(self) -> None:
        self.driver.get(f"{self.base_url}/app/workflows")
        self._find(NEW_PIPELINE_BTN)

    def actions_available(self, *actions: str) -> bool:
        """Whether the given catalog actions are offered as palette buttons. Must
        be called with the builder overlay OPEN (the palette lives inside it);
        waits for the palette to populate before deciding, so a stack whose
        registry manifest predates forge/create-volume is detected, not raced."""
        for a in actions:
            try:
                self._wait(self.timeout).until(
                    EC.presence_of_element_located((By.XPATH, self._action_xpath(a)))
                )
            except TimeoutException:
                return False
        return True

    # ── builder ────────────────────────────────────────────────────────────────

    def open_new_pipeline(self, name: str) -> None:
        self._click(NEW_PIPELINE_BTN)
        self._type_into(PIPELINE_NAME_INPUT, name)
        self.pipeline_name = name

    @staticmethod
    def _action_xpath(action: str) -> str:
        # The palette action button is the one whose *own* title marks it a
        # create-from-action button and whose primary label div is the action
        # name — distinguishing it from a reusable-step button that merely lists
        # the same action as its subtitle.
        return (
            "//button[@title='create & configure a new step from this action']"
            f"[.//div[normalize-space()='{action}']]"
        )

    def _block_count(self) -> int:
        return len(self.driver.find_elements(*DRAG_HANDLE))

    def _pick_action(self, action: str) -> None:
        self._click((By.XPATH, self._action_xpath(action)))
        self._find(STEP_NAME_INPUT)  # StepDefForm mounted in the right panel

    def _submit_step_and_wait(self, before: int) -> None:
        self._click(ADD_STEP_BTN)
        # The freshly-created step drops into the canvas as a new block; wait for
        # the block count to tick up rather than guessing at timing.
        self._wait(self.timeout).until(lambda d: self._block_count() > before)

    def add_volume_step(
        self, name: str, volume: str = "workspace", size_mb: int = 128,
        medium: str = "disk", mount_path: str = "/workspace",
    ) -> None:
        before = self._block_count()
        self._pick_action("forge/create-volume")
        self._type_into(STEP_NAME_INPUT, name)
        self._type_into(VOL_NAME_INPUT, volume)
        self._type_into(VOL_SIZE_INPUT, str(size_mb))
        self._type_into(VOL_MEDIUM_INPUT, medium)
        self._type_into(VOL_MOUNT_INPUT, mount_path)
        self._submit_step_and_wait(before)

    def add_forge_run_step(
        self, name: str, image: str, run: str, output_env: str | None = None,
    ) -> None:
        """Create a forge/run step from the palette. Any active parallel/matrix
        palette mode is honoured by the builder when the block drops in."""
        before = self._block_count()
        self._pick_action("forge/run")
        self._type_into(STEP_NAME_INPUT, name)
        # The image field is an editable ImageSelect; typing may open a filter
        # dropdown, so dismiss it by clicking the form header before moving on.
        self._type_into(RUN_IMAGE_INPUT, image)
        self._dismiss_image_dropdown()
        self._type_into(RUN_CMD_TEXTAREA, run)
        if output_env:
            self._type_into(RUN_OUTPUT_ENV_INPUT, output_env)
        self._submit_step_and_wait(before)

    def _dismiss_image_dropdown(self) -> None:
        try:
            self.driver.find_element(*STEP_FORM_HEADER).click()
        except (NoSuchElementException, ElementClickInterceptedException):
            pass

    def attach_volume(self, volume: str = "workspace") -> None:
        """Attach an upstream-created workspace volume to the *currently selected*
        forge block (a step is auto-selected right after it is added, so call this
        immediately afterwards). Waits for the upstream volume name to populate the
        selector before choosing it."""
        def option_ready(d):
            els = d.find_elements(*VOLUME_SELECT)
            if not els:
                return False
            texts = [o.text.strip() for o in els[0].find_elements(By.TAG_NAME, "option")]
            return volume in texts and els[0]
        el = self._wait(self.timeout).until(option_ready)
        Select(el).select_by_visible_text(volume)

    def add_gate(self, message: str) -> None:
        before = self._block_count()
        self._click(GATE_BTN)
        self._wait(self.timeout).until(lambda d: self._block_count() > before)
        self._type_into(GATE_MESSAGE_INPUT, message)

    def _ensure_io_editor_open(self) -> None:
        """The declared inputs/outputs editors sit behind a collapsed `▸ inputs/outputs`
        toggle in the builder header; expand it if it isn't already open."""
        if self.driver.find_elements(*DECL_INPUT_ADD):
            return
        self._click(IO_TOGGLE)
        self._find(DECL_INPUT_ADD)

    def add_declared_input(self, name: str, default: str = "") -> None:
        """Add a declared pipeline input (name + optional default) via the builder's
        inputs editor."""
        self._ensure_io_editor_open()
        before = len(self.driver.find_elements(*DECL_INPUT_DEFAULTS))
        self._click(DECL_INPUT_ADD)
        self._wait(self.timeout).until(lambda d: len(d.find_elements(*DECL_INPUT_DEFAULTS)) > before)
        default_field = self.driver.find_elements(*DECL_INPUT_DEFAULTS)[-1]  # the new row
        name_field = default_field.find_element(By.XPATH, "preceding-sibling::input[1]")
        self._type(name_field, name)
        if default:
            self._type(default_field, default)

    def add_declared_output(self, name: str, value: str) -> None:
        """Add a declared pipeline output (name → ${...} value template) via the
        builder's outputs editor."""
        self._ensure_io_editor_open()
        before = len(self.driver.find_elements(*DECL_OUTPUT_VALUES))
        self._click(DECL_OUTPUT_ADD)
        self._wait(self.timeout).until(lambda d: len(d.find_elements(*DECL_OUTPUT_VALUES)) > before)
        value_field = self.driver.find_elements(*DECL_OUTPUT_VALUES)[-1]  # the new row
        name_field = value_field.find_element(By.XPATH, "preceding-sibling::input[1]")
        self._type(name_field, name)
        self._type(value_field, value)

    def add_trigger_step(self, name: str, pipeline: str, inputs_kv: str = "") -> None:
        """Create a workflows/trigger step targeting `pipeline`, passing `inputs_kv`
        (a 'KEY=VALUE ...' string). Drives the same palette-action → step-form flow as
        a forge step, but with the pipeline picker + inputs map."""
        before = self._block_count()
        self._pick_action("workflows/trigger")
        self._type_into(STEP_NAME_INPUT, name)
        self._type_into(TRIGGER_PIPELINE_INPUT, pipeline)
        self._dismiss_image_dropdown()  # closes the PipelineSelect filter dropdown
        if inputs_kv:
            self._type_into(TRIGGER_INPUTS_INPUT, inputs_kv)
        self._submit_step_and_wait(before)

    def start_matrix_mode(self) -> None:
        """Arm matrix mode: the next step added from the palette becomes a matrix
        fan-out block."""
        self._click(MATRIX_MODE_BTN)

    def set_matrix(self, var: str, values_from: str) -> None:
        """Fill the inline matrix editor of the single matrix block."""
        self._type_into(MATRIX_VAR_INPUT, var)
        self._type_into(MATRIX_VALUES_FROM_INPUT, values_from)

    def start_parallel_mode(self) -> None:
        self._click(PARALLEL_MODE_BTN)

    def finish_parallel_mode(self) -> None:
        self._click(PARALLEL_MODE_BTN)

    def save_pipeline(self) -> None:
        self._click(CREATE_PIPELINE_BTN)
        # Overlay closes and the pipelines tab selects the new workflow, exposing
        # the trigger control.
        self._wait(self.timeout).until(EC.invisibility_of_element_located(PIPELINE_NAME_INPUT))
        self._find(TRIGGER_BTN)

    def trigger(self) -> str:
        """Trigger the selected pipeline and return the run id from the URL the
        portal navigates to."""
        self._click(TRIGGER_BTN)
        self._wait(self.timeout).until(lambda d: "/runs/" in d.current_url)
        return self.current_url().rstrip("/").split("/runs/")[-1]

    # ── run view ───────────────────────────────────────────────────────────────

    def run_status(self) -> str:
        """The header run-status pill text (UPPERCASE, as the Pill renders it), or
        '' before the workflow name has loaded."""
        if not self.pipeline_name:
            return ""
        try:
            name = self.driver.find_element(
                By.XPATH, f"//span[normalize-space()='{self.pipeline_name}']"
            )
            pill = name.find_element(By.XPATH, "following-sibling::span[1]")
            return pill.text.strip().upper()
        except (NoSuchElementException, StaleElementReferenceException):
            return ""

    def wait_for_approval_gate(self, timeout: int = 300) -> None:
        """Block until the run pauses on the approval gate, failing fast if the run
        reaches a terminal non-approval state first."""
        def check(d):
            if self.driver.find_elements(*APPROVAL_BANNER):
                return True
            if self.run_status() in ("FAILED", "CANCELLED"):
                raise AssertionError(
                    f"run reached {self.run_status()} before the approval gate"
                )
            return False
        self._poll(check, timeout, "approval gate never appeared")

    def approve(self) -> None:
        self._click(APPROVE_BTN)
        # The approval banner disappears once the decision is accepted.
        self._wait(self.timeout).until(EC.invisibility_of_element_located(APPROVAL_BANNER))

    def wait_for_status(self, target: str, timeout: int = 420) -> None:
        target = target.upper()
        terminal = {"COMPLETED", "FAILED", "CANCELLED"}

        def check(d):
            status = self.run_status()
            if status == target:
                return True
            if status in terminal and status != target:
                raise AssertionError(f"run ended {status}, expected {target}")
            return False
        self._poll(check, timeout, f"run did not reach {target}")

    def select_step(self, label: str) -> None:
        """Click a step card in the RunView pipeline column by its visible label."""
        self._click((By.XPATH, f"//button[.//span[normalize-space()='{label}']]"))

    def wait_text_prefix(self, prefix: str, timeout: int | None = None) -> str:
        """Wait for a <span> whose text starts with `prefix` (e.g. the logs-panel
        header 'echo-string [item=<value>]' the matrix expansion surfaces) and
        return its full text."""
        el = self._find((By.XPATH, f"//span[starts-with(normalize-space(), '{prefix}')]"), timeout)
        return el.text.strip()

    def page_has_text(self, text: str) -> bool:
        return bool(self.driver.find_elements(By.XPATH, f"//*[contains(normalize-space(), \"{text}\")]"))

    # ── polling ────────────────────────────────────────────────────────────────

    def _poll(self, check, timeout: int, message: str) -> None:
        deadline = time.time() + timeout
        while time.time() < deadline:
            try:
                if check(self.driver):
                    return
            except AssertionError:
                raise
            except (NoSuchElementException, StaleElementReferenceException):
                pass  # transient during React re-render; keep polling
            time.sleep(2)
        raise TimeoutException(f"{message} (waited {timeout}s; last status={self.run_status()!r})")
