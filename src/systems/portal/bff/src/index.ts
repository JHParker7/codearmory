/**
 * Portal BFF entrypoint — the Express server that backs the React SPA.
 *
 * It does three jobs: serves the built SPA (in production), exposes an optional
 * Prometheus `/metrics` endpoint, and proxies every `/api/*` request through to
 * conductor (verbatim, except the normalized `/state/*` workspace routes). The
 * SPA only ever talks to this server, never to conductor directly.
 */
import './observability/tracing.js';
import express from 'express';
import rateLimit from 'express-rate-limit';
import { existsSync } from 'fs';
import { createServer as createHttpServer } from 'http';
import path from 'path';
import { fileURLToPath } from 'url';
import { logger } from './observability/logger.js';
import { registry, httpRequestDuration } from './observability/metrics.js';
import { stateRoutes } from './routes/state.js';
import { proxyToUpstream } from './proxy.js';

import { CONDUCTOR_URL } from './config.js';
export { CONDUCTOR_URL };
const PORT = parseInt(process.env.PORT ?? '3001', 10);

const __dirname = path.dirname(fileURLToPath(import.meta.url));

const app = express();
// Behind the k8s ingress/Service, the TCP peer is the proxy, so without this every
// client collapses to one IP and shares a single rate-limit bucket (one user's
// traffic would 429 everyone). Trust one proxy hop so req.ip is the real client IP
// from X-Forwarded-For. Configurable via TRUST_PROXY for multi-hop setups.
// TRUST_PROXY accepts: a boolean ('true'/'false'), a numeric hop count ('1'),
// or an Express preset string ('loopback'). Default: trust one proxy hop.
const trustProxyRaw = (process.env.TRUST_PROXY ?? '1').trim();
const trustProxy: boolean | number | string =
  trustProxyRaw === 'true' ? true
  : trustProxyRaw === 'false' ? false
  : /^\d+$/.test(trustProxyRaw) ? Number(trustProxyRaw)
  : trustProxyRaw;
app.set('trust proxy', trustProxy);
app.use(express.json());

const spaFallbackLimiter = rateLimit({
  windowMs: 15 * 60 * 1000, // 15 minutes
  max: 100, // limit each client IP to 100 requests per windowMs
  standardHeaders: true,
  legacyHeaders: false,
});

// Request logging + response-time recording (route label resolved at finish time).
app.use((req, res, next) => {
  const start = process.hrtime.bigint();
  const end = httpRequestDuration.startTimer();
  res.on('finish', () => {
    const ms = Number(process.hrtime.bigint() - start) / 1e6;
    const route = (req.route as { path?: string } | undefined)?.path ?? req.path;
    end({ method: req.method, route, status_code: String(res.statusCode) });
    logger.info({ method: req.method, url: req.url, status: res.statusCode, ms }, 'request');
  });
  next();
});

// ── Optional metrics endpoint ─────────────────────────────────────────────────
if (process.env.METRICS_ENABLED === 'true') {
  app.get('/metrics', async (_req, res) => {
    res.set('Content-Type', registry.contentType);
    res.end(await registry.metrics());
  });
}

// ── API router ────────────────────────────────────────────────────────────────
// Mounted at /api so Express strips the prefix before entering these handlers.
// stateRoutes sees /state/*, proxyToUpstream sees the bare conductor path.

const api = express.Router();
api.use(stateRoutes);
api.all('*', async (req, res) => {
  try {
    // req.url (not req.path) so the query string is preserved — conductor needs
    // filter/pagination params (?limit=, ?project=, ?actor_id=, …). Express has
    // already stripped the /api mount prefix here.
    await proxyToUpstream(req, res, req.url);
  } catch {
    res.status(502).json({ error: 'upstream unavailable' });
  }
});

app.use('/api', api);

// ── Serve the SPA on the same port as the API ─────────────────────────────────
// One server, one port serves both the React app and /api. In production the
// build lands in ../public and is served statically. In development there is no
// public/ — Vite runs in middleware mode so the same server serves the app with
// HMR (its websocket attaches to this http server, so HMR shares the port too).
// The API router is mounted above, so /api always wins over the SPA catch-all.

const publicDir = path.join(__dirname, '..', 'public');
const httpServer = createHttpServer(app);

async function start() {
  if (existsSync(publicDir)) {
    app.use(express.static(publicDir));
    app.get('*', spaFallbackLimiter, (_req, res) => {
      res.sendFile(path.join(publicDir, 'index.html'));
    });
  } else {
    // Vite dev server in middleware mode, rooted at the portal SPA (../..,
    // alongside vite.config.ts). Its server.proxy is unused here — /api is served
    // by this Express app directly, not proxied across ports.
    //
    // vite is a devDependency of the portal (the parent), not of the BFF's prod
    // image; this branch only runs in dev (no public/), where it resolves from
    // the portal's node_modules. The specifier is held in a variable so the
    // production tsc build — which has no vite — doesn't try to type-resolve it.
    const viteModule = 'vite';
    const { createServer: createViteServer } = await import(viteModule);
    const vite = await createViteServer({
      root: path.join(__dirname, '..', '..'),
      server: { middlewareMode: true, hmr: { server: httpServer } },
      appType: 'spa',
    });
    app.use(vite.middlewares);
    logger.info('vite middleware mounted — SPA + API served on a single port');
  }

  httpServer.listen(PORT, () => {
    logger.info({ port: PORT, conductor: CONDUCTOR_URL }, 'portal-bff started');
  });
}

start().catch((err) => {
  logger.error({ err }, 'portal-bff failed to start');
  process.exit(1);
});
