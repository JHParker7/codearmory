/**
 * Step inspector — shown in the pipeline builder's right panel when a step is
 * selected. It surfaces, for the chosen step, the things you need to wire it into
 * a pipeline: what it does (action + summary), its example **inputs** (the `with`
 * config, with ${...} references highlighted and listed), and how to consume its
 * **output** in a later step (`${steps.<name>.output}`). It is read-only — editing
 * happens in the Steps tab and the JSON panel.
 */
import { T } from '../../theme';
import type { Step, WorkflowAction } from '../../api/bff';
import { collectRefs } from './pipelineGraph';

const sectionLabel: React.CSSProperties = {
  fontFamily: T.mono, fontSize: 9, color: T.faint, letterSpacing: 1, textTransform: 'uppercase', margin: '12px 0 6px',
};
const code: React.CSSProperties = {
  fontFamily: T.mono, fontSize: 11, color: T.text, background: T.cardHi, border: `1px solid ${T.border}`, padding: '2px 5px', wordBreak: 'break-all',
};

/** Renders a string with ${...} references tinted so wired inputs stand out. */
function withValue(v: unknown) {
  const s = typeof v === 'string' ? v : JSON.stringify(v);
  const parts = s.split(/(\$\{[^}]+\})/g);
  return (
    <span style={{ fontFamily: T.mono, fontSize: 11, color: T.text, wordBreak: 'break-all' }}>
      {parts.map((p, i) => p.startsWith('${')
        ? <span key={i} style={{ color: T.amber }}>{p}</span>
        : <span key={i}>{p}</span>)}
    </span>
  );
}

function CopyRef({ text }: { text: string }) {
  return (
    <button onClick={() => { navigator.clipboard?.writeText(text).catch(() => {}); }}
      title="copy reference"
      style={{ ...code, cursor: 'pointer', textAlign: 'left', display: 'block', width: '100%' }}>{text} <span style={{ color: T.faint }}>⧉</span></button>
  );
}

export function StepInspector({ step, action, name }: { step: Step | null; action?: WorkflowAction; name?: string }) {
  if (!step) {
    return (
      <div style={{ flex: 1, overflow: 'auto', padding: 14, fontFamily: T.mono, fontSize: 12, color: T.faint }}>
        → select a step in the pipeline to see its inputs and output
      </div>
    );
  }
  const withMap = (step.with ?? {}) as Record<string, unknown>;
  const entries = Object.entries(withMap);
  const refs = collectRefs(withMap);
  // Output is referenced by this occurrence's name (the per-step override if set).
  const refName = name || step.name;
  const outRef = `\${steps.${refName}.output}`;
  // An action with output_map_field (forge/run) outputs ONLY the env vars it
  // captures via output_env — stdout is never the step output.
  const envOutput = !!action?.async?.output_map_field;
  const outputEnv = Array.isArray(withMap.output_env)
    ? (withMap.output_env as unknown[]).filter((x): x is string => typeof x === 'string')
    : [];
  const outputDesc = action?.async?.output_field
    ? `the ${action.async.output_field} of the ${action.name} result`
    : 'this step’s output (the action result)';

  return (
    <div style={{ flex: 1, minHeight: 0, overflow: 'auto', padding: 14 }}>
      <div style={{ fontFamily: T.mono, fontSize: 13, fontWeight: 700, color: T.textHi }}>{step.name}</div>
      <div style={{ fontFamily: T.mono, fontSize: 11, color: T.blue, marginTop: 2 }}>{step.action}</div>
      {(step.description || action?.summary) && (
        <div style={{ fontFamily: T.mono, fontSize: 11, color: T.dim, marginTop: 6, lineHeight: 1.5 }}>{step.description || action?.summary}</div>
      )}

      <div style={sectionLabel}>inputs (with)</div>
      {entries.length === 0 ? (
        <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint }}>no configured inputs</div>
      ) : (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 5 }}>
          {entries.map(([k, v]) => (
            <div key={k} style={{ display: 'flex', gap: 6, alignItems: 'baseline' }}>
              <span style={{ fontFamily: T.mono, fontSize: 11, color: T.green, flexShrink: 0 }}>{k}:</span>
              {withValue(v)}
            </div>
          ))}
        </div>
      )}

      {(refs.inputs.length > 0 || refs.steps.length > 0) && (
        <>
          <div style={sectionLabel}>consumes</div>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
            {refs.inputs.map((n) => (
              <div key={`i-${n}`} style={{ fontFamily: T.mono, fontSize: 11, color: T.text }}>• run input <span style={{ color: T.amber }}>{n}</span></div>
            ))}
            {refs.steps.map((n) => (
              <div key={`s-${n}`} style={{ fontFamily: T.mono, fontSize: 11, color: T.text }}>• output of step <span style={{ color: T.amber }}>{n}</span></div>
            ))}
          </div>
        </>
      )}

      <div style={sectionLabel}>output</div>
      {envOutput ? (
        outputEnv.length > 0 ? (
          <>
            <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 5 }}>captures these env vars as this step’s output — reference each in a later step:</div>
            <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
              {outputEnv.map((k) => <CopyRef key={k} text={`\${steps.${refName}.output.${k}}`} />)}
            </div>
          </>
        ) : (
          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 5 }}>this step produces no output — add output variables in the Steps tab to capture values for later steps.</div>
        )
      ) : (
        <>
          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 5 }}>reference {outputDesc} in a later step:</div>
          <CopyRef text={outRef} />
          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, margin: '6px 0 4px' }}>or a JSON field of it:</div>
          <div style={code}>{`\${steps.${refName}.output.<field>}`}</div>
        </>
      )}
    </div>
  );
}
