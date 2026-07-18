import { useCallback, useEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { T } from '../theme';

type Tone = 'red' | 'amber';

export interface ConfirmOptions {
  /** Heading shown at the top of the dialog. Defaults to "Confirm deletion". */
  title?: string;
  /** Body explaining what is about to happen. Plain string or rich node. */
  message: ReactNode;
  /** Label for the destructive action button. Defaults to "delete". */
  confirmLabel?: string;
  /** Label for the dismiss button. Defaults to "cancel". */
  cancelLabel?: string;
  /** Accent for the confirm button. Defaults to "red". */
  tone?: Tone;
  /**
   * When set, the user must type this exact string before the confirm button
   * unlocks — a type-to-confirm guard for high-consequence deletes.
   */
  requireText?: string;
}

/**
 * Presentational confirmation modal matching the app's overlay pattern
 * (fixed backdrop + centred card). Escape or a backdrop click cancels;
 * the cancel button is focused by default so destructive actions are never
 * the keyboard default.
 */
export function ConfirmDialog({
  title = 'Confirm deletion',
  message,
  confirmLabel = 'delete',
  cancelLabel = 'cancel',
  tone = 'red',
  requireText,
  onConfirm,
  onCancel,
}: ConfirmOptions & { onConfirm: () => void; onCancel: () => void }) {
  const cancelRef = useRef<HTMLButtonElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const [typed, setTyped] = useState('');
  const accent = tone === 'amber' ? T.amber : T.red;
  const accentSoft = tone === 'amber' ? T.amberSoft : T.redSoft;
  // Type-to-confirm: lock the destructive action until the typed text matches.
  const locked = requireText != null && typed !== requireText;

  useEffect(() => {
    // With type-to-confirm, focus the input so the user can start typing; else
    // focus cancel so Enter activates the safe choice. Escape always cancels.
    if (requireText != null) inputRef.current?.focus();
    else cancelRef.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') { e.stopPropagation(); onCancel(); }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [onConfirm, onCancel, requireText]);

  return (
    <div onClick={onCancel} style={{ position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.7)', zIndex: 60, display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
      <div onClick={e => e.stopPropagation()} role="dialog" aria-modal="true" style={{ width: 420, maxWidth: '92vw', background: T.card, border: `1px solid ${T.borderHi}` }}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '12px 18px', borderBottom: `1px solid ${T.border}`, background: T.cardHi }}>
          <span style={{ fontFamily: T.mono, fontSize: 12, color: T.text }}><span style={{ color: accent }}>!</span> {title}</span>
          <button onClick={onCancel} style={{ background: 'transparent', border: 0, color: T.faint, cursor: 'pointer', fontSize: 16 }}>×</button>
        </div>
        <div style={{ padding: '18px 20px', fontFamily: T.mono, fontSize: 12, lineHeight: 1.6, color: T.dim }}>{message}</div>
        {requireText != null && (
          <div style={{ padding: '0 20px 4px' }}>
            <input
              ref={inputRef}
              value={typed}
              onChange={e => setTyped(e.target.value)}
              onKeyDown={e => { if (e.key === 'Enter' && !locked) onConfirm(); }}
              placeholder={requireText}
              aria-label="Type the name to confirm"
              style={{ width: '100%', boxSizing: 'border-box', background: T.bg, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 12, padding: '7px 10px' }}
            />
          </div>
        )}
        <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8, padding: '18px 20px 18px' }}>
          <button ref={cancelRef} onClick={onCancel}
            style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '6px 14px', cursor: 'pointer' }}>
            [ {cancelLabel} ]
          </button>
          <button onClick={onConfirm} disabled={locked}
            style={{ background: accentSoft, border: `1px solid ${accent}`, color: accent, fontFamily: T.mono, fontSize: 11, padding: '6px 14px', cursor: locked ? 'not-allowed' : 'pointer', opacity: locked ? 0.45 : 1 }}>
            [ {confirmLabel} ]
          </button>
        </div>
      </div>
    </div>
  );
}

/**
 * Imperative confirm: returns a `confirm(opts)` that resolves to true/false and
 * the dialog element to render once. Usage:
 *
 *   const [confirm, confirmEl] = useConfirm();
 *   ...
 *   const onDelete = async () => { if (!(await confirm({ message: 'Delete X?' }))) return; ...do it... };
 *   ...
 *   return (<>{confirmEl}...</>);
 */
export function useConfirm(): [(opts: ConfirmOptions) => Promise<boolean>, ReactNode] {
  const [opts, setOpts] = useState<ConfirmOptions | null>(null);
  const resolver = useRef<((v: boolean) => void) | null>(null);

  const confirm = useCallback((o: ConfirmOptions) => {
    setOpts(o);
    return new Promise<boolean>(resolve => { resolver.current = resolve; });
  }, []);

  const settle = useCallback((result: boolean) => {
    setOpts(null);
    resolver.current?.(result);
    resolver.current = null;
  }, []);

  const element = opts
    ? <ConfirmDialog {...opts} onConfirm={() => settle(true)} onCancel={() => settle(false)} />
    : null;

  return [confirm, element];
}
