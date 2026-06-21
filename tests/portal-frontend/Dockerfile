# Browser E2E for the portal app. The Playwright base image bundles the
# browsers and their OS dependencies. Default CMD runs only the Playwright-marked
# tests; the Selenium-marked tests need a Chrome/chromedriver pair and are run
# explicitly (e.g. `pytest -m selenium`).
FROM mcr.microsoft.com/playwright/python:v1.48.0-jammy
WORKDIR /tests
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
RUN playwright install chromium
COPY . .
ENTRYPOINT ["pytest"]
CMD ["-vv", "-m", "playwright"]
