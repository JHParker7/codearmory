# Integration-test runner image for the dogfood CI. Run as a Kubernetes Job by the
# outpost `test` module after a redeploy: a Job pod has ordinary cluster networking,
# so it reaches conductor's in-cluster ClusterIP (which a forge sandbox cannot). The
# API e2e suite drives full user journeys through conductor, exercising routing, auth,
# RBAC, tickets, and workflows end-to-end.
FROM public.ecr.aws/docker/library/python:3.12-slim
WORKDIR /tests
COPY tests/e2e/ /tests/
RUN pip install --no-cache-dir requests==2.34.2 pytest==9.0.3
# API_URL is injected by the Job (points at the in-cluster conductor). Only the
# API suite runs here — the CLI/TUI suites need the armory binary and a TTY.
ENV API_URL=http://ca-codearmory-conductor:8080
CMD ["pytest", "-v", "-p", "no:cacheprovider", "test_api_e2e.py"]
