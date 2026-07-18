/** Pre-typed Redux hooks — use these instead of the bare react-redux hooks so dispatch/state are fully typed against the store. */
import { useDispatch, useSelector } from 'react-redux';
import type { AppDispatch, RootState } from './index';

/** `useDispatch` bound to the store's AppDispatch (thunk-aware). */
export const useAppDispatch = useDispatch.withTypes<AppDispatch>();
/** `useSelector` bound to RootState so selectors are typed without an explicit annotation. */
export const useAppSelector = useSelector.withTypes<RootState>();
