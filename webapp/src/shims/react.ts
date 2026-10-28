// Mattermost exposes its own React on window.React (plugins/export.ts). Re-export
// it so the bundle shares one React instance with the host webapp — bundling a
// second copy would leave our hooks without a dispatcher.
import type * as ReactNS from 'react';

const R = (globalThis as unknown as { React: typeof ReactNS }).React;

export default R;

export const createElement: typeof ReactNS.createElement = R.createElement;
export const Fragment: typeof ReactNS.Fragment = R.Fragment;
export const useState: typeof ReactNS.useState = R.useState;
export const useEffect: typeof ReactNS.useEffect = R.useEffect;
export const useRef: typeof ReactNS.useRef = R.useRef;
export const useCallback: typeof ReactNS.useCallback = R.useCallback;
export const useSyncExternalStore: typeof ReactNS.useSyncExternalStore = R.useSyncExternalStore;
