import type { Request, Response } from 'express';
import { SpanStatusCode } from '@opentelemetry/api';
import { CONDUCTOR_URL } from './config.js';
import { logger } from './observability/logger.js';
import { tracer } from './observability/tracing.js';
import { upstreamRequestDuration } from './observability/metrics.js';

/**
 * Forward an incoming SPA request to conductor and stream the response back.
 *
 * The bearer `Authorization` header (when present) is passed through; a JSON
 * body is forwarded for any method that can carry one (including DELETE, which
 * some routes require a body on). The upstream status and body are mirrored onto
 * `res`. Wraps the call in an OTEL span + duration metric and re-throws on a
 * transport failure so the caller can return a 502.
 *
 * @param path conductor path (already prefixed/built by the caller), appended to CONDUCTOR_URL.
 */
export async function proxyToUpstream(req: Request, res: Response, path: string): Promise<void> {
  const url = `${CONDUCTOR_URL}${path}`;
  const headers: Record<string, string> = { 'Content-Type': 'application/json' };
  const auth = req.headers['authorization'];
  if (typeof auth === 'string') headers['Authorization'] = auth;

  // Forward a JSON body for any method that can carry one — including DELETE,
  // which some routes (e.g. DELETE /mfa/totp) require a password body on.
  const hasBody = req.method !== 'GET' && req.method !== 'HEAD';
  const span = tracer.startSpan(`proxy ${req.method}`);
  const endTimer = upstreamRequestDuration.startTimer({ method: req.method, route: 'proxy' });

  try {
    const upstream = await fetch(url, {
      method: req.method,
      headers,
      body: hasBody ? JSON.stringify(req.body) : undefined,
    });

    span.setAttribute('http.status_code', upstream.status);
    endTimer();

    res.status(upstream.status);
    const text = await upstream.text();
    if (text) {
      res.setHeader('Content-Type', 'application/json');
      res.send(text);
    } else {
      res.end();
    }
  } catch (err) {
    span.recordException(err as Error);
    span.setStatus({ code: SpanStatusCode.ERROR });
    endTimer();
    logger.error({ err, url }, 'upstream proxy failed');
    throw err;
  } finally {
    span.end();
  }
}
