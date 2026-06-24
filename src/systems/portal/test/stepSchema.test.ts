import { expect } from 'chai';
import {
  schemaKey, schemaForAction, buildStepWith, WITH_KEY_PREFIX, RAW_WITH_KEY,
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
    expect(fields.map(f => f.key)).to.deep.equal(['image', 'run', 'env', 'runner_class', RAW_WITH_KEY]);
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
});
