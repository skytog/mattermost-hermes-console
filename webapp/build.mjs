import { build } from 'esbuild';
import path from 'node:path';

// Mattermost loads plugin bundles as a plain <script>, so emit a self-contained
// IIFE that calls window.registerPlugin. `react` and the JSX runtime are resolved
// to shims that re-export the host webapp's window globals, so the bundle shares
// ONE React instance (bundling a second copy leaves our hooks without a dispatcher).
const shim = (specifier, file) => ({
    name: `shim-${specifier}`,
    setup(b) {
        b.onResolve({ filter: new RegExp(`^${specifier.replace('/', '\\/')}$`) }, () => ({ path: path.resolve(file) }));
    },
});

await build({
    entryPoints: ['src/index.tsx'],
    bundle: true,
    format: 'iife',
    target: ['es2018'],
    jsx: 'transform',
    minify: false,
    sourcemap: false,
    outfile: 'dist/main.js',
    logLevel: 'info',
    define: { 'process.env.NODE_ENV': '"production"' },
    plugins: [
        shim('react', 'src/shims/react.ts'),
        shim('react-dom', 'src/shims/react-dom.ts'),
    ],
});
