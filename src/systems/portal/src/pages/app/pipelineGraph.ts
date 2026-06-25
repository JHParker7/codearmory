/**
 * Pure mapping between the Scratch-style block builder and the workflows backend
 * model — the portal twin of the CLI's tuiPipelineStages.
 *
 * The backend stores a pipeline as an *ordered* list of steps, each with an
 * optional parallel_group: consecutive steps sharing a non-nil parallel_group run
 * concurrently (one "stage"); every other step is its own sequential stage. The
 * builder is a vertical stack of blocks (one per step occurrence) where a block
 * can be "linked" to the block above it to run in the same stage. This module is
 * the (pure, unit-tested) bridge between the two:
 *
 *   blocksFromSteps  ordered steps  -> blocks (first block of each stage starts a
 *                                       stage; the rest are parallelWithPrev)
 *   stepsFromBlocks  blocks         -> ordered steps with parallel_group
 *   stagesOf         blocks         -> blocks grouped into stage bands (rendering)
 *
 * Node/library types are kept out of here so the mapping can be tested under
 * mocha without the UI; the block component adapts these shapes.
 */

/** A pipeline step reference as the workflows API stores/accepts it. */
export interface StepRef {
  step_id: string;
  parallel_group?: number | null;
}

/** One step occurrence in the builder. `parallelWithPrev` links it into the same
 * stage as the block above (they run concurrently). The first block is always a
 * stage start, so its flag is forced false. `uid` is unique per occurrence so the
 * same step can appear more than once. */
export interface Block {
  uid: string;
  stepId: string;
  parallelWithPrev: boolean;
}

/**
 * Collapse an ordered step list into sequential stages of step_ids. Mirrors the
 * CLI's tuiPipelineStages: only *consecutive* steps with the same non-nil
 * parallel_group merge into one stage.
 */
export function stagesFromSteps(steps: StepRef[]): string[][] {
  const stages: string[][] = [];
  let prevGroup: number | null | undefined = undefined;
  for (const s of steps) {
    const g = s.parallel_group ?? null;
    if (g !== null && g === prevGroup) {
      stages[stages.length - 1].push(s.step_id);
    } else {
      stages.push([s.step_id]);
    }
    prevGroup = g;
  }
  return stages;
}

/** Stored steps -> builder blocks. The first block of each stage starts the stage
 * (parallelWithPrev=false); any further blocks in a parallel stage link upward. */
export function blocksFromSteps(steps: StepRef[]): Block[] {
  const blocks: Block[] = [];
  let i = 0;
  for (const stage of stagesFromSteps(steps)) {
    stage.forEach((stepId, idx) => {
      blocks.push({ uid: `b${i++}`, stepId, parallelWithPrev: idx > 0 });
    });
  }
  return blocks;
}

/** Group blocks into stage bands: a run of [start, linked, linked…] is one stage.
 * The first block always starts a stage regardless of its flag. */
export function stagesOf(blocks: Block[]): Block[][] {
  const stages: Block[][] = [];
  blocks.forEach((b, idx) => {
    if (idx === 0 || !b.parallelWithPrev) stages.push([b]);
    else stages[stages.length - 1].push(b);
  });
  return stages;
}

/** Builder blocks -> ordered steps with parallel_group. Each stage band of >1
 * block gets a shared group; a solo block gets null. Order follows the blocks. */
export function stepsFromBlocks(blocks: Block[]): StepRef[] {
  const out: StepRef[] = [];
  let group = 0;
  for (const stage of stagesOf(blocks)) {
    if (stage.length > 1) {
      const g = group++;
      for (const b of stage) out.push({ step_id: b.stepId, parallel_group: g });
    } else {
      out.push({ step_id: stage[0].stepId, parallel_group: null });
    }
  }
  return out;
}
