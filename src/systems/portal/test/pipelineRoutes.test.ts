import { expect } from 'chai';
import {
  routesFromBlocks, layoutGraph, findCycle, nodeName, pruneRoutes, stepsFromNodes,
} from '../src/pages/app/pipelineGraph';
import type { Block, Route } from '../src/pages/app/pipelineGraph';

/** Inline blocks, so the node name is the block's own name and no catalog is needed. */
const mk = (name: string, parallelWithPrev = false): Block => ({
  uid: `u-${name}`, stepId: '', parallelWithPrev, name, inline: { action: 'forge/run' },
});
const noCatalog = () => undefined;
const key = (rs: Route[]) => rs.map((r) => `${r.from}->${r.to}`).sort();

describe('routesFromBlocks (legacy ordered pipeline -> graph)', () => {
  it('turns a linear stack into a chain', () => {
    const rs = routesFromBlocks([mk('a'), mk('b'), mk('c')], noCatalog);
    expect(key(rs)).to.deep.equal(['a->b', 'b->c']);
  });

  it('fans out and joins a parallel band', () => {
    // build | lint+test | deploy — the cross product between adjacent stages IS
    // the barrier, mirroring the backend's derivation.
    const rs = routesFromBlocks([mk('build'), mk('lint'), mk('test', true), mk('deploy')], noCatalog);
    expect(key(rs)).to.deep.equal(['build->lint', 'build->test', 'lint->deploy', 'test->deploy']);
  });

  it('gives a single step no routes', () => {
    expect(routesFromBlocks([mk('only')], noCatalog)).to.deep.equal([]);
  });

  it('treats a leading parallel band as multiple entry steps', () => {
    const rs = routesFromBlocks([mk('a'), mk('b', true), mk('c')], noCatalog);
    expect(key(rs)).to.deep.equal(['a->c', 'b->c']);
    // Neither a nor b has an inbound route, so both start the run.
    expect(rs.some((r) => r.to === 'a' || r.to === 'b')).to.equal(false);
  });
});

describe('layoutGraph', () => {
  it('places a chain in successive columns', () => {
    const blocks = [mk('a'), mk('b'), mk('c')];
    const placed = layoutGraph(blocks, routesFromBlocks(blocks, noCatalog), noCatalog);
    expect(placed.map((p) => p.layer)).to.deep.equal([0, 1, 2]);
  });

  it('puts a parallel band in one column and the join past it', () => {
    const blocks = [mk('build'), mk('lint'), mk('test', true), mk('deploy')];
    const placed = layoutGraph(blocks, routesFromBlocks(blocks, noCatalog), noCatalog);
    const layer = Object.fromEntries(placed.map((p) => [p.name, p.layer]));
    expect(layer.build).to.equal(0);
    expect(layer.lint).to.equal(1);
    expect(layer.test).to.equal(1);
    expect(layer.deploy).to.equal(2);
    // Siblings share a column, so they must not share a row.
    const band = placed.filter((p) => p.layer === 1).map((p) => p.row);
    expect(new Set(band).size).to.equal(band.length);
  });

  it('places a join past the LONGEST path to it, so edges only point rightward', () => {
    // a->b->c->d and a->d: d must sit past c, not next to b.
    const blocks = [mk('a'), mk('b'), mk('c'), mk('d')];
    const routes: Route[] = [
      { from: 'a', to: 'b' }, { from: 'b', to: 'c' }, { from: 'c', to: 'd' }, { from: 'a', to: 'd' },
    ];
    const placed = layoutGraph(blocks, routes, noCatalog);
    const layer = Object.fromEntries(placed.map((p) => [p.name, p.layer]));
    expect(layer.d).to.equal(3);
  });

  it('terminates on a cycle rather than looping forever', () => {
    const blocks = [mk('a'), mk('b')];
    const placed = layoutGraph(blocks, [{ from: 'a', to: 'b' }, { from: 'b', to: 'a' }], noCatalog);
    expect(placed).to.have.length(2);
  });
});

describe('findCycle', () => {
  it('accepts an acyclic graph', () => {
    const blocks = [mk('a'), mk('b'), mk('c')];
    expect(findCycle(blocks, routesFromBlocks(blocks, noCatalog), noCatalog)).to.deep.equal([]);
  });

  it('accepts a diamond', () => {
    const blocks = [mk('a'), mk('b'), mk('c'), mk('d')];
    const routes: Route[] = [
      { from: 'a', to: 'b' }, { from: 'a', to: 'c' }, { from: 'b', to: 'd' }, { from: 'c', to: 'd' },
    ];
    expect(findCycle(blocks, routes, noCatalog)).to.deep.equal([]);
  });

  it('names the steps caught in a cycle', () => {
    const blocks = [mk('a'), mk('b'), mk('c')];
    const routes: Route[] = [{ from: 'a', to: 'b' }, { from: 'b', to: 'c' }, { from: 'c', to: 'a' }];
    expect(findCycle(blocks, routes, noCatalog).sort()).to.deep.equal(['a', 'b', 'c']);
  });
});

describe('pruneRoutes', () => {
  it('drops edges whose endpoints are gone', () => {
    const routes: Route[] = [{ from: 'a', to: 'b' }, { from: 'b', to: 'ghost' }];
    expect(key(pruneRoutes(routes, ['a', 'b']))).to.deep.equal(['a->b']);
  });
});

describe('stepsFromNodes', () => {
  it('never emits parallel_group — routes replace it, and the backend rejects both', () => {
    // Even from blocks carrying the legacy flag, the graph editor writes routes only.
    const refs = stepsFromNodes([mk('a'), mk('b', true), mk('c', true)]);
    expect(refs).to.have.length(3);
    refs.forEach((r) => expect(r).to.not.have.property('parallel_group'));
  });

  it('keeps step order, since it is still the run records\' step index', () => {
    expect(stepsFromNodes([mk('a'), mk('b'), mk('c')]).map((r) => r.name)).to.deep.equal(['a', 'b', 'c']);
  });
});

describe('stepsFromNodes: fan-out and gates survive the graph editor', () => {
  it('emits a gate as an approval ref, never as an inline action', () => {
    // The backend rejects an inline step whose action is "approval" ("use a gate"),
    // so building one that way would 400 on save.
    const gate: Block = { uid: 'g', stepId: '', parallelWithPrev: false, name: 'approve', approval: { message: 'ship it?' } };
    const refs = stepsFromNodes([gate]);
    expect(refs[0]).to.have.property('approval');
    expect(refs[0].approval).to.deep.equal({ message: 'ship it?' });
    expect(refs[0]).to.not.have.property('action');
  });

  it('keeps a matrix on a node — and, unlike the old model, its siblings can be parallel', () => {
    const b: Block = { ...mk('build'), matrix: { var: 'os', values: ['linux', 'mac'] } };
    const refs = stepsFromNodes([b]);
    expect(refs[0].matrix).to.deep.equal({ var: 'os', values: ['linux', 'mac'] });
    // parallel_group was mutually exclusive with matrix; routes are not, so the
    // node keeps its fan-out with no group to conflict with.
    expect(refs[0]).to.not.have.property('parallel_group');
  });

  it('keeps a scatter on an inline node', () => {
    const b: Block = { ...mk('build'), scatter: { regex: '^services/[^/]+$', mode: 'dir' } };
    expect(stepsFromNodes([b])[0].scatter).to.deep.equal({ regex: '^services/[^/]+$', mode: 'dir' });
  });

  it('drops an incomplete matrix rather than sending a var-less fan-out', () => {
    const b: Block = { ...mk('build'), matrix: { var: '  ', values: [] } };
    expect(stepsFromNodes([b])[0]).to.not.have.property('matrix');
  });
});

describe('nodeName', () => {
  it('prefers the block name, falling back to the referenced step definition', () => {
    expect(nodeName(mk('build'), noCatalog)).to.equal('build');
    const ref: Block = { uid: 'u', stepId: 's1', parallelWithPrev: false };
    expect(nodeName(ref, (id) => (id === 's1' ? 'shared-step' : undefined))).to.equal('shared-step');
  });
});
