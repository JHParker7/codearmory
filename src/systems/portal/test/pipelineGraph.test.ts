import { expect } from 'chai';
import {
  stagesFromSteps, blocksFromSteps, stepsFromBlocks, stagesOf, StepRef, Block,
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
