// Re-export the host webapp's ReactDOM from window.ReactDOM.
import type * as ReactDOMNS from 'react-dom';

const RD = (globalThis as unknown as { ReactDOM: typeof ReactDOMNS }).ReactDOM;

export default RD;
