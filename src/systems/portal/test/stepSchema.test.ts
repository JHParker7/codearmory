import { expect } from 'chai';
import {
  schemaKey, schemaForAction, buildStepWith, formValsFromWith, WITH_KEY_PREFIX, RAW_WITH_KEY, GIT_CLONE_ENV,
  actionSupportsGitRepo, gitRepoFromWith, withGitRepo,
} from '../src/pages/app/stepSchema.ts';

// valueOf factory: reads prefixed keys ("with.<key>") from a plain map.
const reader = (vals: Record<string, string>) => (key: string) => vals[key] ?? '';
const p = (key: string) => WITH_KEY_PREFIX + key;

describe('schemaKey', () => {
  it('buckets a known action to itself', () => {
    expect(schemaKey('forge/run')).to.equal('forge/run');
  });
  it('buckets unknown/custom actions to the empty generic bucket', () => {
    expect(schemaKey('http')).to.equal('');
    expect(schemaKey('totally/custom')).to.equal('');
    expect(schemaKey('')).to.equal('');
  });
});

describe('schemaForAction', () => {
  it('returns the tailored fields plus a trailing advanced With for a known action', () => {
    const fields = schemaForAction('forge/run');
    expect(fields.map(f => f.key)).to.deep.equal(['image', 'run', 'runner_class', 'volumes', 'env', 'output_env', RAW_WITH_KEY]);
    expect(fields[fields.length - 1].kind).to.equal('json');
    expect(fields[fields.length - 1].required).to.not.equal(true);
  });
  it('falls back to a single required With JSON field for an unknown action', () => {
    const fields = schemaForAction('http');
    expect(fields).to.have.length(1);
    expect(fields[0].key).to.equal(RAW_WITH_KEY);
    expect(fields[0].required).to.equal(true);
  });
});

describe('buildStepWith', () => {
  it('assembles required + optional typed fields for forge/run', () => {
    const out = buildStepWith('forge/run', reader({
      [p('image')]: 'ubuntu:22.04',
      [p('run')]: 'go test ./...',
      [p('env')]: 'CI=true TOKEN=abc',
    }));
    expect(out).to.deep.equal({
      image: 'ubuntu:22.04',
      run: 'go test ./...',
      env: { CI: 'true', TOKEN: 'abc' },
    });
  });

  it('throws when a required field is missing', () => {
    expect(() => buildStepWith('forge/run', reader({ [p('run')]: 'echo hi' }))).to.throw(/Image is required/);
    expect(() => buildStepWith('forge/run', reader({ [p('image')]: 'alpine' }))).to.throw(/Run is required/);
  });

  it('omits blank optional fields', () => {
    const out = buildStepWith('forge/run', reader({ [p('image')]: 'alpine', [p('run')]: 'true' }));
    expect(out).to.deep.equal({ image: 'alpine', run: 'true' });
  });

  it('parses a non-negative int field and rejects bad ones', () => {
    expect(buildStepWith('blueprints/backend', reader({ [p('ttl_secs')]: '14400' }))).to.deep.equal({ ttl_secs: 14400 });
    expect(() => buildStepWith('blueprints/backend', reader({ [p('ttl_secs')]: '-5' }))).to.throw(/non-negative integer/);
    expect(() => buildStepWith('blueprints/backend', reader({ [p('ttl_secs')]: 'abc' }))).to.throw(/non-negative integer/);
  });

  it('rejects malformed env tokens', () => {
    expect(() => buildStepWith('forge/run', reader({
      [p('image')]: 'alpine', [p('run')]: 'true', [p('env')]: 'NOTANENV',
    }))).to.throw(/expected KEY=VALUE/);
  });

  it('requires a With JSON object for unknown actions', () => {
    expect(() => buildStepWith('http', reader({}))).to.throw(/requires a With JSON object/);
    expect(() => buildStepWith('http', reader({ [p(RAW_WITH_KEY)]: 'not json' }))).to.throw(/not valid JSON/);
    expect(() => buildStepWith('http', reader({ [p(RAW_WITH_KEY)]: '[1,2]' }))).to.throw(/JSON object/);
  });

  it('parses a valid With JSON object for unknown actions', () => {
    const out = buildStepWith('http', reader({ [p(RAW_WITH_KEY)]: '{"service":"forge","path":"/x"}' }));
    expect(out).to.deep.equal({ service: 'forge', path: '/x' });
  });

  it('overlays advanced With JSON without clobbering typed fields', () => {
    const out = buildStepWith('forge/run', reader({
      [p('image')]: 'alpine',
      [p('run')]: 'true',
      [p(RAW_WITH_KEY)]: '{"image":"IGNORED","extra":"kept"}',
    }));
    expect(out).to.deep.equal({ image: 'alpine', run: 'true', extra: 'kept' });
  });

  // The shared step definition is repo-agnostic (the repo is chosen per step in the
  // pipeline builder), so a secret_refs supplied via the advanced With JSON still
  // flows through unchanged.
  it('passes an advanced-With secret_refs through untouched', () => {
    const out = buildStepWith('forge/run', reader({
      [p('image')]: 'alpine',
      [p('run')]: 'true',
      [p(RAW_WITH_KEY)]: '{"secret_refs":{"DEPLOY_KEY":"secret:deploy"}}',
    }));
    expect(out).to.deep.equal({ image: 'alpine', run: 'true', secret_refs: { DEPLOY_KEY: 'secret:deploy' } });
  });
});

// The repo is no longer a shared-step field — it is a per-occurrence override set in
// the pipeline builder. formValsFromWith therefore leaves secret_refs to the advanced
// With JSON field (it has no dedicated repo field to claim it).
describe('formValsFromWith (secret_refs → advanced With)', () => {
  it('routes a git: secret_ref into the advanced With JSON and round-trips', () => {
    const withMap = {
      image: 'alpine',
      run: 'true',
      secret_refs: { [GIT_CLONE_ENV]: 'git:https://github.com/acme/widgets.git' },
    };
    const vals = formValsFromWith('forge/run', withMap);
    expect(vals[p('secret_refs')]).to.equal(undefined); // no dedicated repo field
    expect(JSON.parse(vals[p(RAW_WITH_KEY)])).to.deep.equal({ secret_refs: withMap.secret_refs });
    const back = buildStepWith('forge/run', (k) => vals[k] ?? '');
    expect(back).to.deep.equal(withMap);
  });
});

// Per-occurrence git repo helpers used by the pipeline builder's StepInputsEditor.
describe('per-step git repo helpers', () => {
  it('actionSupportsGitRepo is true only for forge steps', () => {
    expect(actionSupportsGitRepo('forge/run')).to.equal(true);
    expect(actionSupportsGitRepo('tickets/create')).to.equal(false);
    expect(actionSupportsGitRepo('http')).to.equal(false);
  });

  it('gitRepoFromWith recovers the bare ref and ignores non-git refs', () => {
    expect(gitRepoFromWith({ secret_refs: { [GIT_CLONE_ENV]: 'git:https://github.com/acme/widgets.git' } }))
      .to.equal('https://github.com/acme/widgets.git');
    // A run-input template round-trips too (the "based on an env var" case).
    expect(gitRepoFromWith({ secret_refs: { [GIT_CLONE_ENV]: 'git:${inputs.REPO}' } })).to.equal('${inputs.REPO}');
    expect(gitRepoFromWith({})).to.equal('');
    expect(gitRepoFromWith({ secret_refs: { OTHER: 'secret:x' } })).to.equal('');
  });

  it('withGitRepo sets/clears GIT_CLONE_URL while preserving sibling refs', () => {
    expect(withGitRepo(undefined, 'https://github.com/acme/widgets.git'))
      .to.deep.equal({ [GIT_CLONE_ENV]: 'git:https://github.com/acme/widgets.git' });
    // Preserves a sibling secret: ref.
    expect(withGitRepo({ DEPLOY_KEY: 'secret:deploy' }, '${inputs.REPO}'))
      .to.deep.equal({ DEPLOY_KEY: 'secret:deploy', [GIT_CLONE_ENV]: 'git:${inputs.REPO}' });
    // Clearing drops GIT_CLONE_URL but keeps siblings.
    expect(withGitRepo({ [GIT_CLONE_ENV]: 'git:x', DEPLOY_KEY: 'secret:deploy' }, ''))
      .to.deep.equal({ DEPLOY_KEY: 'secret:deploy' });
    // Clearing the sole ref returns undefined so the override can be dropped entirely.
    expect(withGitRepo({ [GIT_CLONE_ENV]: 'git:x' }, '')).to.equal(undefined);
  });
});

describe('shared workspace volumes', () => {
  it('create-volume defaults workflow_id to ${run_id}', () => {
    const out = buildStepWith('forge/create-volume', reader({
      [p('name')]: 'workspace',
      [p('size_mb')]: '1024',
      [p('medium')]: 'memory',
      [p('mount_path')]: '/workspace',
    }));
    expect(out).to.deep.equal({
      workflow_id: '${run_id}', name: 'workspace', size_mb: 1024, medium: 'memory', mount_path: '/workspace',
    });
  });

  it('create-volume lets an explicit workflow_id (advanced With) win', () => {
    const out = buildStepWith('forge/create-volume', reader({
      [p(RAW_WITH_KEY)]: '{"workflow_id":"custom-scope"}',
    }));
    expect(out.workflow_id).to.equal('custom-scope');
  });

  it('forge/run volume attach becomes the run-scoped volumes array', () => {
    const out = buildStepWith('forge/run', reader({
      [p('image')]: 'alpine:3.19',
      [p('run')]: 'make build',
      [p('volumes')]: 'workspace:/src',
    }));
    expect(out.volumes).to.deep.equal([
      { workflow_id: '${run_id}', name: 'workspace', mount_path: '/src', workdir: true },
    ]);
  });

  it('forge/run omits volumes when the attach field is blank', () => {
    const out = buildStepWith('forge/run', reader({
      [p('image')]: 'alpine:3.19',
      [p('run')]: 'make build',
    }));
    expect(out).to.not.have.property('volumes');
  });

  it('round-trips a forge/run volume attach through formValsFromWith', () => {
    const withMap = buildStepWith('forge/run', reader({
      [p('image')]: 'alpine:3.19',
      [p('run')]: 'make build',
      [p('volumes')]: 'cache:/data',
    }));
    const vals = formValsFromWith('forge/run', withMap);
    expect(vals[p('volumes')]).to.equal('cache:/data');
    // The default-mount case renders as the bare name.
    const bare = formValsFromWith('forge/run', { image: 'x', run: 'y', volumes: [{ workflow_id: '${run_id}', name: 'workspace', mount_path: '/workspace', workdir: true }] });
    expect(bare[p('volumes')]).to.equal('workspace');
  });

  it('does not spill the default workflow_id into advanced With on edit', () => {
    const withMap = buildStepWith('forge/create-volume', reader({ [p('name')]: 'workspace' }));
    const vals = formValsFromWith('forge/create-volume', withMap);
    expect(vals[p(RAW_WITH_KEY)]).to.equal(undefined);
  });
});

describe('forge/build-image', () => {
  it('nests flat fields into a build object + registry secret_ref', () => {
    const out = buildStepWith('forge/build-image', reader({
      [p('destinations')]: 'reg.io/acme/app:1.0, reg.io/acme/app:latest',
      [p('dockerfile')]: 'docker/Dockerfile',
      [p('build_args')]: 'VERSION=1.0 COMMIT=abc',
      [p('target')]: 'prod',
      [p('registry_secret')]: 'my-registry',
      [p('volumes')]: 'workspace:/src',
      [p('runner_class')]: 'build-kata',
    }));
    expect(out.build).to.deep.equal({
      destinations: ['reg.io/acme/app:1.0', 'reg.io/acme/app:latest'],
      dockerfile: 'docker/Dockerfile',
      build_args: { VERSION: '1.0', COMMIT: 'abc' },
      target: 'prod',
    });
    expect(out.secret_refs).to.deep.equal({ REGISTRY_AUTH: 'secret:my-registry' });
    expect(out.runner_class).to.equal('build-kata');
    expect(out.volumes).to.be.an('array');
    expect(out).to.not.have.property('destinations'); // moved under build
  });

  it('round-trips through formValsFromWith', () => {
    const withMap = buildStepWith('forge/build-image', reader({
      [p('destinations')]: 'reg.io/acme/app:1.0',
      [p('dockerfile')]: 'Dockerfile',
      [p('registry_secret')]: 'my-registry',
      [p('runner_class')]: 'build-kata',
    }));
    const vals = formValsFromWith('forge/build-image', withMap);
    expect(vals[p('destinations')]).to.equal('reg.io/acme/app:1.0');
    expect(vals[p('dockerfile')]).to.equal('Dockerfile');
    expect(vals[p('registry_secret')]).to.equal('my-registry');
    expect(vals[p('runner_class')]).to.equal('build-kata');
    // The nested build object and secret_refs must not leak into the advanced With.
    expect(vals[p(RAW_WITH_KEY)]).to.equal(undefined);
    // And it re-nests identically.
    expect(buildStepWith('forge/build-image', (k) => vals[k] ?? '')).to.deep.equal(withMap);
  });
});

describe('forge/git-clone', () => {
  it('nests path/ref/depth under a checkout object with a no-op run default', () => {
    const out = buildStepWith('forge/git-clone', reader({
      [p('volumes')]: 'workspace:/src',
      [p('path')]: 'app',
      [p('ref')]: 'main',
      [p('depth')]: '1',
    }));
    expect(out.checkout).to.deep.equal({ path: 'app', ref: 'main', depth: 1 });
    // image is forge-controlled; the step must not send one.
    expect(out).to.not.have.property('image');
    expect(out.run).to.equal('true'); // no-op; the checkout prologue is woven in
    expect(out.volumes).to.deep.equal([
      { workflow_id: '${run_id}', name: 'workspace', mount_path: '/src', workdir: true },
    ]);
    // path/ref/depth moved under checkout, not left at the top level.
    expect(out).to.not.have.property('path');
  });

  it('always sets checkout (empty) and lets a post-clone command override run', () => {
    const out = buildStepWith('forge/git-clone', reader({
      [p('volumes')]: 'workspace',
      [p('run')]: 'git submodule update --init',
    }));
    expect(out.checkout).to.deep.equal({});
    expect(out.run).to.equal('git submodule update --init');
  });

  it('requires a volume (image is forge-controlled, not required)', () => {
    expect(() => buildStepWith('forge/git-clone', reader({ [p('path')]: 'app' }))).to.throw(/Clone into volume is required/);
    expect(() => buildStepWith('forge/git-clone', reader({ [p('volumes')]: 'workspace' }))).to.not.throw();
  });

  it('round-trips through formValsFromWith without leaking checkout into advanced With', () => {
    const withMap = buildStepWith('forge/git-clone', reader({
      [p('volumes')]: 'workspace:/src',
      [p('path')]: 'app',
      [p('ref')]: 'release',
      [p('depth')]: '0',
    }));
    const vals = formValsFromWith('forge/git-clone', withMap);
    expect(vals[p('path')]).to.equal('app');
    expect(vals[p('ref')]).to.equal('release');
    expect(vals[p('depth')]).to.equal('0');
    expect(vals[p('volumes')]).to.equal('workspace:/src');
    expect(vals[p(RAW_WITH_KEY)]).to.equal(undefined);
    // And it re-nests identically.
    expect(buildStepWith('forge/git-clone', (k) => vals[k] ?? '')).to.deep.equal(withMap);
  });
});
