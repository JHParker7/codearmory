import { expect } from 'chai';
import {
  schemaKey, schemaForAction, buildStepWith, formValsFromWith, WITH_KEY_PREFIX, RAW_WITH_KEY, GIT_CLONE_ENV,
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
    expect(fields.map(f => f.key)).to.deep.equal(['image', 'run', 'runner_class', 'secret_refs', 'env', 'output_env', RAW_WITH_KEY]);
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

  it('wraps a selected repo into a forge git: secret_ref', () => {
    const out = buildStepWith('forge/run', reader({
      [p('image')]: 'alpine',
      [p('run')]: 'git clone "$GIT_CLONE_URL"',
      [p('secret_refs')]: 'https://github.com/acme/widgets.git',
    }));
    expect(out).to.deep.equal({
      image: 'alpine',
      run: 'git clone "$GIT_CLONE_URL"',
      secret_refs: { [GIT_CLONE_ENV]: 'git:https://github.com/acme/widgets.git' },
    });
  });

  it('omits secret_refs when no repo is selected, leaving an advanced one intact', () => {
    const out = buildStepWith('forge/run', reader({
      [p('image')]: 'alpine',
      [p('run')]: 'true',
      [p(RAW_WITH_KEY)]: '{"secret_refs":{"DEPLOY_KEY":"secret:deploy"}}',
    }));
    expect(out).to.deep.equal({ image: 'alpine', run: 'true', secret_refs: { DEPLOY_KEY: 'secret:deploy' } });
  });
});

describe('formValsFromWith (repo round-trip)', () => {
  it('recovers the bare clone URL from a git: secret_ref', () => {
    const withMap = {
      image: 'alpine',
      run: 'true',
      secret_refs: { [GIT_CLONE_ENV]: 'git:https://github.com/acme/widgets.git' },
    };
    const vals = formValsFromWith('forge/run', withMap);
    expect(vals[p('secret_refs')]).to.equal('https://github.com/acme/widgets.git');
    // The repo field claims secret_refs, so it is not dumped into the advanced field.
    expect(vals[p(RAW_WITH_KEY)]).to.equal(undefined);
    // And it round-trips back to the same with map.
    const back = buildStepWith('forge/run', (k) => vals[k] ?? '');
    expect(back).to.deep.equal(withMap);
  });

  it('leaves secret_refs to the advanced With JSON when it carries non-repo refs', () => {
    const withMap = {
      image: 'alpine',
      run: 'true',
      secret_refs: { [GIT_CLONE_ENV]: 'git:https://github.com/acme/widgets.git', DEPLOY_KEY: 'secret:deploy' },
    };
    const vals = formValsFromWith('forge/run', withMap);
    // The repo field stays empty; the whole secret_refs map goes to advanced JSON…
    expect(vals[p('secret_refs')]).to.equal(undefined);
    expect(JSON.parse(vals[p(RAW_WITH_KEY)])).to.deep.equal({ secret_refs: withMap.secret_refs });
    // …and round-trips intact (no dropped DEPLOY_KEY).
    const back = buildStepWith('forge/run', (k) => vals[k] ?? '');
    expect(back).to.deep.equal(withMap);
  });
});
