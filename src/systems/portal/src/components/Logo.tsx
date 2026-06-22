import { T } from '../theme';

export function Logo({ size = 22 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 22 22" fill="none">
      <rect x="2" y="2" width="18" height="18" stroke={T.green} strokeWidth="1.4" />
      <path d="M6 7l4 4-4 4M11 15h5" stroke={T.green} strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}
