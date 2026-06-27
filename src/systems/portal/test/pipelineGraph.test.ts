import { expect } from 'chai';
import {
  stagesFromSteps, blocksFromSteps, stepsFromBlocks, stagesOf, StepRef, Block,
  stepsToPayload, configToJson, parseConfig, collectRefs,
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

  it('returns no stages for an empty pipeline', () => {
    expect(stagesFromSteps([])).to.deep.equal([]);
  });
});

describe('blocksFromSteps', () => {
  it('marks the first block of each stage as a start and the rest as parallel', () => {
    const blocks = blocksFromSteps([
      { step_id: 'build' },
      { step_id: 'lint', parallel_group: 0 },
      { step_id: 'test', parallel_group: 0 },
      { step_id: 'deploy' },
    ]);
    expect(blocks.map((b) => b.stepId)).to.deep.equal(['build', 'lint', 'test', 'deploy']);
    expect(blocks.map((b) => b.parallelWithPrev)).to.deep.equal([false, false, true, false]);
    expect(new Set(blocks.map((b) => b.uid)).size).to.equal(4); // unique instance ids
  });

  it('gives a repeated step distinct blocks', () => {
    const blocks = blocksFromSteps([{ step_id: 'build' }, { step_id: 'notify' }, { step_id: 'build' }]);
    expect(blocks.filter((b) => b.stepId === 'build')).to.have.length(2);
    expect(new Set(blocks.map((b) => b.uid)).size).to.equal(3);
  });
});

describe('stagesOf', () => {
  const blk = (uid: string, stepId: string, parallelWithPrev: boolean): Block => ({ uid, stepId, parallelWithPrev });
  it('bands consecutive linked blocks; first block always starts a stage', () => {
    const stages = stagesOf([
      blk('b0', 'build', true), // first block: flag ignored, starts a stage
      blk('b1', 'lint', false),
      blk('b2', 'test', true),
    ]);
    expect(stages.map((s) => s.map((b) => b.stepId))).to.deep.equal([['build'], ['lint', 'test']]);
  });
});

describe('stepsFromBlocks', () => {
  const blk = (uid: string, stepId: string, parallelWithPrev: boolean): Block => ({ uid, stepId, parallelWithPrev });

  it('solo blocks become sequential steps with null group', () => {
    const out = stepsFromBlocks([blk('b0', 'a', false), blk('b1', 'b', false)]);
    expect(out).to.deep.equal([
      { step_id: 'a', parallel_group: null },
      { step_id: 'b', parallel_group: null },
    ]);
  });

  it('a linked run shares one parallel group; a later start is its own stage', () => {
    // build starts a stage; lint+test link into it (all three run in parallel);
    // deploy starts a fresh sequential stage.
    const out = stepsFromBlocks([
      blk('b0', 'build', false),
      blk('b1', 'lint', true),
      blk('b2', 'test', true),
      blk('b3', 'deploy', false),
    ]);
    expect(out).to.deep.equal([
      { step_id: 'build', parallel_group: 0 },
      { step_id: 'lint', parallel_group: 0 },
      { step_id: 'test', parallel_group: 0 },
      { step_id: 'deploy', parallel_group: null },
    ]);
  });

  it('round-trips steps -> blocks -> steps (with a repeated step)', () => {
    const steps: StepRef[] = [
      { step_id: 'build', parallel_group: null },
      { step_id: 'lint', parallel_group: 0 },
      { step_id: 'test', parallel_group: 0 },
      { step_id: 'build', parallel_group: null }, // build again, later
    ];
    expect(stepsFromBlocks(blocksFromSteps(steps))).to.deep.equal(steps);
  });

  it('emits step_id from the block, and ignores a leading parallel flag', () => {
    const out = stepsFromBlocks([blk('b0', 'build', true), blk('b1', 'build', false)]);
    expect(out).to.deep.equal([
      { step_id: 'build', parallel_group: null },
      { step_id: 'build', parallel_group: null },
    ]);
  });
});

describe('matrix round-trip', () => {
  it('carries a solo step matrix through blocks and back', () => {
    const steps: StepRef[] = [
      { step_id: 'build', parallel_group: null },
      { step_id: 'deploy', parallel_group: null, matrix: { var: 'region', values: ['us', 'eu'] } },
    ];
    const blocks = blocksFromSteps(steps);
    expect(blocks[1].matrix).to.deep.equal({ var: 'region', values: ['us', 'eu'] });
    expect(stepsFromBlocks(blocks)).to.deep.equal(steps);
  });

  it('drops an incomplete matrix (no var) on serialize', () => {
    const out = stepsFromBlocks([
      { uid: 'b0', stepId: 'a', parallelWithPrev: false, matrix: { var: '  ', values: ['x'] } },
    ]);
    expect(out).to.deep.equal([{ step_id: 'a', parallel_group: null }]);
  });

  it('carries a values_from matrix', () => {
    const steps: StepRef[] = [
      { step_id: 'fan', parallel_group: null, matrix: { var: 'r', values_from: '${inputs.regions}' } },
    ];
    expect(stepsFromBlocks(blocksFromSteps(steps))).to.deep.equal(steps);
  });
});

describe('config ⇄ JSON (live editable panel)', () => {
  it('stepsToPayload emits group / matrix / bare shapes and drops an empty-var matrix', () => {
    const steps: StepRef[] = [
      { step_id: 'a', parallel_group: 0 },
      { step_id: 'b', parallel_group: null, matrix: { var: 'region', values: ['us'] } },
      { step_id: 'c', parallel_group: null, matrix: { var: '  ', values: ['x'] } },
      { step_id: 'd', parallel_group: null },
    ];
    expect(stepsToPayload(steps)).to.deep.equal([
      { step_id: 'a', parallel_group: 0 },
      { step_id: 'b', matrix: { var: 'region', values: ['us'] } },
      { step_id: 'c' },
      { step_id: 'd' },
    ]);
  });

  it('configToJson omits an empty description and pretty-prints the payload', () => {
    const json = configToJson('deploy', '', [{ step_id: 'a', parallel_group: null }]);
    expect(JSON.parse(json)).to.deep.equal({ name: 'deploy', steps: [{ step_id: 'a' }] });
    expect(json).to.contain('\n'); // pretty-printed
    const withDesc = JSON.parse(configToJson('deploy', 'ship it', []));
    expect(withDesc).to.deep.equal({ name: 'deploy', description: 'ship it', steps: [] });
  });

  it('parseConfig round-trips configToJson (name, description, parallel + matrix steps)', () => {
    const steps: StepRef[] = [
      { step_id: 'build', parallel_group: null },
      { step_id: 'lint', parallel_group: 0 },
      { step_id: 'deploy', parallel_group: null, matrix: { var: 'r', values: ['us', 'eu'] } },
    ];
    const parsed = parseConfig(configToJson('pipe', 'desc', steps));
    expect(parsed.name).to.equal('pipe');
    expect(parsed.description).to.equal('desc');
    expect(parsed.steps).to.deep.equal([
      { step_id: 'build', parallel_group: null },
      { step_id: 'lint', parallel_group: 0 },
      { step_id: 'deploy', parallel_group: null, matrix: { var: 'r', values: ['us', 'eu'] } },
    ]);
  });

  it('parseConfig defaults missing name/description and a missing steps array', () => {
    const parsed = parseConfig('{}');
    expect(parsed).to.deep.equal({ name: '', description: '', steps: [] });
  });

  it('parseConfig rejects malformed input with a user-facing message', () => {
    expect(() => parseConfig('{ not json')).to.throw();
    expect(() => parseConfig('[]')).to.throw('JSON object');
    expect(() => parseConfig('{"steps": "nope"}')).to.throw('array');
    expect(() => parseConfig('{"steps": [{"parallel_group": 0}]}')).to.throw('step_id');
  });
});

describe('inline approval gates', () => {
  it('round-trips a gate through blocks (no step_id) and back', () => {
    const steps: StepRef[] = [
      { step_id: 'build', parallel_group: null },
      { parallel_group: null, approval: { message: 'deploy?', approvers: ['alice'] } },
    ];
    const blocks = blocksFromSteps(steps);
    expect(blocks[1].approval).to.deep.equal({ message: 'deploy?', approvers: ['alice'] });
    expect(stepsFromBlocks(blocks)).to.deep.equal([
      { step_id: 'build', parallel_group: null },
      { approval: { message: 'deploy?', approvers: ['alice'] } },
    ]);
  });

  it('stepsToPayload emits a gate as just {approval}', () => {
    expect(stepsToPayload([{ parallel_group: null, approval: { message: 'ok?' } }]))
      .to.deep.equal([{ approval: { message: 'ok?' } }]);
  });

  it('configToJson/parseConfig round-trip a gate', () => {
    const steps: StepRef[] = [{ parallel_group: null, approval: { message: 'go?', approvers: ['a', 'b'] } }];
    const parsed = parseConfig(configToJson('p', '', steps));
    expect(parsed.steps).to.deep.equal([{ parallel_group: null, approval: { message: 'go?', approvers: ['a', 'b'] } }]);
  });

  it('parseConfig accepts a gate with no step_id', () => {
    const parsed = parseConfig('{"name":"p","steps":[{"approval":{"message":"hold"}}]}');
    expect(parsed.steps).to.deep.equal([{ parallel_group: null, approval: { message: 'hold' } }]);
  });
});

describe('collectRefs (step inspector)', () => {
  it('extracts run inputs (named + bare) and upstream step outputs, ignoring matrix', () => {
    const refs = collectRefs({
      image: 'ubuntu',
      run: 'deploy ${inputs.env} to ${matrix.region}',
      url: '${steps.build.output.url}',
      bare: '${TOKEN}',
      nested: { a: ['${steps.lint.output}'] },
    });
    expect(refs.inputs).to.have.members(['env', 'TOKEN']);
    expect(refs.steps).to.have.members(['build', 'lint']);
  });

  it('returns empty lists when there are no references', () => {
    expect(collectRefs({ a: 'plain text', b: 3, c: true })).to.deep.equal({ inputs: [], steps: [] });
  });

  it('dedupes repeated references', () => {
    const refs = collectRefs({ a: '${steps.x.output} ${steps.x.output}', b: '${inputs.y}', c: '${y}' });
    expect(refs.steps).to.deep.equal(['x']);
    expect(refs.inputs).to.have.members(['y']);
  });
});
