import './observability/tracing.js';
import express from 'express';
import rateLimit from 'express-rate-limit';
import { existsSync } from 'fs';
import path from 'path';
import { fileURLToPath } from 'url';
import { logger } from './observability/logger.js';
import { registry, httpRequestDuration } from './observability/metrics.js';
import { stateRoutes } from './routes/state.js';
import { proxyToUpstream } from './proxy.js';

export { CONDUCTOR_URL } from './config.js';
const PORT = parseInt(process.env.PORT ?? '3001', 10);

const __dirname = path.dirname(fileURLToPath(import.meta.url));

const app = express();
app.use(express.json());

const spaFallbackLimiter = rateLimit({
  windowMs: 15 * 60 * 1000, // 15 minutes
  max: 100, // limit each IP to 100 requests per windowMs
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

// ── Static files + SPA fallback ───────────────────────────────────────────────
// In production the React build lands in ../public (next to dist/).
// In dev there is no public/ — Vite handles static serving separately.

const publicDir = path.join(__dirname, '..', 'public');
if (existsSync(publicDir)) {
  app.use(express.static(publicDir));
  app.get('*', spaFallbackLimiter, (_req, res) => {
    res.sendFile(path.join(publicDir, 'index.html'));
  });
}

app.listen(PORT, () => {
  logger.info({ port: PORT, conductor: process.env.CONDUCTOR_URL ?? 'http://localhost:8082' }, 'portal-bff started');
});
