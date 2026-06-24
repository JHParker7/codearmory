/**
 * BFF `/state/*` routes — the one place the SPA does not get a verbatim conductor
 * passthrough. GET fetches blueprints workspace state and normalizes conductor's
 * 200/204/423 branching into a single uniform {@link WorkspaceView} (cached per
 * token); DELETE and other write methods proxy through and invalidate the cache.
 */
import { Router } from 'express';
import { SpanStatusCode } from '@opentelemetry/api';
import { CONDUCTOR_URL } from '../config.js';
import { logger } from '../observability/logger.js';
import { tracer } from '../observability/tracing.js';
import { upstreamRequestDuration, stateCacheHits, stateCacheMisses } from '../observability/metrics.js';
import { fromEmpty, fromLocked, fromState } from '../transform/workspace.js';
import { stateCache } from '../cache.js';
import { proxyToUpstream } from '../proxy.js';

export const stateRoutes = Router();

/**
 * Reject path-traversal and percent-encode each segment of an untrusted
 * workspace path, preserving the username/workspace slash structure.
 *
 * This stops a '../' segment from normalizing the request out of the
 * /blueprints/state/ prefix onto another conductor route (and mis-keying the
 * cache). Returns null when the path is malformed.
 */
function encodeWsPath(wsPath: string): string | null {
  const segments = wsPath.split('/');
  if (segments.some((s) => s === '' || s === '.' || s === '..')) return null;
  return segments.map(encodeURIComponent).join('/');
}

/** GET /state/* — fetch workspace state from conductor, normalize it to a WorkspaceView, and cache per token. */
stateRoutes.get('/state/*', async (req, res) => {
  const safePath = encodeWsPath((req.params as Record<string, string>)[0]);
  if (safePath === null) {
    res.status(400).json({ error: 'invalid workspace path' });
    return;
  }
  const auth = req.headers['authorization'];
  const authKey = typeof auth === 'string' ? auth : '';

  const cached = stateCache.get(safePath, authKey);
  if (cached !== undefined) {
    stateCacheHits.inc();
    res.json(cached);
    return;
  }
  stateCacheMisses.inc();

  const headers: Record<string, string> = {};
  if (typeof auth === 'string') headers['Authorization'] = auth;

  const span = tracer.startSpan('GET /state');
  span.setAttribute('workspace.path', safePath);
  const endTimer = upstreamRequestDuration.startTimer({ method: 'GET', route: 'state' });

  try {
    const upstream = await fetch(`${CONDUCTOR_URL}/blueprints/state/${safePath}`, { headers });
    span.setAttribute('http.status_code', upstream.status);
    endTimer();

    let view: unknown;
    if (upstream.status === 204) {
      view = fromEmpty();
    } else if (!upstream.ok && upstream.status !== 423) {
      res.status(upstream.status).send(await upstream.text());
      return;
    } else {
      // 200 or 423: the body should be JSON, but guard against an empty or
      // non-JSON payload so a malformed upstream response doesn't collapse into a
      // generic 502 and discard the real status (especially the 423 lock info).
      const text = await upstream.text();
      let parsed: any = {};
      if (text) {
        try {
          parsed = JSON.parse(text);
        } catch {
          if (upstream.status !== 423) {
            res.status(502).json({ error: 'invalid upstream response' });
            return;
          }
        }
      }
      view = upstream.status === 423 ? fromLocked(parsed) : fromState(parsed);
    }

    stateCache.set(safePath, authKey, view);
    res.json(view);
  } catch (err) {
    span.recordException(err as Error);
    span.setStatus({ code: SpanStatusCode.ERROR });
    endTimer();
    logger.error({ err, wsPath: safePath }, 'upstream state fetch failed');
    res.status(502).json({ error: 'upstream unavailable' });
  } finally {
    span.end();
  }
});

/** DELETE /state/* — proxy the delete to conductor, then invalidate the cached view on success. */
stateRoutes.delete('/state/*', async (req, res) => {
  const safePath = encodeWsPath((req.params as Record<string, string>)[0]);
  if (safePath === null) {
    res.status(400).json({ error: 'invalid workspace path' });
    return;
  }
  const headers: Record<string, string> = {};
  const auth = req.headers['authorization'];
  if (typeof auth === 'string') headers['Authorization'] = auth;

  const span = tracer.startSpan('DELETE /state');
  span.setAttribute('workspace.path', safePath);
  const endTimer = upstreamRequestDuration.startTimer({ method: 'DELETE', route: 'state' });

  try {
    const upstream = await fetch(`${CONDUCTOR_URL}/blueprints/state/${safePath}`, { method: 'DELETE', headers });
    span.setAttribute('http.status_code', upstream.status);
    endTimer();
    if (upstream.ok) stateCache.invalidate(safePath);
    res.status(upstream.status).end();
  } catch (err) {
    span.recordException(err as Error);
    span.setStatus({ code: SpanStatusCode.ERROR });
    endTimer();
    logger.error({ err, wsPath: safePath }, 'upstream state delete failed');
    res.status(502).json({ error: 'upstream unavailable' });
  } finally {
    span.end();
  }
});

/**
 * Writes to /state/* (POST/PUT/PATCH/LOCK/UNLOCK — terraform apply/lock). GET and
 * DELETE are handled above; this catches the remaining methods. Proxy to conductor
 * and invalidate the cached view so a subsequent GET is write-coherent rather than
 * serving the pre-write state for the cache TTL.
 */
stateRoutes.all('/state/*', async (req, res) => {
  const safePath = encodeWsPath((req.params as Record<string, string>)[0]);
  if (safePath === null) {
    res.status(400).json({ error: 'invalid workspace path' });
    return;
  }
  const qi = req.url.indexOf('?');
  const query = qi >= 0 ? req.url.slice(qi) : '';
  try {
    await proxyToUpstream(req, res, `/blueprints/state/${safePath}${query}`);
    stateCache.invalidate(safePath);
  } catch {
    res.status(502).json({ error: 'upstream unavailable' });
  }
});
