# Selenium portal E2E runner for a full Kubernetes (kata) codearmory deployment.
#
# Ships Chromium + chromedriver so the tests run headless in CI/containers.
# Point it at a reachable portal and run:
#   docker run --rm -e PORTAL_URL=http://host.docker.internal:3001 <image>
FROM python:3.12-slim

ENV DEBIAN_FRONTEND=noninteractive \
    CHROME_BIN=/usr/bin/chromium \
    CHROMEDRIVER=/usr/bin/chromedriver \
    HEADLESS=1 \
    E2E_ARTIFACTS_DIR=/artifacts

RUN apt-get update \
    && apt-get install -y --no-install-recommends chromium chromium-driver ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /tests
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY . .

RUN mkdir -p /artifacts
ENTRYPOINT ["pytest"]
CMD ["-vv", "-m", "e2e"]
