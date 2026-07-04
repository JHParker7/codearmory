# Full Kubernetes (kata) portal E2E tests

Selenium tests that drive a **real, running codearmory deployment through the
portal in a browser** — the Kubernetes runtime with the **kata** sandbox backend
(the platform's primary runtime). They create an account, build a workflow with
the portal's visual pipeline builder, trigger it, approve its gate, and watch it
complete. Nothing is mocked and no step/pipeline is created via the API — the
point is to catch the bugs that only surface when a human actually clicks the
builder.

## What `test_full_pipeline.py` builds

A single pipeline, assembled entirely by clicking the builder:

1. a shared **volume** (`forge/create-volume`)
2. a `forge/run` step that writes **3 random strings** to a file in the volume
3. a manual **approval gate**
4. a `forge/run` step that **reads the file** and outputs the list of strings as
   the `STRINGS` output variable
5. a **matrix** fanned out over that list (`values_from: ${steps.read-strings.output.STRINGS}`)
   — one run per string, echoing it
6. a simple **parallel block** of two concurrent steps

`test_parallel_block_runs_concurrently` is a smaller standalone test of just the
parallel block.

The build → trigger → approve → observe flow is 100% UI. The two facts the run
page can't show — that the matrix really fanned into 3 distinct runs, and that
the parallel steps truly overlapped in time — are asserted by reading the run
back through the portal's own `/api` BFF with the portal's own token, purely as a
verification oracle.

## Prerequisites on the target deployment

- **Portal reachable** at `PORTAL_URL` (SPA + BFF). Port-forward or ingress:
  ```bash
  kubectl -n <ns> port-forward svc/<release>-portal 3001:3001
  # → PORTAL_URL=http://localhost:3001
  ```
- **Self-serve signup enabled** (the tests create their own users). On a
  never-initialized instance the first-run `/setup` page is handled automatically
  and the created account becomes the admin.
- **forge + workflows deployed and registered** (they are core), with
  `forge/create-volume` and `forge/run` in the action catalog. The full test
  self-skips if those actions are missing.
- **forge image allowlist** permits the test image. Default `alpine:3.19`;
  override with `FORGE_TEST_IMAGE` to any allowed image whose shell has
  `sh/head/od/tr/sed/cat/sleep` (busybox or coreutils both qualify).
- **A working default StorageClass.** On k8s the volume `medium` is a no-op —
  every shared volume is a PVC (`ReadWriteOnce` by default). Cross-pod file
  sharing between the write and read steps therefore works on a **single-node**
  cluster (both sequential pods land on the one node), which a typical kata
  cluster is. On a **multi-node** cluster set `FORGE_VOLUME_ACCESS_MODE=ReadWriteMany`
  with an RWX-capable class, or the read step may schedule off-node and fail to
  mount. If the default StorageClass has a minimum size above the request, bump
  `FORGE_TEST_VOLUME_MB`.

## Running

```bash
pip install -r tests/full_k8_deployment_tests/requirements.txt
PORTAL_URL=http://localhost:3001 pytest tests/full_k8_deployment_tests -v
```

Just the parallel test:

```bash
PORTAL_URL=http://localhost:3001 pytest tests/full_k8_deployment_tests -v -k parallel
```

In a container (bundles Chromium + chromedriver):

```bash
docker build -t codearmory-k8s-e2e tests/full_k8_deployment_tests
docker run --rm -e PORTAL_URL=http://host.docker.internal:3001 \
  -v "$PWD/_artifacts:/artifacts" codearmory-k8s-e2e
```

## Configuration (env vars)

| Var | Default | Meaning |
| --- | --- | --- |
| `PORTAL_URL` | `http://localhost:3001` | Portal base URL (SPA + BFF). |
| `HEADLESS` | `1` | `0` to watch the browser locally. |
| `FORGE_TEST_IMAGE` | `alpine:3.19` | Image the `forge/run` steps use (must be allowlisted). |
| `FORGE_TEST_VOLUME_MB` | `128` | Shared-volume size request (Mi). |
| `FORGE_TEST_MEDIUM` | `disk` | Volume medium (no-op on k8s; both are PVCs). |
| `E2E_ARTIFACTS_DIR` | `$CLAUDE_JOB_DIR/tmp` or `_artifacts` | Where screenshots/HTML land on failure. |
| `CHROME_BIN` | _(auto)_ | Explicit Chrome/Chromium binary. |
| `CHROMEDRIVER` | _(auto)_ | Explicit chromedriver path (else Selenium Manager resolves). |

On any failure the driver saves `<test_name>.png` and `<test_name>.html` to the
artifacts dir for post-mortem debugging.

## Timing

These are slow: each `forge/run` step is a real Kubernetes pod (PVC provisioning,
image pull, kata microVM start). The status waits allow several minutes per
phase. Expect the full pipeline test to take a few minutes end to end.
