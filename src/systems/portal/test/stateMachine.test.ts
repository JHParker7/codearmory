import { expect } from 'chai';
import { docToModel, modelToDoc, serializeDoc, parseDoc } from '../src/pages/app/stateMachine';
import type { SMDoc } from '../src/pages/app/stateMachine';
import type { Route } from '../src/pages/app/pipelineGraph';

const noDef = (_id: string) => undefined;

// Routes as a comparable, order-independent set of "from->to@when".
function routeSet(routes: Route[]): string[] {
  return routes.map(r => `${r.from}->${r.to}@${r.when ?? ''}`).sort();
}

function doc(o: object): SMDoc { return o as SMDoc; }

describe('stateMachine docToModel', () => {
  it('expands a sequence', () => {
    const m = docToModel(doc({ name: 'seq', states: {
      a: { run: 'one', next: 'b' },
      b: { run: 'two', next: 'c' },
      c: { run: 'three', end: true },
    } }));
    expect(m.steps.map(s => s.name).sort()).to.deep.equal(['a', 'b', 'c']);
    expect(routeSet(m.routes)).to.deep.equal(['a->b@', 'b->c@']);
    expect(m.steps[0].action).to.equal('one');
    expect(m.steps[0].step_id).to.equal(undefined);
  });

  it('lowers a separate choice state to conditional routes', () => {
    const m = docToModel(doc({ states: {
      tests: { run: 'test', next: 'decide' },
      decide: { choice: [
        { when: 'steps.tests.status == "failed"', next: 'mark' },
        { default: 'done' },
      ] },
      mark: { run: 'm', end: true },
      done: { run: 'n', end: true },
    } }));
    // "decide" is a choice, not a step.
    expect(m.steps.map(s => s.name)).to.not.include('decide');
    expect(routeSet(m.routes)).to.deep.equal([
      'tests->done@',
      'tests->mark@steps.tests.status == "failed"',
    ]);
  });

  it('rejects a choice on a runner step', () => {
    expect(() => docToModel(doc({ states: {
      tests: { run: 'test', choice: [{ default: 'done' }] },
      done: { run: 'n', end: true },
    } }))).to.throw(/only routes/);
  });

  it('expands a parallel fork and join', () => {
    const m = docToModel(doc({ states: {
      build: { run: 'b', next: ['lint', 'test'] },
      lint: { run: 'l', next: 'gate' },
      test: { run: 't', next: 'gate' },
      gate: { run: 'g', end: true },
    } }));
    expect(routeSet(m.routes)).to.deep.equal(['build->lint@', 'build->test@', 'lint->gate@', 'test->gate@']);
  });

  it('rewires a map region boundary', () => {
    const m = docToModel(doc({ states: {
      seed: { run: 'seed', next: 'per_dir' },
      per_dir: { map: { var: 'dir', over: '${inputs.dirs}', volume: 'workspace', states: {
        unit: { run: 'test', next: 'pkg' },
        pkg: { run: 'build', end: true },
      } }, next: 'report' },
      report: { run: 'report', end: true },
    } }));
    expect(m.maps).to.have.length(1);
    expect(m.maps[0]).to.include({ id: 'per_dir', var: 'dir', values_from: '${inputs.dirs}' });
    const byName = Object.fromEntries(m.steps.map(s => [s.name, s]));
    expect(byName.per_dir).to.equal(undefined); // container is not a node
    expect(byName.unit.map_id).to.equal('per_dir');
    expect(routeSet(m.routes)).to.deep.equal(['pkg->report@', 'seed->unit@', 'unit->pkg@']);
  });

  it('rejects malformed documents', () => {
    expect(() => docToModel(doc({ states: {} }))).to.throw();
    expect(() => docToModel(doc({ states: { a: { next: 'b' }, b: { end: true } } }))).to.throw(/run, use, approval, map or choice/);
    expect(() => docToModel(doc({ states: { a: { run: 'r', next: 'ghost' } } }))).to.throw(/unknown state/);
  });
});

describe('stateMachine round-trip', () => {
  it('model -> doc -> model preserves routes and steps', () => {
    const src = docToModel(doc({ name: 'rt', states: {
      build: { run: 'b', next: ['lint', 'test'] },
      lint: { run: 'l', next: 'check' },
      test: { run: 't', next: 'check' },
      check: { run: 'c', next: 'decide' },
      decide: { choice: [{ when: 'x == 1', next: 'ship' }, { default: 'skip' }] },
      ship: { run: 's', end: true },
      skip: { run: 'k', end: true },
    } }));
    const back = modelToDoc('rt', '', src.steps, src.routes, src.maps, [], [], noDef);
    const again = docToModel(back);
    expect(routeSet(again.routes)).to.deep.equal(routeSet(src.routes));
    expect(again.steps.map(s => s.name).sort()).to.deep.equal(src.steps.map(s => s.name).sort());
  });

  it('serializes to YAML and parses back', () => {
    const d = modelToDoc('p', '', [{ action: 'go build', name: 'build' }], [], [], [], [], noDef);
    const yaml = serializeDoc(d, 'yaml');
    expect(yaml).to.match(/states:/);
    expect(yaml).to.match(/run: go build/);
    const round = parseDoc(yaml);
    expect(round.states.build.run).to.equal('go build');
  });
});
