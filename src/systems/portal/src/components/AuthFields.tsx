import { useState } from 'react';
import { T } from '../theme';

// Shared terminal-styled form primitives for the auth/setup pages (Login, Signup,
// Setup). Kept here so the bootstrap setup flow reuses the exact same look without
// duplicating the field markup.

export function StrengthBar({ score, checks }: { score: number; checks: Record<string, boolean> }) {
  const total = 16;
  const filled = Math.round((score / 5) * total);
  const color = score >= 4 ? T.green : score >= 3 ? T.amber : score >= 1 ? T.red : T.faint;
  const label = ['empty', 'weak', 'fair', 'good', 'strong', 'excellent'][score];
  return (
    <div style={{ fontFamily: T.mono, fontSize: 11.5, marginTop: -4, marginBottom: 16 }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, color: T.dim }}>
        <span style={{ color: T.faint }}>strength</span>
        <span style={{ color, letterSpacing: 1 }}>
          [{'█'.repeat(filled)}<span style={{ color: T.border }}>{'░'.repeat(total - filled)}</span>]
        </span>
        <span style={{ color, marginLeft: 'auto' }}>{label}</span>
      </div>
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: '2px 14px', marginTop: 6, color: T.faint, fontSize: 10.5, letterSpacing: 0.2 }}>
        {([['len', 'len ≥ 8'], ['upper', '[A-Z]'], ['lower', '[a-z]'], ['num', '[0-9]'], ['sym', '[!@#$%]']] as [string, string][]).map(([k, l]) => (
          <span key={k} style={{ color: checks[k] ? T.green : T.faint }}>{checks[k] ? '[x]' : '[ ]'} {l}</span>
        ))}
      </div>
    </div>
  );
}

export function PromptField({
  prompt, value, onChange, type = 'text', placeholder, hint, rightSlot,
}: {
  prompt: string; value: string; onChange: (v: string) => void;
  type?: string; placeholder?: string; hint?: string;
  rightSlot?: React.ReactNode;
}) {
  const [focused, setFocused] = useState(false);
  return (
    <div style={{ marginBottom: 16 }}>
      <div style={{ display: 'flex', alignItems: 'baseline', justifyContent: 'space-between', marginBottom: 5 }}>
        <span style={{ fontSize: 12, color: focused ? T.green : T.dim, fontFamily: T.mono, letterSpacing: 0.3 }}>
          <span style={{ color: T.green }}>$</span> {prompt}
        </span>
      </div>
      <div style={{ display: 'flex', alignItems: 'center', background: T.cardHi, border: `1px solid ${focused ? T.green : T.border}`, boxShadow: focused ? `0 0 0 3px ${T.greenSoft}` : 'none', transition: 'border-color .15s, box-shadow .15s', padding: '8px 12px' }}>
        <span style={{ color: T.green, fontFamily: T.mono, fontSize: 13.5, marginRight: 8, userSelect: 'none' }}>›</span>
        <input
          type={type} value={value}
          onChange={(e) => onChange(e.target.value)}
          onFocus={() => setFocused(true)} onBlur={() => setFocused(false)}
          placeholder={placeholder}
          style={{ flex: 1, background: 'transparent', border: 0, outline: 'none', color: T.text, fontFamily: T.mono, fontSize: 13.5, letterSpacing: 0.2, padding: 0 }}
        />
        {rightSlot}
      </div>
      {hint && <div style={{ fontSize: 11, color: T.faint, fontFamily: T.mono, marginTop: 5, letterSpacing: 0.2 }}>{hint}</div>}
    </div>
  );
}
