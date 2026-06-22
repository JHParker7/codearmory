import sinon from 'sinon';
import { expect } from 'chai';
import {
  signup,
  login,
  getSetupStatus,
  getUser,
  updateUser,
  getWorkspaceState,
  deleteWorkspaceState,
  listAuditLogs,
  listSteps,
  getStep,
  createStep,
  updateStep,
  deleteStep,
  listActions,
  createWorkflow,
  updateWorkflow,
  createExecution,
  createRunnerClass,
  getRunnerClass,
  updateRunnerClass,
  deleteRunnerClass,
  listContainerRepos,
  listImageTags,
  getManifest,
  deleteManifest,
  getGiteaAccount,
  linkGiteaAccount,
  unlinkGiteaAccount,
  listGiteaRepos,
  createGiteaRepo,
  getGiteaRepo,
  deleteGiteaRepo,
  listBranches,
  listGitTags,
  listCommits,
  listPulls,
  createPull,
  getPull,
  mergePull,
  listInvites,
  getInvite,
  acceptInvite,
  declineInvite,
  deleteInvite,
  listOrgs,
  updateOrg,
  deleteOrg,
  inviteToOrg,
  createPermission,
  updatePermission,
  deletePermission,
  createRole,
  updateRole,
  deleteRole,
  listServiceRequests,
  getServiceRequest,
  approveServiceRequest,
  declineServiceRequest,
  getSession,
  deleteSession,
  listTeams,
  createTeam,
  updateTeam,
  deleteTeam,
  inviteToTeam,
  deleteComment,
} from '../src/api/bff.ts';
import type { WorkspaceView } from '../src/api/bff.ts';

// ── Helpers ───────────────────────────────────────────────────────────────────

function mockResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function mockText(status: number, text: string): Response {
  return new Response(text, { status });
}

function mock204(): Response {
  return new Response(null, { status: 204 });
}

const EMPTY_VIEW: WorkspaceView = { isEmpty: true, locked: false, lock: null, state: null };
const STATE_VIEW: WorkspaceView = {
  isEmpty: false,
  locked: false,
  lock: null,
  state: {
    serial: 3,
    terraform_version: '1.9.0',
    lineage: 'abc-123',
    resource_count: 2,
    resource_types: [{ type: 'aws_instance', count: 2 }],
    outputs: null,
  },
};
const LOCKED_VIEW: WorkspaceView = {
  isEmpty: false,
  locked: true,
  lock: { id: 'lock-id', operation: 'plan', who: 'alice@host', info: null, version: '1.9.0', created: '2024-01-01T00:00:00Z', path: 'alice/prod' },
  state: null,
};

// ── All bff.ts client tests ───────────────────────────────────────────────────

describe('bff client', () => {
  const TOKEN = 'test-token-abc';
  let fetchStub: sinon.SinonStub;

  beforeEach(() => { fetchStub = sinon.stub(globalThis, 'fetch'); });
  afterEach(() => { fetchStub.restore(); });

  // ── signup ──────────────────────────────────────────────────────────────────

  describe('signup', () => {
    it('POSTs to /api/gatekeeper/signup with the payload', async () => {
      const payload = { email: 'a@b.com', username: 'alice', password: 'pass1234' };
      const result = { user_id: 'uid-1', email: 'a@b.com', username: 'alice' };
      fetchStub.resolves(mockResponse(201, result));

      const res = await signup(payload);

      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/signup');
      expect(opts.method).to.equal('POST');
      expect(JSON.parse(opts.body as string)).to.deep.equal(payload);
      expect(res).to.deep.equal(result);
    });

    it('throws with status 409 on conflict', async () => {
      fetchStub.resolves(mockText(409, 'conflict'));
      try {
        await signup({ email: 'x@x.com', username: 'x', password: 'pass1234' });
        expect.fail('should have thrown');
      } catch (err: unknown) {
        const e = err as { status: number; message: string };
        expect(e.status).to.equal(409);
        expect(e.message).to.equal('conflict');
      }
    });

    it('throws on 500 with status attached', async () => {
      fetchStub.resolves(mockText(500, ''));
      try {
        await signup({ email: 'x@x.com', username: 'x', password: 'pass1234' });
        expect.fail('should have thrown');
      } catch (err: unknown) {
        expect((err as { status: number }).status).to.equal(500);
      }
    });
  });

  // ── login ───────────────────────────────────────────────────────────────────

  describe('login', () => {
    it('POSTs to /api/gatekeeper/login with email and password', async () => {
      fetchStub.resolves(mockResponse(200, { token: 'jwt-abc' }));

      const res = await login('a@b.com', 'pass');

      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/login');
      expect(opts.method).to.equal('POST');
      expect(JSON.parse(opts.body as string)).to.deep.equal({ email: 'a@b.com', password: 'pass' });
      expect(res.token).to.equal('jwt-abc');
    });

    it('throws with status 401 on wrong credentials', async () => {
      fetchStub.resolves(mockText(401, 'unauthorized'));
      try {
        await login('a@b.com', 'wrong');
        expect.fail('should have thrown');
      } catch (err: unknown) {
        expect((err as { status: number }).status).to.equal(401);
      }
    });
  });

  // ── getSetupStatus ────────────────────────────────────────────────────────

  describe('getSetupStatus', () => {
    it('GETs /api/gatekeeper/setup/status without a token', async () => {
      fetchStub.resolves(mockResponse(200, { initialized: false }));

      const res = await getSetupStatus();

      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/setup/status');
      expect(opts.method).to.equal('GET');
      expect((opts.headers as Record<string, string>)['Authorization']).to.be.undefined;
      expect(res.initialized).to.be.false;
    });

    it('returns initialized=true once the instance has users', async () => {
      fetchStub.resolves(mockResponse(200, { initialized: true }));
      const res = await getSetupStatus();
      expect(res.initialized).to.be.true;
    });
  });

  // ── getUser ─────────────────────────────────────────────────────────────────

  describe('getUser', () => {
    const USER_ID = 'uid-123';

    it('GETs /api/gatekeeper/users/{id} with Authorization header', async () => {
      const user = { user_id: USER_ID, email: 'a@b.com', username: 'alice' };
      fetchStub.resolves(mockResponse(200, user));

      const res = await getUser(TOKEN, USER_ID);

      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal(`/api/gatekeeper/users/${USER_ID}`);
      expect(opts.method).to.equal('GET');
      expect((opts.headers as Record<string, string>)['Authorization']).to.equal(`Bearer ${TOKEN}`);
      expect(res.user_id).to.equal(USER_ID);
    });

    it('throws 404 for an unknown user id', async () => {
      fetchStub.resolves(mockText(404, 'not found'));
      try {
        await getUser(TOKEN, 'no-such-id');
        expect.fail('should have thrown');
      } catch (err: unknown) {
        expect((err as { status: number }).status).to.equal(404);
      }
    });
  });

  // ── updateUser ──────────────────────────────────────────────────────────────

  describe('updateUser', () => {
    it('PUTs to /api/gatekeeper/users/{id} with the update payload', async () => {
      const updated = { user_id: 'uid-1', email: 'new@b.com', username: 'alice' };
      fetchStub.resolves(mockResponse(200, updated));

      const payload = { email: 'new@b.com', username: 'alice' };
      const res = await updateUser(TOKEN, 'uid-1', payload);

      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/users/uid-1');
      expect(opts.method).to.equal('PUT');
      expect(JSON.parse(opts.body as string)).to.deep.equal(payload);
      expect(res.email).to.equal('new@b.com');
    });
  });

  // ── getWorkspaceState ────────────────────────────────────────────────────────

  describe('getWorkspaceState', () => {
    it('GETs /api/state/{path} with Authorization header', async () => {
      fetchStub.resolves(mockResponse(200, STATE_VIEW));

      await getWorkspaceState(TOKEN, 'alice/prod');

      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/state/alice/prod');
      expect((opts.headers as Record<string, string>)['Authorization']).to.equal(`Bearer ${TOKEN}`);
    });

    it('returns the WorkspaceView as-is for a healthy workspace', async () => {
      fetchStub.resolves(mockResponse(200, STATE_VIEW));
      const res = await getWorkspaceState(TOKEN, 'alice/prod');
      expect(res).to.deep.equal(STATE_VIEW);
    });

    it('returns isEmpty view for an empty workspace', async () => {
      fetchStub.resolves(mockResponse(200, EMPTY_VIEW));
      const res = await getWorkspaceState(TOKEN, 'alice/empty');
      expect(res.isEmpty).to.be.true;
      expect(res.state).to.be.null;
      expect(res.lock).to.be.null;
    });

    it('returns locked view for a locked workspace', async () => {
      fetchStub.resolves(mockResponse(200, LOCKED_VIEW));
      const res = await getWorkspaceState(TOKEN, 'alice/locked');
      expect(res.locked).to.be.true;
      expect(res.lock).to.not.be.null;
      expect(res.lock!.who).to.equal('alice@host');
      expect(res.state).to.be.null;
    });

    it('supports org-scoped paths (org/team/workspace)', async () => {
      fetchStub.resolves(mockResponse(200, STATE_VIEW));
      await getWorkspaceState(TOKEN, 'acme/platform/prod');
      const [url] = fetchStub.firstCall.args as [string];
      expect(url).to.equal('/api/state/acme/platform/prod');
    });

    it('throws on 403 forbidden', async () => {
      fetchStub.resolves(mockText(403, 'forbidden'));
      try {
        await getWorkspaceState(TOKEN, 'other/private');
        expect.fail('should have thrown');
      } catch (err: unknown) {
        expect((err as { status: number }).status).to.equal(403);
      }
    });

    it('throws on 500', async () => {
      fetchStub.resolves(mockText(500, 'internal error'));
      try {
        await getWorkspaceState(TOKEN, 'alice/prod');
        expect.fail('should have thrown');
      } catch (err: unknown) {
        expect((err as { status: number }).status).to.equal(500);
      }
    });
  });

  // ── deleteWorkspaceState ─────────────────────────────────────────────────────

  describe('deleteWorkspaceState', () => {
    it('DELETEs /api/state/{path}', async () => {
      fetchStub.resolves(mock204());
      await deleteWorkspaceState(TOKEN, 'alice/prod');
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/state/alice/prod');
      expect(opts.method).to.equal('DELETE');
    });

    it('throws on 423 (locked workspace cannot be deleted)', async () => {
      fetchStub.resolves(mockText(423, 'locked'));
      try {
        await deleteWorkspaceState(TOKEN, 'alice/locked');
        expect.fail('should have thrown');
      } catch (err: unknown) {
        expect((err as { status: number }).status).to.equal(423);
      }
    });

    it('throws on 404 (workspace not found)', async () => {
      fetchStub.resolves(mockText(404, 'not found'));
      try {
        await deleteWorkspaceState(TOKEN, 'alice/gone');
        expect.fail('should have thrown');
      } catch (err: unknown) {
        expect((err as { status: number }).status).to.equal(404);
      }
    });
  });

  // ── listAuditLogs ────────────────────────────────────────────────────────────

  describe('listAuditLogs', () => {
    it('GETs /api/gatekeeper/audit-logs without filters', async () => {
      fetchStub.resolves(mockResponse(200, []));
      await listAuditLogs(TOKEN);
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/audit-logs');
      expect(opts.method).to.equal('GET');
      expect((opts.headers as Record<string, string>)['Authorization']).to.equal(`Bearer ${TOKEN}`);
    });

    it('appends filter query params when provided', async () => {
      fetchStub.resolves(mockResponse(200, []));
      await listAuditLogs(TOKEN, { actor_id: 'u1', action: 'create', limit: 10, offset: 20 });
      const [url] = fetchStub.firstCall.args as [string];
      expect(url).to.include('actor_id=u1');
      expect(url).to.include('action=create');
      expect(url).to.include('limit=10');
      expect(url).to.include('offset=20');
    });

    it('throws on 403', async () => {
      fetchStub.resolves(mockText(403, 'forbidden'));
      try {
        await listAuditLogs(TOKEN);
        expect.fail('should have thrown');
      } catch (err: unknown) {
        expect((err as { status: number }).status).to.equal(403);
      }
    });
  });

  // ── CI Steps ─────────────────────────────────────────────────────────────────

  describe('listSteps', () => {
    it('GETs /api/workflows/steps', async () => {
      fetchStub.resolves(mockResponse(200, []));
      await listSteps(TOKEN);
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/workflows/steps');
      expect(opts.method).to.equal('GET');
      expect((opts.headers as Record<string, string>)['Authorization']).to.equal(`Bearer ${TOKEN}`);
    });
  });

  describe('getStep', () => {
    it('GETs /api/workflows/steps/:id', async () => {
      const step = { step_id: 's1', name: 'build', action: 'docker/build', active: true, created_by: 'u1', created_at: '', updated_at: '' };
      fetchStub.resolves(mockResponse(200, step));
      const res = await getStep(TOKEN, 's1');
      const [url] = fetchStub.firstCall.args as [string];
      expect(url).to.equal('/api/workflows/steps/s1');
      expect(res.step_id).to.equal('s1');
    });

    it('throws 404 for unknown step', async () => {
      fetchStub.resolves(mockText(404, 'not found'));
      try {
        await getStep(TOKEN, 'nope');
        expect.fail('should have thrown');
      } catch (err: unknown) {
        expect((err as { status: number }).status).to.equal(404);
      }
    });
  });

  describe('createStep', () => {
    it('POSTs to /api/workflows/steps with payload', async () => {
      const payload = { name: 'lint', action: 'shell/run', timeout: 30 };
      const created = { step_id: 's2', ...payload, active: true, created_by: 'u1', created_at: '', updated_at: '' };
      fetchStub.resolves(mockResponse(201, created));
      const res = await createStep(TOKEN, payload);
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/workflows/steps');
      expect(opts.method).to.equal('POST');
      expect(JSON.parse(opts.body as string)).to.deep.equal(payload);
      expect(res.step_id).to.equal('s2');
    });
  });

  describe('updateStep', () => {
    it('PUTs to /api/workflows/steps/:id with partial payload', async () => {
      const updated = { step_id: 's1', name: 'renamed', action: 'shell/run', active: true, created_by: 'u1', created_at: '', updated_at: '' };
      fetchStub.resolves(mockResponse(200, updated));
      const res = await updateStep(TOKEN, 's1', { name: 'renamed' });
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/workflows/steps/s1');
      expect(opts.method).to.equal('PUT');
      expect(JSON.parse(opts.body as string)).to.deep.equal({ name: 'renamed' });
      expect(res.name).to.equal('renamed');
    });
  });

  describe('deleteStep', () => {
    it('DELETEs /api/workflows/steps/:id', async () => {
      fetchStub.resolves(mock204());
      await deleteStep(TOKEN, 's1');
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/workflows/steps/s1');
      expect(opts.method).to.equal('DELETE');
    });

    it('throws 404 for unknown step', async () => {
      fetchStub.resolves(mockText(404, 'not found'));
      try {
        await deleteStep(TOKEN, 'nope');
        expect.fail('should have thrown');
      } catch (err: unknown) {
        expect((err as { status: number }).status).to.equal(404);
      }
    });
  });

  describe('listActions', () => {
    it('GETs /api/workflows/actions', async () => {
      fetchStub.resolves(mockResponse(200, [{ name: 'docker/build' }]));
      const res = await listActions(TOKEN);
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/workflows/actions');
      expect(opts.method).to.equal('GET');
      expect(res[0].name).to.equal('docker/build');
    });
  });

  // ── Workflow CRUD ─────────────────────────────────────────────────────────────

  describe('createWorkflow', () => {
    it('POSTs to /api/workflows/pipelines with name and steps', async () => {
      const payload = { name: 'ci', steps: [{ step_id: 's1' }] };
      const created = { workflow_id: 'w1', name: 'ci', steps: [], active: true, created_by: 'u1', org_id: null, created_at: '', updated_at: '' };
      fetchStub.resolves(mockResponse(201, created));
      const res = await createWorkflow(TOKEN, payload);
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/workflows/pipelines');
      expect(opts.method).to.equal('POST');
      expect(JSON.parse(opts.body as string)).to.deep.equal(payload);
      expect(res.workflow_id).to.equal('w1');
    });

    it('throws 400 on validation error', async () => {
      fetchStub.resolves(mockText(400, 'bad request'));
      try {
        await createWorkflow(TOKEN, { name: '', steps: [] });
        expect.fail('should have thrown');
      } catch (err: unknown) {
        expect((err as { status: number }).status).to.equal(400);
      }
    });
  });

  describe('updateWorkflow', () => {
    it('PUTs to /api/workflows/pipelines/:id', async () => {
      const updated = { workflow_id: 'w1', name: 'renamed', steps: [], active: true, created_by: 'u1', org_id: null, created_at: '', updated_at: '' };
      fetchStub.resolves(mockResponse(200, updated));
      await updateWorkflow(TOKEN, 'w1', { name: 'renamed' });
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/workflows/pipelines/w1');
      expect(opts.method).to.equal('PUT');
      expect(JSON.parse(opts.body as string)).to.deep.equal({ name: 'renamed' });
    });
  });

  // ── Forge ──────────────────────────────────────────────────────────────────

  describe('createExecution', () => {
    it('POSTs to /api/forge/executions', async () => {
      const payload = { image: 'alpine:3', command: ['echo', 'hi'] };
      const created = { execution_id: 'e1', status: 'pending', image: 'alpine:3', command: ['echo', 'hi'], created_at: '', updated_at: '' };
      fetchStub.resolves(mockResponse(201, created));
      const res = await createExecution(TOKEN, payload);
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/forge/executions');
      expect(opts.method).to.equal('POST');
      expect(JSON.parse(opts.body as string)).to.deep.equal(payload);
      expect(res.execution_id).to.equal('e1');
    });

    it('includes optional fields in body when provided', async () => {
      const payload = { image: 'alpine:3', command: ['sh'], env: { FOO: 'bar' }, timeout: 60, runner_class: 'standard' };
      fetchStub.resolves(mockResponse(201, { execution_id: 'e2', status: 'pending', image: 'alpine:3', command: ['sh'], created_at: '', updated_at: '' }));
      await createExecution(TOKEN, payload);
      const [, opts] = fetchStub.firstCall.args as [string, RequestInit];
      const body = JSON.parse(opts.body as string);
      expect(body.env).to.deep.equal({ FOO: 'bar' });
      expect(body.runner_class).to.equal('standard');
    });
  });

  describe('createRunnerClass', () => {
    it('POSTs to /api/forge/runner-classes', async () => {
      const payload = { name: 'standard', memory_mb: 512, cpu_millicores: 500, enabled: true };
      fetchStub.resolves(mockResponse(201, payload));
      await createRunnerClass(TOKEN, payload);
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/forge/runner-classes');
      expect(opts.method).to.equal('POST');
      expect(JSON.parse(opts.body as string)).to.deep.equal(payload);
    });
  });

  describe('getRunnerClass', () => {
    it('GETs /api/forge/runner-classes/:name', async () => {
      fetchStub.resolves(mockResponse(200, { name: 'standard', memory_mb: 512, cpu_millicores: 500, enabled: true }));
      const res = await getRunnerClass(TOKEN, 'standard');
      const [url] = fetchStub.firstCall.args as [string];
      expect(url).to.equal('/api/forge/runner-classes/standard');
      expect(res.name).to.equal('standard');
    });
  });

  describe('updateRunnerClass', () => {
    it('PUTs to /api/forge/runner-classes/:name', async () => {
      fetchStub.resolves(mockResponse(200, { name: 'standard', memory_mb: 512, cpu_millicores: 500, enabled: false }));
      await updateRunnerClass(TOKEN, 'standard', { enabled: false });
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/forge/runner-classes/standard');
      expect(opts.method).to.equal('PUT');
      expect(JSON.parse(opts.body as string)).to.deep.equal({ enabled: false });
    });
  });

  describe('deleteRunnerClass', () => {
    it('DELETEs /api/forge/runner-classes/:name', async () => {
      fetchStub.resolves(mock204());
      await deleteRunnerClass(TOKEN, 'standard');
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/forge/runner-classes/standard');
      expect(opts.method).to.equal('DELETE');
    });

    it('throws 404 for unknown runner class', async () => {
      fetchStub.resolves(mockText(404, 'not found'));
      try {
        await deleteRunnerClass(TOKEN, 'nope');
        expect.fail('should have thrown');
      } catch (err: unknown) {
        expect((err as { status: number }).status).to.equal(404);
      }
    });
  });

  // ── Containers ────────────────────────────────────────────────────────────────

  describe('listContainerRepos', () => {
    it('GETs /api/containers/repositories', async () => {
      fetchStub.resolves(mockResponse(200, []));
      await listContainerRepos(TOKEN);
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/containers/repositories');
      expect(opts.method).to.equal('GET');
      expect((opts.headers as Record<string, string>)['Authorization']).to.equal(`Bearer ${TOKEN}`);
    });
  });

  describe('listImageTags', () => {
    it('GETs /api/containers/repositories/:ns/:img/tags', async () => {
      fetchStub.resolves(mockResponse(200, [{ name: 'latest', digest: 'sha256:abc' }]));
      const res = await listImageTags(TOKEN, 'myorg', 'myapp');
      const [url] = fetchStub.firstCall.args as [string];
      expect(url).to.equal('/api/containers/repositories/myorg/myapp/tags');
      expect(res[0].name).to.equal('latest');
    });
  });

  describe('getManifest', () => {
    it('GETs /api/containers/repositories/:ns/:img/manifests/:ref', async () => {
      const manifest = { digest: 'sha256:abc', media_type: 'application/vnd.oci.image.manifest.v1+json' };
      fetchStub.resolves(mockResponse(200, manifest));
      const res = await getManifest(TOKEN, 'myorg', 'myapp', 'latest');
      const [url] = fetchStub.firstCall.args as [string];
      expect(url).to.equal('/api/containers/repositories/myorg/myapp/manifests/latest');
      expect(res.digest).to.equal('sha256:abc');
    });
  });

  describe('deleteManifest', () => {
    it('DELETEs /api/containers/repositories/:ns/:img/manifests/:digest', async () => {
      fetchStub.resolves(mock204());
      await deleteManifest(TOKEN, 'myorg', 'myapp', 'sha256:abc');
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/containers/repositories/myorg/myapp/manifests/sha256:abc');
      expect(opts.method).to.equal('DELETE');
    });
  });

  // ── Gitea integration ──────────────────────────────────────────────────────

  describe('getGiteaAccount', () => {
    it('GETs /api/gitea_integration/account', async () => {
      fetchStub.resolves(mockResponse(200, { username: 'gituser', url: 'http://git.local' }));
      const res = await getGiteaAccount(TOKEN);
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gitea_integration/account');
      expect(opts.method).to.equal('GET');
      expect(res.username).to.equal('gituser');
    });

    it('throws 404 when no account linked', async () => {
      fetchStub.resolves(mockText(404, 'not found'));
      try {
        await getGiteaAccount(TOKEN);
        expect.fail('should have thrown');
      } catch (err: unknown) {
        expect((err as { status: number }).status).to.equal(404);
      }
    });
  });

  describe('linkGiteaAccount', () => {
    it('PUTs to /api/gitea_integration/account with token in body', async () => {
      fetchStub.resolves(mockResponse(200, { username: 'gituser' }));
      await linkGiteaAccount(TOKEN, 'my-git-pat');
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gitea_integration/account');
      expect(opts.method).to.equal('PUT');
      expect(JSON.parse(opts.body as string)).to.deep.equal({ token: 'my-git-pat' });
    });
  });

  describe('unlinkGiteaAccount', () => {
    it('DELETEs /api/gitea_integration/account', async () => {
      fetchStub.resolves(mock204());
      await unlinkGiteaAccount(TOKEN);
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gitea_integration/account');
      expect(opts.method).to.equal('DELETE');
    });
  });

  describe('listGiteaRepos', () => {
    it('GETs /api/gitea_integration/repos', async () => {
      fetchStub.resolves(mockResponse(200, []));
      await listGiteaRepos(TOKEN);
      const [url] = fetchStub.firstCall.args as [string];
      expect(url).to.equal('/api/gitea_integration/repos');
    });
  });

  describe('createGiteaRepo', () => {
    it('POSTs to /api/gitea_integration/repos', async () => {
      const payload = { name: 'myrepo', private: true };
      const created = { owner: 'alice', name: 'myrepo', full_name: 'alice/myrepo', private: true };
      fetchStub.resolves(mockResponse(201, created));
      const res = await createGiteaRepo(TOKEN, payload);
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gitea_integration/repos');
      expect(opts.method).to.equal('POST');
      expect(JSON.parse(opts.body as string)).to.deep.equal(payload);
      expect(res.full_name).to.equal('alice/myrepo');
    });
  });

  describe('getGiteaRepo', () => {
    it('GETs /api/gitea_integration/repos/:owner/:name', async () => {
      fetchStub.resolves(mockResponse(200, { owner: 'alice', name: 'myrepo', full_name: 'alice/myrepo', private: false }));
      await getGiteaRepo(TOKEN, 'alice', 'myrepo');
      const [url] = fetchStub.firstCall.args as [string];
      expect(url).to.equal('/api/gitea_integration/repos/alice/myrepo');
    });
  });

  describe('deleteGiteaRepo', () => {
    it('DELETEs /api/gitea_integration/repos/:owner/:name', async () => {
      fetchStub.resolves(mock204());
      await deleteGiteaRepo(TOKEN, 'alice', 'myrepo');
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gitea_integration/repos/alice/myrepo');
      expect(opts.method).to.equal('DELETE');
    });
  });

  describe('listBranches', () => {
    it('GETs /api/gitea_integration/repos/:owner/:repo/branches', async () => {
      fetchStub.resolves(mockResponse(200, [{ name: 'main', commit_sha: 'abc123' }]));
      const res = await listBranches(TOKEN, 'alice', 'myrepo');
      const [url] = fetchStub.firstCall.args as [string];
      expect(url).to.equal('/api/gitea_integration/repos/alice/myrepo/branches');
      expect(res[0].name).to.equal('main');
    });
  });

  describe('listGitTags', () => {
    it('GETs /api/gitea_integration/repos/:owner/:repo/tags', async () => {
      fetchStub.resolves(mockResponse(200, [{ name: 'v1.0', commit_sha: 'def456' }]));
      await listGitTags(TOKEN, 'alice', 'myrepo');
      const [url] = fetchStub.firstCall.args as [string];
      expect(url).to.equal('/api/gitea_integration/repos/alice/myrepo/tags');
    });
  });

  describe('listCommits', () => {
    it('GETs /api/gitea_integration/repos/:owner/:repo/commits', async () => {
      fetchStub.resolves(mockResponse(200, [{ sha: 'abc', message: 'init', author: 'alice', date: '' }]));
      await listCommits(TOKEN, 'alice', 'myrepo');
      const [url] = fetchStub.firstCall.args as [string];
      expect(url).to.equal('/api/gitea_integration/repos/alice/myrepo/commits');
    });
  });

  describe('listPulls', () => {
    it('GETs /api/gitea_integration/repos/:owner/:repo/pulls', async () => {
      fetchStub.resolves(mockResponse(200, []));
      await listPulls(TOKEN, 'alice', 'myrepo');
      const [url] = fetchStub.firstCall.args as [string];
      expect(url).to.equal('/api/gitea_integration/repos/alice/myrepo/pulls');
    });
  });

  describe('createPull', () => {
    it('POSTs to /api/gitea_integration/repos/:owner/:repo/pulls', async () => {
      const payload = { title: 'fix: typo', head: 'fix/typo', base: 'main' };
      const created = { index: 1, title: 'fix: typo', head: 'fix/typo', base: 'main', status: 'open' };
      fetchStub.resolves(mockResponse(201, created));
      const res = await createPull(TOKEN, 'alice', 'myrepo', payload);
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gitea_integration/repos/alice/myrepo/pulls');
      expect(opts.method).to.equal('POST');
      expect(JSON.parse(opts.body as string)).to.deep.equal(payload);
      expect(res.index).to.equal(1);
    });
  });

  describe('getPull', () => {
    it('GETs /api/gitea_integration/repos/:owner/:repo/pulls/:index', async () => {
      fetchStub.resolves(mockResponse(200, { index: 3, title: 'feat', head: 'f', base: 'main', status: 'open' }));
      await getPull(TOKEN, 'alice', 'myrepo', 3);
      const [url] = fetchStub.firstCall.args as [string];
      expect(url).to.equal('/api/gitea_integration/repos/alice/myrepo/pulls/3');
    });
  });

  describe('mergePull', () => {
    it('POSTs to /api/gitea_integration/repos/:owner/:repo/pulls/:index/merge', async () => {
      fetchStub.resolves(mock204());
      await mergePull(TOKEN, 'alice', 'myrepo', 3);
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gitea_integration/repos/alice/myrepo/pulls/3/merge');
      expect(opts.method).to.equal('POST');
    });

    it('throws 409 when already merged', async () => {
      fetchStub.resolves(mockText(409, 'already merged'));
      try {
        await mergePull(TOKEN, 'alice', 'myrepo', 1);
        expect.fail('should have thrown');
      } catch (err: unknown) {
        expect((err as { status: number }).status).to.equal(409);
      }
    });
  });

  // ── Invites ───────────────────────────────────────────────────────────────────

  describe('listInvites', () => {
    it('GETs /api/gatekeeper/invites', async () => {
      fetchStub.resolves(mockResponse(200, []));
      await listInvites(TOKEN);
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/invites');
      expect(opts.method).to.equal('GET');
    });
  });

  describe('getInvite', () => {
    it('GETs /api/gatekeeper/invites/:id', async () => {
      const inv = { invite_id: 'i1', email: 'x@x.com', invited_by: 'u1', status: 'pending', created_at: '', updated_at: '' };
      fetchStub.resolves(mockResponse(200, inv));
      const res = await getInvite(TOKEN, 'i1');
      const [url] = fetchStub.firstCall.args as [string];
      expect(url).to.equal('/api/gatekeeper/invites/i1');
      expect(res.invite_id).to.equal('i1');
    });
  });

  describe('acceptInvite', () => {
    it('POSTs to /api/gatekeeper/invites/:id/accept', async () => {
      fetchStub.resolves(mock204());
      await acceptInvite(TOKEN, 'i1');
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/invites/i1/accept');
      expect(opts.method).to.equal('POST');
    });
  });

  describe('declineInvite', () => {
    it('POSTs to /api/gatekeeper/invites/:id/decline', async () => {
      fetchStub.resolves(mock204());
      await declineInvite(TOKEN, 'i1');
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/invites/i1/decline');
      expect(opts.method).to.equal('POST');
    });
  });

  describe('deleteInvite', () => {
    it('DELETEs /api/gatekeeper/invites/:id', async () => {
      fetchStub.resolves(mock204());
      await deleteInvite(TOKEN, 'i1');
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/invites/i1');
      expect(opts.method).to.equal('DELETE');
    });

    it('throws 404 for unknown invite', async () => {
      fetchStub.resolves(mockText(404, 'not found'));
      try {
        await deleteInvite(TOKEN, 'nope');
        expect.fail('should have thrown');
      } catch (err: unknown) {
        expect((err as { status: number }).status).to.equal(404);
      }
    });
  });

  // ── Orgs extended ──────────────────────────────────────────────────────────

  describe('listOrgs', () => {
    it('GETs /api/gatekeeper/orgs', async () => {
      fetchStub.resolves(mockResponse(200, []));
      await listOrgs(TOKEN);
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/orgs');
      expect(opts.method).to.equal('GET');
    });
  });

  describe('updateOrg', () => {
    it('PUTs to /api/gatekeeper/orgs/:id with org_name', async () => {
      fetchStub.resolves(mockResponse(200, { org_id: 'o1', org_name: 'new-name' }));
      await updateOrg(TOKEN, 'o1', 'new-name');
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/orgs/o1');
      expect(opts.method).to.equal('PUT');
      expect(JSON.parse(opts.body as string)).to.deep.equal({ org_name: 'new-name' });
    });
  });

  describe('deleteOrg', () => {
    it('DELETEs /api/gatekeeper/orgs/:id', async () => {
      fetchStub.resolves(mock204());
      await deleteOrg(TOKEN, 'o1');
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/orgs/o1');
      expect(opts.method).to.equal('DELETE');
    });
  });

  describe('inviteToOrg', () => {
    it('POSTs to /api/gatekeeper/orgs/:id/invites with email', async () => {
      fetchStub.resolves(mock204());
      await inviteToOrg(TOKEN, 'o1', 'bob@example.com');
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/orgs/o1/invites');
      expect(opts.method).to.equal('POST');
      expect(JSON.parse(opts.body as string)).to.deep.equal({ email: 'bob@example.com' });
    });
  });

  // ── Permissions ────────────────────────────────────────────────────────────

  describe('createPermission', () => {
    it('POSTs to /api/gatekeeper/permissions', async () => {
      const payload = { name: 'read-forge', service: 'forge', actions: ['read'], resources: ['*'] };
      const created = { permission_id: 'p1', ...payload };
      fetchStub.resolves(mockResponse(201, created));
      const res = await createPermission(TOKEN, payload);
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/permissions');
      expect(opts.method).to.equal('POST');
      expect(JSON.parse(opts.body as string)).to.deep.equal(payload);
      expect(res.permission_id).to.equal('p1');
    });
  });

  describe('updatePermission', () => {
    it('PUTs to /api/gatekeeper/permissions/:id', async () => {
      fetchStub.resolves(mockResponse(200, { permission_id: 'p1', name: 'write-forge', service: 'forge', actions: ['write'], resources: ['*'] }));
      await updatePermission(TOKEN, 'p1', { actions: ['write'] });
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/permissions/p1');
      expect(opts.method).to.equal('PUT');
      expect(JSON.parse(opts.body as string)).to.deep.equal({ actions: ['write'] });
    });
  });

  describe('deletePermission', () => {
    it('DELETEs /api/gatekeeper/permissions/:id', async () => {
      fetchStub.resolves(mock204());
      await deletePermission(TOKEN, 'p1');
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/permissions/p1');
      expect(opts.method).to.equal('DELETE');
    });
  });

  // ── Roles ──────────────────────────────────────────────────────────────────

  describe('createRole', () => {
    it('POSTs to /api/gatekeeper/roles with permissions_ids', async () => {
      const payload = { permissions_ids: ['p1', 'p2'] };
      const created = { role_id: 'r1', permissions_ids: ['p1', 'p2'] };
      fetchStub.resolves(mockResponse(201, created));
      const res = await createRole(TOKEN, payload);
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/roles');
      expect(opts.method).to.equal('POST');
      expect(JSON.parse(opts.body as string)).to.deep.equal(payload);
      expect(res.role_id).to.equal('r1');
    });
  });

  describe('updateRole', () => {
    it('PUTs to /api/gatekeeper/roles/:id', async () => {
      fetchStub.resolves(mockResponse(200, { role_id: 'r1', permissions_ids: ['p3'] }));
      await updateRole(TOKEN, 'r1', { permissions_ids: ['p3'] });
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/roles/r1');
      expect(opts.method).to.equal('PUT');
      expect(JSON.parse(opts.body as string)).to.deep.equal({ permissions_ids: ['p3'] });
    });
  });

  describe('deleteRole', () => {
    it('DELETEs /api/gatekeeper/roles/:id', async () => {
      fetchStub.resolves(mock204());
      await deleteRole(TOKEN, 'r1');
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/roles/r1');
      expect(opts.method).to.equal('DELETE');
    });
  });

  // ── Service permission requests ────────────────────────────────────────────

  describe('listServiceRequests', () => {
    it('GETs /api/gatekeeper/service-permission-requests without filters', async () => {
      fetchStub.resolves(mockResponse(200, []));
      await listServiceRequests(TOKEN);
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/service-permission-requests');
      expect(opts.method).to.equal('GET');
    });

    it('appends filter query params when provided', async () => {
      fetchStub.resolves(mockResponse(200, []));
      await listServiceRequests(TOKEN, { service_name: 'forge', status: 'pending' });
      const [url] = fetchStub.firstCall.args as [string];
      expect(url).to.include('service_name=forge');
      expect(url).to.include('status=pending');
    });
  });

  describe('getServiceRequest', () => {
    it('GETs /api/gatekeeper/service-permission-requests/:id', async () => {
      const sr = { request_id: 'sr1', service_name: 'forge', requested_by: 'u1', status: 'pending', created_at: '', updated_at: '' };
      fetchStub.resolves(mockResponse(200, sr));
      const res = await getServiceRequest(TOKEN, 'sr1');
      const [url] = fetchStub.firstCall.args as [string];
      expect(url).to.equal('/api/gatekeeper/service-permission-requests/sr1');
      expect(res.request_id).to.equal('sr1');
    });
  });

  describe('approveServiceRequest', () => {
    it('POSTs to /api/gatekeeper/service-permission-requests/:id/approve', async () => {
      fetchStub.resolves(mock204());
      await approveServiceRequest(TOKEN, 'sr1');
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/service-permission-requests/sr1/approve');
      expect(opts.method).to.equal('POST');
    });
  });

  describe('declineServiceRequest', () => {
    it('POSTs to /api/gatekeeper/service-permission-requests/:id/decline', async () => {
      fetchStub.resolves(mock204());
      await declineServiceRequest(TOKEN, 'sr1');
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/service-permission-requests/sr1/decline');
      expect(opts.method).to.equal('POST');
    });
  });

  // ── Sessions ───────────────────────────────────────────────────────────────

  describe('getSession', () => {
    it('GETs /api/gatekeeper/sessions/:id', async () => {
      const session = { session_id: 'sess1', user_id: 'u1', created_at: '' };
      fetchStub.resolves(mockResponse(200, session));
      const res = await getSession(TOKEN, 'sess1');
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/sessions/sess1');
      expect(opts.method).to.equal('GET');
      expect(res.session_id).to.equal('sess1');
    });

    it('throws 404 for unknown session', async () => {
      fetchStub.resolves(mockText(404, 'not found'));
      try {
        await getSession(TOKEN, 'nope');
        expect.fail('should have thrown');
      } catch (err: unknown) {
        expect((err as { status: number }).status).to.equal(404);
      }
    });
  });

  describe('deleteSession', () => {
    it('DELETEs /api/gatekeeper/sessions/:id', async () => {
      fetchStub.resolves(mock204());
      await deleteSession(TOKEN, 'sess1');
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/sessions/sess1');
      expect(opts.method).to.equal('DELETE');
    });
  });

  // ── Teams ──────────────────────────────────────────────────────────────────

  describe('listTeams', () => {
    it('GETs /api/gatekeeper/teams', async () => {
      fetchStub.resolves(mockResponse(200, []));
      await listTeams(TOKEN);
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/teams');
      expect(opts.method).to.equal('GET');
    });
  });

  describe('createTeam', () => {
    it('POSTs to /api/gatekeeper/teams with team_name', async () => {
      const payload = { team_name: 'backend' };
      const created = { team_id: 't1', team_name: 'backend' };
      fetchStub.resolves(mockResponse(201, created));
      const res = await createTeam(TOKEN, payload);
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/teams');
      expect(opts.method).to.equal('POST');
      expect(JSON.parse(opts.body as string)).to.deep.equal(payload);
      expect(res.team_id).to.equal('t1');
    });

    it('includes optional role_id when provided', async () => {
      fetchStub.resolves(mockResponse(201, { team_id: 't2', team_name: 'ops' }));
      await createTeam(TOKEN, { team_name: 'ops', role_id: 'r1' });
      const [, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(JSON.parse(opts.body as string).role_id).to.equal('r1');
    });
  });

  describe('updateTeam', () => {
    it('PUTs to /api/gatekeeper/teams/:id with team_name', async () => {
      fetchStub.resolves(mockResponse(200, { team_id: 't1', team_name: 'frontend' }));
      await updateTeam(TOKEN, 't1', { team_name: 'frontend' });
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/teams/t1');
      expect(opts.method).to.equal('PUT');
      expect(JSON.parse(opts.body as string)).to.deep.equal({ team_name: 'frontend' });
    });
  });

  describe('deleteTeam', () => {
    it('DELETEs /api/gatekeeper/teams/:id', async () => {
      fetchStub.resolves(mock204());
      await deleteTeam(TOKEN, 't1');
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/teams/t1');
      expect(opts.method).to.equal('DELETE');
    });
  });

  describe('inviteToTeam', () => {
    it('POSTs to /api/gatekeeper/teams/:id/invites with email', async () => {
      fetchStub.resolves(mock204());
      await inviteToTeam(TOKEN, 't1', 'carol@example.com');
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/gatekeeper/teams/t1/invites');
      expect(opts.method).to.equal('POST');
      expect(JSON.parse(opts.body as string)).to.deep.equal({ email: 'carol@example.com' });
    });
  });

  // ── Ticket comment delete ──────────────────────────────────────────────────

  describe('deleteComment', () => {
    it('DELETEs /api/tickets/tickets/:ticketId/comments/:commentId', async () => {
      fetchStub.resolves(mock204());
      await deleteComment(TOKEN, 'tkt1', 'cmt2');
      const [url, opts] = fetchStub.firstCall.args as [string, RequestInit];
      expect(url).to.equal('/api/tickets/tickets/tkt1/comments/cmt2');
      expect(opts.method).to.equal('DELETE');
      expect((opts.headers as Record<string, string>)['Authorization']).to.equal(`Bearer ${TOKEN}`);
    });

    it('throws 404 for unknown comment', async () => {
      fetchStub.resolves(mockText(404, 'not found'));
      try {
        await deleteComment(TOKEN, 'tkt1', 'nope');
        expect.fail('should have thrown');
      } catch (err: unknown) {
        expect((err as { status: number }).status).to.equal(404);
      }
    });
  });
});
