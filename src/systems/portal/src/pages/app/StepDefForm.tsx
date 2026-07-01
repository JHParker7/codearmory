/**
 * Reusable step-definition form — creates or edits a pre-configured, reusable Step
 * (a named action + its `with` config). It is the single editor shared by two
 * surfaces:
 *   - the top-level Steps tab (create/edit steps ahead of time), and
 *   - the pipeline builder's right panel (create a step inline from a chosen action,
 *     or edit the step behind the selected block) — so "configure a step" means the
 *     same thing everywhere.
 *
 * A step declares an interface grouped into config (what the step IS), inputs (the
 * params a pipeline supplies — the value set here is the default), and outputs (what
 * later steps can read). Per-occurrence wiring between steps is NOT done here — it
 * lives on the block in the pipeline builder. The tailored `with` inputs are driven
 * by the action schema (./stepSchema); unknown actions fall back to a raw With JSON
 * field. On save it calls createStep/updateStep and reports the saved step via
 * onSaved so callers can refresh their catalog / add a block.
 */
import { useState, useEffect, useMemo } from 'react';
import type { ReactNode, CSSProperties } from 'react';
import { T } from '../../theme';
import { ImageSelect } from '../../components/ImageSelect';
import { createStep, updateStep, listActions, listForgeImages } from '../../api/bff';
import type { Step, WorkflowAction } from '../../api/bff';
import { schemaForAction, buildStepWith, formValsFromWith, WITH_KEY_PREFIX, RAW_WITH_KEY } from './stepSchema';

const inputStyle: CSSProperties = { width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 11, padding: '6px 8px', outline: 'none', boxSizing: 'border-box', marginBottom: 6 };
const labelStyle: CSSProperties = { display: 'block', fontFamily: T.mono, fontSize: 9, color: T.faint, textTransform: 'uppercase', letterSpacing: 0.5, marginBottom: 3 };
const codeStyle: CSSProperties = { fontFamily: T.mono, fontSize: 10.5, color: T.text, background: T.cardHi, border: `1px solid ${T.border}`, padding: '2px 5px', wordBreak: 'break-all', display: 'inline-block' };

export interface StepDefFormProps {
  token: string;
  /** null = create a new step; a Step = edit it. */
  initial: Step | null;
  /** When set, the action is fixed (create-from-a-chosen-action, or editing a step
   *  whose action defines it) and shown read-only instead of as a selector. */
  lockAction?: string;
  /** Called after a successful create/update with the saved step and whether it was
   *  a create (so a caller can add it to the pipeline as a new block). */
  onSaved: (step: Step, wasCreate: boolean) => void;
  onCancel?: () => void;
  /** Focus the name field on mount (in-panel create flows). */
  autoFocus?: boolean;
  /** Prefilled name for a fresh create (ignored when editing). */
  defaultName?: string;
  /** Label for the submit button in create mode (default "[ create ]"). The builder
   *  uses "[ add step ]" since a create there also drops a block into the pipeline. */
  createLabel?: string;
}

export function StepDefForm({ token, initial, lockAction, onSaved, onCancel, autoFocus, defaultName, createLabel }: StepDefFormProps) {
  const [actions, setActions] = useState<WorkflowAction[]>([]);
  const [images, setImages] = useState<string[]>([]);
  const [name, setName] = useState(initial?.name ?? defaultName ?? '');
  const [description, setDescription] = useState(initial?.description ?? '');
  const [timeoutSecs, setTimeoutSecs] = useState(initial?.timeout != null ? String(initial.timeout) : '');
  // Action is fixed when lockAction is set (builder) or when editing; otherwise it
  // defaults to forge/run on a fresh create in the Steps tab.
  const [action, setAction] = useState(lockAction ?? initial?.action ?? 'forge/run');
  const [withVals, setWithVals] = useState<Record<string, string>>(
    () => (initial ? formValsFromWith(initial.action, initial.with ?? {}) : {}),
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Load the action catalog (for the Action selector and the output-reference
  // helper) and the forge image allowlist (so the image field is a picker).
  useEffect(() => { listActions(token).then(setActions).catch(() => {}); }, [token]);
  useEffect(() => { listForgeImages(token).then(setImages).catch(() => {}); }, [token]);

  const actionOptions = useMemo(() => {
    const names = actions.map(a => a.name);
    return names.includes('http') ? names : [...names, 'http'];
  }, [actions]);
  const actionDef = useMemo(() => actions.find(a => a.name === action), [actions, action]);

  const isEdit = !!initial;
  const actionLocked = !!lockAction || isEdit;
  const setWith = (key: string, val: string) => setWithVals(v => ({ ...v, [key]: val }));

  const handleSubmit = async () => {
    if (!name.trim() || !action.trim()) return;
    setBusy(true); setError(null);
    try {
      const withMap = buildStepWith(action, k => withVals[k] ?? '');
      const payload = {
        name: name.trim(),
        description: description.trim() || undefined,
        action: action.trim(),
        with: Object.keys(withMap).length > 0 ? withMap : undefined,
        timeout: timeoutSecs ? parseInt(timeoutSecs, 10) : undefined,
      };
      const saved = initial
        ? await updateStep(token, initial.step_id, payload)
        : await createStep(token, payload);
      onSaved(saved, !initial);
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setBusy(false); }
  };

  // Output-reference helper shown when editing: how a later step consumes this
  // step's output. A forge-style action (output_map_field) outputs ONLY the env
  // vars it captures via output_env — stdout is never a step output.
  const outputHelper = (() => {
    if (!isEdit) return null;
    const refName = name.trim() || initial!.name;
    const envOutput = !!actionDef?.async?.output_map_field;
    const outputEnvRaw = (withVals[WITH_KEY_PREFIX + 'output_env'] ?? '').split(',').map(s => s.trim()).filter(Boolean);
    return (
      <>
        <div style={{ height: 1, background: T.border, margin: '10px 0 8px' }} />
        <label style={labelStyle}>output reference</label>
        {envOutput ? (
          outputEnvRaw.length > 0 ? (
            <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
              {outputEnvRaw.map(k => <code key={k} style={codeStyle}>{`\${steps.${refName}.output.${k}}`}</code>)}
            </div>
          ) : (
            <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>add output variables above to capture values for later steps (stdout is not a step output)</div>
          )
        ) : (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
            <code style={codeStyle}>{`\${steps.${refName}.output}`}</code>
            <span style={{ fontFamily: T.mono, fontSize: 9, color: T.faint }}>or a field of it: {`\${steps.${refName}.output.<field>}`}</span>
          </div>
        )}
      </>
    );
  })();

  return (
    <div>
      {error && <div style={{ color: T.red, fontFamily: T.mono, fontSize: 10, marginBottom: 6 }}>{error}</div>}

      <label style={labelStyle}>name *</label>
      <input value={name} onChange={e => setName(e.target.value)} placeholder="unit_tests" autoFocus={autoFocus} style={inputStyle} />

      <label style={labelStyle}>action *</label>
      {actionLocked ? (
        <div style={{ ...inputStyle, color: T.blue, background: T.bg }}>{action}</div>
      ) : actionOptions.length > 0 ? (
        <select value={action} onChange={e => { setAction(e.target.value); setWithVals({}); setError(null); }} style={inputStyle}>
          {action && !actionOptions.includes(action) && <option value={action}>{action}</option>}
          {actionOptions.map(a => <option key={a} value={a}>{a}</option>)}
        </select>
      ) : (
        <input value={action} onChange={e => setAction(e.target.value)} placeholder="forge/run" style={inputStyle} />
      )}

      {(() => {
        const fields = schemaForAction(action);
        const renderField = (f: ReturnType<typeof schemaForAction>[number]) => {
          const key = WITH_KEY_PREFIX + f.key;
          const val = withVals[key] ?? '';
          return (
            <div key={key}>
              <label style={labelStyle}>{f.label}{f.required ? ' *' : ''}</label>
              {f.catalog === 'image' && images.length > 0 ? (
                <div style={{ marginBottom: 6 }}>
                  <ImageSelect value={val} onChange={v => setWith(key, v)} options={images} placeholder={f.placeholder} fontSize={11} />
                </div>
              ) : f.multiline ? (
                <textarea value={val} onChange={e => setWith(key, e.target.value)} placeholder={f.placeholder}
                  rows={f.key === 'run' ? 3 : 2} style={{ ...inputStyle, resize: 'vertical' }} />
              ) : (
                <input value={val} onChange={e => setWith(key, e.target.value)} placeholder={f.placeholder} style={inputStyle} />
              )}
            </div>
          );
        };
        // config (what the step IS — set once on the definition), inputs (the params
        // a pipeline supplies; the value set here is the default), and outputs (what
        // later steps can read). The advanced With field (if any) is the escape
        // hatch and trails the rest.
        const config = fields.filter(f => f.config);
        const advanced = fields.filter(f => f.key === RAW_WITH_KEY);
        const outputs = fields.filter(f => f.output);
        const inputs = fields.filter(f => !f.config && !f.output && f.key !== RAW_WITH_KEY);
        const section = (label: string, help: ReactNode, fs: typeof fields) => fs.length > 0 && (
          <>
            <div style={{ height: 1, background: T.border, margin: '10px 0 8px' }} />
            <label style={labelStyle}>{label}</label>
            <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 6, lineHeight: 1.4 }}>{help}</div>
            {fs.map(renderField)}
          </>
        );
        return (
          <>
            {section('config', 'what this step runs — fixed by the step definition, the same in every pipeline', config)}
            {section('inputs', <>values the pipeline supplies — what you set here is the <span style={{ color: T.dim }}>default</span>; connect an input to another step's output in the pipeline builder</>, inputs)}
            {section('outputs', <>values this step produces for later steps (read as <span style={{ color: T.dim }}>{'${steps.<step>.output.VAR}'}</span>) — without any, the step produces no output (stdout is not a step output)</>, outputs)}
            {advanced.map(renderField)}
          </>
        );
      })()}

      <label style={labelStyle}>timeout</label>
      <input value={timeoutSecs} onChange={e => setTimeoutSecs(e.target.value)} placeholder="seconds (default 30)" style={inputStyle} />

      <label style={labelStyle}>description</label>
      <input value={description} onChange={e => setDescription(e.target.value)} placeholder="optional" style={inputStyle} />

      {outputHelper}

      <div style={{ display: 'flex', gap: 6, marginTop: 10 }}>
        <button onClick={handleSubmit} disabled={!name.trim() || !action.trim() || busy}
          style={{ flex: 1, background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 10, fontWeight: 600, padding: '6px 0', cursor: 'pointer', opacity: (!name.trim() || !action.trim() || busy) ? 0.6 : 1 }}>
          {busy ? '[ · · · ]' : isEdit ? '[ save ]' : (createLabel ?? '[ create ]')}
        </button>
        {onCancel && (
          <button onClick={onCancel} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '6px 10px', cursor: 'pointer' }}>✕</button>
        )}
      </div>
    </div>
  );
}
