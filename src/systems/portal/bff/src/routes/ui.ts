/**
 * BFF `/:svc/ui/*` routes — serve a registered service's embedded mini-portal.
 *
 * Unlike the JSON `/api` passthrough (proxyToUpstream forces `application/json`
 * and re-serializes the body), a mini-portal serves HTML/JS/CSS/font/image
 * assets, so this streams the upstream response verbatim: the upstream
 * `Content-Type` and the raw, binary-safe body. The bearer `Authorization`
 * header is forwarded exactly as for `/api`, so the shell loads the iframe
 * same-origin and never hands a token to a cross-origin frame. Conductor only
 * routes these paths when the service registers a (public) `/ui` endpoint in its
 * manifest, so this is inert for services that ship no UI.
 */
import { Router } from 'express';
import { SpanStatusCode } from '@opentelemetry/api';
import { CONDUCTOR_URL } from '../config.js';
import { logger } from '../observability/logger.js';
import { tracer } from '../observability/tracing.js';
import { upstreamRequestDuration } from '../observability/metrics.js';

export const uiRoutes = Router();

// A service name is a simple slug; the strict match also blocks any path-traversal
// attempt from smuggling through the :svc segment.
const SVC_RE = /^[a-zA-Z0-9_-]+$/;

uiRoutes.all(['/:svc/ui', '/:svc/ui/*'], async (req, res) => {
  const svc = (req.params as Record<string, string>).svc;
  if (!SVC_RE.test(svc)) {
    res.status(400).json({ error: 'invalid service name' });
    return;
  }

  // req.url is the bare conductor path (the /api mount prefix is already stripped),
  // preserving the /ui sub-path and any query string the mini-SPA needs for assets.
  const url = `${CONDUCTOR_URL}${req.url}`;
  const headers: Record<string, string> = {};
  const auth = req.headers['authorization'];
  if (typeof auth === 'string') headers['Authorization'] = auth;

  const span = tracer.startSpan(`ui ${req.method}`);
  const endTimer = upstreamRequestDuration.startTimer({ method: req.method, route: 'ui' });

  try {
    const upstream = await fetch(url, { method: req.method, headers });
    span.setAttribute('http.status_code', upstream.status);
    endTimer();

    res.status(upstream.status);
    // Pass the upstream content type through verbatim (HTML/JS/CSS/font/image) —
    // unlike the JSON proxy — and stream the body as raw bytes to stay binary-safe.
    const ct = upstream.headers.get('content-type');
    if (ct) res.setHeader('Content-Type', ct);
    const cc = upstream.headers.get('cache-control');
    if (cc) res.setHeader('Cache-Control', cc);
    res.send(Buffer.from(await upstream.arrayBuffer()));
  } catch (err) {
    span.recordException(err as Error);
    span.setStatus({ code: SpanStatusCode.ERROR });
    endTimer();
    logger.error({ err, url }, 'upstream ui fetch failed');
    res.status(502).json({ error: 'upstream unavailable' });
  } finally {
    span.end();
  }
});
