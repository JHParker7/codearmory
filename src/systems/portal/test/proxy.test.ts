// Integration tests for the BFF proxy layer and state routes.
// Tests handler logic directly without starting a server.

import sinon from 'sinon';
import { expect } from 'chai';
import { proxyToUpstream } from '../bff/src/proxy.ts';
import { stateRoutes } from '../bff/src/routes/state.ts';
import { stateCache } from '../bff/src/cache.ts';
import type { Request, Response } from 'express';

// ── Helpers ───────────────────────────────────────────────────────────────────

function makeReq(overrides: Partial<{
  method: string;
  path: string;
  headers: Record<string, string>;
  body: unknown;
  params: Record<string, string>;
}>): Request {
  return {
    method: 'GET',
    path: '/users',
    headers: {},
    body: undefined,
    params: {},
    ...overrides,
  } as unknown as Request;
}

function makeRes() {
  let statusCode = 200;
  const headers: Record<string, string> = {};
  let sentBody: unknown;
  let ended = false;

  const res = {
    status(code: number) { statusCode = code; return res; },
    json(body: unknown) { sentBody = body; return res; },
    send(body: unknown) { sentBody = body; return res; },
    end() { ended = true; return res; },
    setHeader(k: string, v: string) { headers[k] = v; },
    _statusCode: () => statusCode,
    _body: () => sentBody,
    _headers: () => headers,
    _ended: () => ended,
  };
  return res;
}

type RouteLayer = {
  route?: {
    path: string;
    stack: Array<{ method: string; handle: (req: unknown, res: unknown, next: () => void) => void }>;
  };
};

function findHandler(method: 'get' | 'delete') {
  const layers = (stateRoutes as unknown as { stack: RouteLayer[] }).stack;
  const layer = layers.find(
    l => l.route?.path === '/state/*' && l.route.stack.some(s => s.method === method),
  );
  if (!layer?.route) throw new Error(`handler for ${method} /state/* not found`);
  return layer.route.stack.find(s => s.method === method)!.handle;
}

function dispatchHandler(
  handler: (req: unknown, res: unknown, next: () => void) => void,
  wsPath: string,
  authHeader?: string,
) {
  const req = makeReq({
    params: { '0': wsPath },
    headers: authHeader ? { authorization: authHeader } : {},
  });
  const res = makeRes();

  return new Promise<typeof res>((resolve, reject) => {
    handler(req, res, () => reject(new Error('next() called unexpectedly')));
    setTimeout(() => resolve(res), 50);
  });
}

// ── BFF proxy integration ─────────────────────────────────────────────────────

describe('BFF proxy integration', () => {
  let fetchStub: sinon.SinonStub;

  beforeEach(() => { fetchStub = sinon.stub(globalThis, 'fetch'); stateCache.clear(); });
  afterEach(() => { fetchStub.restore(); stateCache.clear(); });

  // ── proxyToUpstream ─────────────────────────────────────────────────────────

  describe('proxyToUpstream', () => {
    it('forwards GET to CONDUCTOR_URL with the given path', async () => {
      fetchStub.resolves(new Response('{"ok":true}', { status: 200 }));
      const req = makeReq({ method: 'GET', path: '/teams' });
      const res = makeRes();

      await proxyToUpstream(req as Request, res as unknown as Response, '/teams');

      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.include('/teams');
      expect(opts.method).to.equal('GET');
      expect(res._statusCode()).to.equal(200);
    });

    it('forwards Authorization header from incoming request', async () => {
      fetchStub.resolves(new Response('{}', { status: 200 }));
      const req = makeReq({ method: 'GET', headers: { authorization: 'Bearer mytoken' } });
      const res = makeRes();

      await proxyToUpstream(req as Request, res as unknown as Response, '/teams');

      const [, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect((opts.headers as Record<string, string>)['Authorization']).to.equal('Bearer mytoken');
    });

    it('does not add Authorization header when absent', async () => {
      fetchStub.resolves(new Response('{}', { status: 200 }));
      const req = makeReq({ method: 'GET' });
      const res = makeRes();

      await proxyToUpstream(req as Request, res as unknown as Response, '/users');

      const [, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect((opts.headers as Record<string, string>)['Authorization']).to.be.undefined;
    });

    it('sends body for POST requests', async () => {
      fetchStub.resolves(new Response('{"team_id":"t1"}', { status: 201 }));
      const req = makeReq({ method: 'POST', body: { team_name: 'ops' } });
      const res = makeRes();

      await proxyToUpstream(req as Request, res as unknown as Response, '/teams');

      const [, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(opts.body).to.equal(JSON.stringify({ team_name: 'ops' }));
    });

    it('sends body for PUT requests', async () => {
      fetchStub.resolves(new Response('{"team_id":"t1"}', { status: 200 }));
      const req = makeReq({ method: 'PUT', body: { team_name: 'frontend' } });
      const res = makeRes();

      await proxyToUpstream(req as Request, res as unknown as Response, '/teams/t1');

      const [, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(opts.body).to.equal(JSON.stringify({ team_name: 'frontend' }));
    });

    it('omits body for DELETE requests', async () => {
      fetchStub.resolves(new Response(null, { status: 204 }));
      const req = makeReq({ method: 'DELETE' });
      const res = makeRes();

      await proxyToUpstream(req as Request, res as unknown as Response, '/teams/t1');

      const [, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(opts.body).to.be.undefined;
    });

    it('omits body for GET requests', async () => {
      fetchStub.resolves(new Response('[]', { status: 200 }));
      const req = makeReq({ method: 'GET' });
      const res = makeRes();

      await proxyToUpstream(req as Request, res as unknown as Response, '/teams');

      const [, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(opts.body).to.be.undefined;
    });

    it('relays non-2xx status codes unchanged', async () => {
      fetchStub.resolves(new Response('"not found"', { status: 404 }));
      const req = makeReq({ method: 'GET' });
      const res = makeRes();

      await proxyToUpstream(req as Request, res as unknown as Response, '/teams/nope');

      expect(res._statusCode()).to.equal(404);
    });

    it('calls end() for empty 204 response', async () => {
      fetchStub.resolves(new Response(null, { status: 204 }));
      const req = makeReq({ method: 'DELETE' });
      const res = makeRes();

      await proxyToUpstream(req as Request, res as unknown as Response, '/teams/t1');

      expect(res._statusCode()).to.equal(204);
      expect(res._ended()).to.be.true;
    });

    it('sets Content-Type: application/json when upstream returns body', async () => {
      fetchStub.resolves(new Response('{"ok":true}', { status: 200 }));
      const req = makeReq({ method: 'GET' });
      const res = makeRes();

      await proxyToUpstream(req as Request, res as unknown as Response, '/teams');

      expect(res._headers()['Content-Type']).to.equal('application/json');
    });
  });

  // ── stateRoutes GET /state/* ──────────────────────────────────────────────

  describe('stateRoutes GET /state/*', () => {
    const getHandler = findHandler('get');

    it('returns fromEmpty() shape when upstream returns 204', async () => {
      fetchStub.resolves(new Response(null, { status: 204 }));
      const res = await dispatchHandler(getHandler, 'alice/prod');
      expect(res._statusCode()).to.equal(200);
      const body = res._body() as { isEmpty: boolean; locked: boolean };
      expect(body.isEmpty).to.be.true;
      expect(body.locked).to.be.false;
    });

    it('returns 502 when upstream fetch throws', async () => {
      fetchStub.rejects(new Error('connection refused'));
      const res = await dispatchHandler(getHandler, 'alice/prod');
      expect(res._statusCode()).to.equal(502);
      const body = res._body() as { error: string };
      expect(body.error).to.equal('upstream unavailable');
    });

    it('relays non-ok upstream status (e.g. 403) directly', async () => {
      fetchStub.resolves(new Response('forbidden', { status: 403 }));
      const res = await dispatchHandler(getHandler, 'alice/prod');
      expect(res._statusCode()).to.equal(403);
    });

    it('forwards Authorization header to upstream', async () => {
      fetchStub.resolves(new Response(null, { status: 204 }));
      await dispatchHandler(getHandler, 'alice/prod', 'Bearer test-token');
      const [, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect((opts.headers as Record<string, string>)['Authorization']).to.equal('Bearer test-token');
    });

    it('includes workspace path in upstream URL', async () => {
      fetchStub.resolves(new Response(null, { status: 204 }));
      await dispatchHandler(getHandler, 'acme/team/workspace');
      const [url] = fetchStub.firstCall.args as [string];
      expect(url).to.include('/state/acme/team/workspace');
    });
  });

  // ── stateRoutes DELETE /state/* ───────────────────────────────────────────

  describe('stateRoutes DELETE /state/*', () => {
    const deleteHandler = findHandler('delete');

    it('passes upstream 204 status through', async () => {
      fetchStub.resolves(new Response(null, { status: 204 }));
      const res = await dispatchHandler(deleteHandler, 'alice/prod');
      expect(res._statusCode()).to.equal(204);
    });

    it('passes upstream 423 (locked) status through', async () => {
      fetchStub.resolves(new Response(null, { status: 423 }));
      const res = await dispatchHandler(deleteHandler, 'alice/locked');
      expect(res._statusCode()).to.equal(423);
    });

    it('returns 502 when upstream is unreachable', async () => {
      fetchStub.rejects(new Error('connection refused'));
      const res = await dispatchHandler(deleteHandler, 'alice/prod');
      expect(res._statusCode()).to.equal(502);
    });

    it('uses DELETE method when calling upstream', async () => {
      fetchStub.resolves(new Response(null, { status: 204 }));
      await dispatchHandler(deleteHandler, 'alice/prod');
      const [, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(opts.method).to.equal('DELETE');
    });

    it('includes workspace path in upstream URL', async () => {
      fetchStub.resolves(new Response(null, { status: 204 }));
      await dispatchHandler(deleteHandler, 'acme/team/workspace');
      const [url] = fetchStub.firstCall.args as [string];
      expect(url).to.include('/state/acme/team/workspace');
    });
  });

  // ── state cache ───────────────────────────────────────────────────────────

  describe('state cache', () => {
    const getHandler = findHandler('get');
    const deleteHandler = findHandler('delete');

    it('returns cached response without hitting upstream on second GET', async () => {
      fetchStub.resolves(new Response(null, { status: 204 }));
      await dispatchHandler(getHandler, 'alice/prod', 'Bearer tok');
      await dispatchHandler(getHandler, 'alice/prod', 'Bearer tok');
      expect(fetchStub.callCount).to.equal(1);
    });

    it('scopes cache by auth token — different tokens fetch independently', async () => {
      fetchStub.resolves(new Response(null, { status: 204 }));
      await dispatchHandler(getHandler, 'alice/prod', 'Bearer tok-a');
      await dispatchHandler(getHandler, 'alice/prod', 'Bearer tok-b');
      expect(fetchStub.callCount).to.equal(2);
    });

    it('invalidates cache after successful DELETE so next GET calls upstream', async () => {
      fetchStub.resolves(new Response(null, { status: 204 }));
      await dispatchHandler(getHandler, 'alice/prod', 'Bearer tok');
      expect(fetchStub.callCount).to.equal(1);

      await dispatchHandler(deleteHandler, 'alice/prod', 'Bearer tok');
      expect(fetchStub.callCount).to.equal(2);

      await dispatchHandler(getHandler, 'alice/prod', 'Bearer tok');
      expect(fetchStub.callCount).to.equal(3);
    });

    it('does not invalidate cache when DELETE fails (non-ok upstream)', async () => {
      fetchStub.resolves(new Response(null, { status: 204 }));
      await dispatchHandler(getHandler, 'alice/prod', 'Bearer tok');

      fetchStub.resolves(new Response(null, { status: 423 }));
      await dispatchHandler(deleteHandler, 'alice/prod', 'Bearer tok');

      fetchStub.resolves(new Response(null, { status: 204 }));
      await dispatchHandler(getHandler, 'alice/prod', 'Bearer tok');
      // First GET (1) + failed DELETE (2); third call should hit cache — no new fetch
      expect(fetchStub.callCount).to.equal(2);
    });
  });
});
