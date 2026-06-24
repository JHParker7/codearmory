import { expect } from 'chai';
import {
  stagesFromSteps, graphFromSteps, stepsFromGraph, StepRef, GraphNode, GraphEdge,
} from '../src/pages/app/pipelineGraph.ts';

describe('stagesFromSteps', () => {
  it('collapses consecutive same-group steps into one stage', () => {
    const steps: StepRef[] = [
      { step_id: 'build' },
      { step_id: 'lint', parallel_group: 0 },
      { step_id: 'test', parallel_group: 0 },
      { step_id: 'deploy' },
    ];
    expect(stagesFromSteps(steps)).to.deep.equal([['build'], ['lint', 'test'], ['deploy']]);
  });

  it('keeps same group value as separate stages when not consecutive', () => {
    const steps: StepRef[] = [
      { step_id: 'a', parallel_group: 0 },
      { step_id: 'b' },
      { step_id: 'c', parallel_group: 0 },
    ];
    expect(stagesFromSteps(steps)).to.deep.equal([['a'], ['b'], ['c']]);
  });

  it('treats null/undefined group as a solo stage', () => {
    const steps: StepRef[] = [{ step_id: 'a', parallel_group: null }, { step_id: 'b' }];
    expect(stagesFromSteps(steps)).to.deep.equal([['a'], ['b']]);
  });

  it('returns no stages for an empty pipeline', () => {
    expect(stagesFromSteps([])).to.deep.equal([]);
  });
});

describe('graphFromSteps', () => {
  it('makes one node per step and fans consecutive stages out/in', () => {
    const steps: StepRef[] = [
      { step_id: 'build' },
      { step_id: 'lint', parallel_group: 0 },
      { step_id: 'test', parallel_group: 0 },
      { step_id: 'deploy' },
    ];
    const { nodes, edges } = graphFromSteps(steps);
    // node ids are per-occurrence instances; the step is in node.data.stepId.
    expect(nodes.map((n) => n.data.stepId)).to.have.members(['build', 'lint', 'test', 'deploy']);
    expect(new Set(nodes.map((n) => n.id)).size).to.equal(4); // distinct instance ids
    const stepOf = new Map(nodes.map((n) => [n.id, n.data.stepId]));
    const wired = edges.map((e) => [stepOf.get(e.source), stepOf.get(e.target)]);
    // build -> lint, build -> test, lint -> deploy, test -> deploy
    expect(wired).to.deep.have.members([
      ['build', 'lint'], ['build', 'test'], ['lint', 'deploy'], ['test', 'deploy'],
    ]);
  });

  it('stacks stages on increasing y and spreads parallel siblings on x', () => {
    const { nodes } = graphFromSteps([
      { step_id: 'build' },
      { step_id: 'lint', parallel_group: 0 },
      { step_id: 'test', parallel_group: 0 },
    ]);
    const byStep = (sid: string) => nodes.find((n) => n.data.stepId === sid)!;
    expect(byStep('lint').position.y).to.be.greaterThan(byStep('build').position.y);
    expect(byStep('test').position.x).to.be.greaterThan(byStep('lint').position.x);
    expect(byStep('lint').position.y).to.equal(byStep('test').position.y); // same stage row
  });

  it('gives a repeated step distinct nodes that both round-trip', () => {
    const steps: StepRef[] = [
      { step_id: 'build', parallel_group: null },
      { step_id: 'notify', parallel_group: null },
      { step_id: 'build', parallel_group: null }, // build again, later
    ];
    const { nodes, edges } = graphFromSteps(steps);
    expect(nodes).to.have.length(3);
    expect(nodes.filter((n) => n.data.stepId === 'build')).to.have.length(2);
    expect(new Set(nodes.map((n) => n.id)).size).to.equal(3);
    expect(stepsFromGraph(nodes, edges)).to.deep.equal(steps);
  });
});

describe('stepsFromGraph', () => {
  const node = (id: string, x = 0, y = 0): GraphNode => ({ id, position: { x, y }, data: { stepId: id } });
  const edge = (s: string, t: string): GraphEdge => ({ id: `${s}->${t}`, source: s, target: t });

  it('layers a linear chain into solo sequential steps', () => {
    const out = stepsFromGraph([node('a'), node('b'), node('c')], [edge('a', 'b'), edge('b', 'c')]);
    expect(out).to.deep.equal([
      { step_id: 'a', parallel_group: null },
      { step_id: 'b', parallel_group: null },
      { step_id: 'c', parallel_group: null },
    ]);
  });

  it('puts same-layer nodes in one parallel group, ordered by x', () => {
    const nodes = [node('build', 0, 0), node('test', 250, 110), node('lint', 0, 110), node('deploy', 0, 220)];
    const edges = [edge('build', 'lint'), edge('build', 'test'), edge('lint', 'deploy'), edge('test', 'deploy')];
    const out = stepsFromGraph(nodes, edges);
    expect(out).to.deep.equal([
      { step_id: 'build', parallel_group: null },
      { step_id: 'lint', parallel_group: 0 }, // x=0  < test x=250
      { step_id: 'test', parallel_group: 0 },
      { step_id: 'deploy', parallel_group: null },
    ]);
  });

  it('round-trips steps -> graph -> steps for a fan-out pipeline', () => {
    const steps: StepRef[] = [
      { step_id: 'build', parallel_group: null },
      { step_id: 'lint', parallel_group: 0 },
      { step_id: 'test', parallel_group: 0 },
      { step_id: 'deploy', parallel_group: null },
    ];
    const { nodes, edges } = graphFromSteps(steps);
    expect(stepsFromGraph(nodes, edges)).to.deep.equal(steps);
  });

  it('throws on a cycle', () => {
    expect(() => stepsFromGraph([node('a'), node('b')], [edge('a', 'b'), edge('b', 'a')]))
      .to.throw(/cycle/);
  });

  it('handles an isolated node as a solo first-layer step', () => {
    const out = stepsFromGraph([node('only')], []);
    expect(out).to.deep.equal([{ step_id: 'only', parallel_group: null }]);
  });

  it('emits step_id from node.data.stepId, not the node id (repeated steps)', () => {
    const nodes: GraphNode[] = [
      { id: 'n0', position: { x: 0, y: 0 }, data: { stepId: 'build' } },
      { id: 'n1', position: { x: 0, y: 160 }, data: { stepId: 'build' } }, // same step, second time
    ];
    const edges: GraphEdge[] = [{ id: 'n0->n1', source: 'n0', target: 'n1' }];
    expect(stepsFromGraph(nodes, edges)).to.deep.equal([
      { step_id: 'build', parallel_group: null },
      { step_id: 'build', parallel_group: null },
    ]);
  });
});
