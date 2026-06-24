import type { ReactNode } from 'react';
import { T } from '../theme';

type Tone = 'green' | 'amber' | 'dim' | 'red';

/** Small uppercase status badge tinted by `tone` (green also shows a status dot); used for states like enabled/pending/error/disabled. */
export function Pill({ children, tone = 'green' }: { children: ReactNode; tone?: Tone }) {
  const c = tone === 'green' ? T.green : tone === 'amber' ? T.amber : tone === 'red' ? T.red : T.dim;
  const bg = tone === 'green' ? T.greenSoft : tone === 'amber' ? T.amberSoft : tone === 'red' ? T.redSoft : 'transparent';
  return (
    <span style={{
      display: 'inline-flex', alignItems: 'center', gap: 6,
      padding: '3px 8px', background: bg,
      border: `1px solid ${c}`, color: c,
      fontSize: 10.5, fontFamily: T.mono, letterSpacing: 0.5,
      textTransform: 'uppercase',
    }}>
      {tone === 'green' && <span style={{ width: 5, height: 5, borderRadius: 3, background: c }} />}
      {children}
    </span>
  );
}
