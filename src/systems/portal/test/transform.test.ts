import { expect } from 'chai';
import { fromEmpty, fromLocked, fromState } from '../bff/src/transform/workspace.ts';

// ── fromEmpty ─────────────────────────────────────────────────────────────────

describe('fromEmpty', () => {
  it('sets isEmpty=true', () => {
    expect(fromEmpty().isEmpty).to.be.true;
  });

  it('sets locked=false', () => {
    expect(fromEmpty().locked).to.be.false;
  });

  it('sets lock and state to null', () => {
    const v = fromEmpty();
    expect(v.lock).to.be.null;
    expect(v.state).to.be.null;
  });
});

// ── fromLocked ────────────────────────────────────────────────────────────────

describe('fromLocked', () => {
  const full = {
    ID: 'lock-id-123',
    Operation: 'plan',
    Who: 'alice@myhost',
    Info: 'running terraform plan',
    Version: '1.9.0',
    Created: '2024-06-01T12:00:00Z',
    Path: 'alice/prod',
  };

  it('sets locked=true and isEmpty=false', () => {
    const v = fromLocked(full);
    expect(v.locked).to.be.true;
    expect(v.isEmpty).to.be.false;
  });

  it('sets state to null', () => {
    expect(fromLocked(full).state).to.be.null;
  });

  it('normalizes PascalCase fields to camelCase', () => {
    const lock = fromLocked(full).lock!;
    expect(lock.id).to.equal('lock-id-123');
    expect(lock.operation).to.equal('plan');
    expect(lock.who).to.equal('alice@myhost');
    expect(lock.version).to.equal('1.9.0');
    expect(lock.created).to.equal('2024-06-01T12:00:00Z');
    expect(lock.path).to.equal('alice/prod');
    expect(lock.info).to.equal('running terraform plan');
  });

  it('sets info to null when absent', () => {
    const { Info: _info, ...withoutInfo } = full;
    expect(fromLocked(withoutInfo).lock!.info).to.be.null;
  });

  it('sets path to null when absent', () => {
    const { Path: _path, ...withoutPath } = full;
    expect(fromLocked(withoutPath).lock!.path).to.be.null;
  });

  it('preserves who including the hostname portion', () => {
    const v = fromLocked({ ...full, Who: 'bob@buildhost.internal' });
    expect(v.lock!.who).to.equal('bob@buildhost.internal');
  });
});

// ── fromState ─────────────────────────────────────────────────────────────────

describe('fromState', () => {
  const base = {
    terraform_version: '1.9.0',
    serial: 7,
    lineage: 'abc-def-123',
    resources: [
      { type: 'aws_instance', name: 'web' },
      { type: 'aws_instance', name: 'worker' },
      { type: 'aws_s3_bucket', name: 'assets' },
      { type: 'aws_iam_role', name: 'exec' },
    ],
  };

  it('sets isEmpty=false and locked=false', () => {
    const v = fromState(base);
    expect(v.isEmpty).to.be.false;
    expect(v.locked).to.be.false;
  });

  it('sets lock to null', () => {
    expect(fromState(base).lock).to.be.null;
  });

  it('passes through serial, terraform_version, and lineage', () => {
    const { state } = fromState(base);
    expect(state!.serial).to.equal(7);
    expect(state!.terraform_version).to.equal('1.9.0');
    expect(state!.lineage).to.equal('abc-def-123');
  });

  it('computes resource_count as total resource count', () => {
    expect(fromState(base).state!.resource_count).to.equal(4);
  });

  it('aggregates resource_types by type name', () => {
    const types = fromState(base).state!.resource_types;
    const awsInstance = types.find(t => t.type === 'aws_instance');
    const s3 = types.find(t => t.type === 'aws_s3_bucket');
    const iam = types.find(t => t.type === 'aws_iam_role');
    expect(awsInstance?.count).to.equal(2);
    expect(s3?.count).to.equal(1);
    expect(iam?.count).to.equal(1);
  });

  it('sorts resource_types by count descending', () => {
    const types = fromState(base).state!.resource_types;
    expect(types[0].type).to.equal('aws_instance');
    expect(types[0].count).to.equal(2);
    for (let i = 1; i < types.length; i++) {
      expect(types[i].count).to.be.lte(types[i - 1].count);
    }
  });

  it('returns empty resource_types for empty resources array', () => {
    const v = fromState({ ...base, resources: [] });
    expect(v.state!.resource_count).to.equal(0);
    expect(v.state!.resource_types).to.deep.equal([]);
  });

  it('handles missing resources key as empty', () => {
    const { resources: _r, ...withoutResources } = base;
    const v = fromState(withoutResources);
    expect(v.state!.resource_count).to.equal(0);
    expect(v.state!.resource_types).to.deep.equal([]);
  });

  it('passes through outputs when present', () => {
    const outputs = { vpc_id: { value: 'vpc-123', type: 'string' } };
    const v = fromState({ ...base, outputs });
    expect(v.state!.outputs).to.deep.equal(outputs);
  });

  it('sets outputs to null when absent', () => {
    expect(fromState(base).state!.outputs).to.be.null;
  });

  it('handles a single resource type correctly', () => {
    const v = fromState({ ...base, resources: [{ type: 'null_resource', name: 'test' }] });
    expect(v.state!.resource_count).to.equal(1);
    expect(v.state!.resource_types).to.deep.equal([{ type: 'null_resource', count: 1 }]);
  });

  it('handles many instances of one type', () => {
    const resources = Array.from({ length: 10 }, (_, i) => ({ type: 'aws_instance', name: `web-${i}` }));
    const v = fromState({ ...base, resources });
    expect(v.state!.resource_count).to.equal(10);
    expect(v.state!.resource_types).to.deep.equal([{ type: 'aws_instance', count: 10 }]);
  });
});
