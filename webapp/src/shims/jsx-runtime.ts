// Re-export the host webapp's JSX runtime from window.ReactJSXRuntime.
import type * as Runtime from 'react/jsx-runtime';

const R = (globalThis as unknown as { ReactJSXRuntime: typeof Runtime }).ReactJSXRuntime;

export const jsx: typeof Runtime.jsx = R.jsx;
export const jsxs: typeof Runtime.jsxs = R.jsxs;
export const Fragment: typeof Runtime.Fragment = R.Fragment;
