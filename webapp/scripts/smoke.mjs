// Headless smoke test: run the built bundle against a stub webapp window and
// assert the App Bar / post-menu entries register and fire the panel POST.
import { readFileSync } from 'node:fs';

const bundle = readFileSync(new URL('../dist/main.js', import.meta.url), 'utf8');

const registered = { appBar: null, postMenu: null };
const calls = [];

globalThis.window = {
    registerPlugin(id, plugin) {
        globalThis.__plugin = plugin;
        globalThis.__pluginId = id;
    },
    setInterval: () => 1,
    clearInterval: () => {},
    location: { pathname: '/t/channels/general' },
};

globalThis.fetch = async (url, opts) => {
    calls.push({ url, body: JSON.parse(opts.body) });
    return { ok: true, json: async () => ({ post_id: 'p' }) };
};

const store = {
    getState: () => ({
        entities: {
            channels: { currentChannelId: 'chan-1', channels: { 'chan-1': { name: 'general', display_name: 'General', team_id: 't1' } } },
            teams: { teams: { t1: { name: 't', display_name: 'Team' } }, currentTeamId: 't1' },
            posts: { posts: { p1: { id: 'p1', channel_id: 'chan-1', root_id: 'root-0' } } },
        },
        views: { rhs: { selectedPostId: 'p1' }, threads: { selectedThreadIdInTeam: {} } },
    }),
    subscribe: () => () => {},
    dispatch: () => {},
};

// No registerRightHandSidebarComponent: the in-chat panel replaced the RHS.
const registry = {
    registerAppBarComponent: (a) => { registered.appBar = a; },
    registerPostDropdownMenuAction: (text, action, filter) => { registered.postMenu = { text, action, filter }; },
};

// eslint-disable-next-line no-eval
eval(bundle);

const plugin = globalThis.__plugin;
if (globalThis.__pluginId !== 'com.nicox.hermes-console') throw new Error('wrong plugin id');
plugin.initialize(registry, store);

if (!registered.appBar) throw new Error('App Bar entry not registered');
if (!registered.postMenu) throw new Error('post menu entry not registered');
if (!registered.appBar.iconUrl.startsWith('data:image/svg+xml')) throw new Error('bad iconUrl');
if (!registered.appBar.tooltipText) throw new Error('missing tooltip');
if (registered.postMenu.text !== 'Open Hermes Console') throw new Error('bad menu text');

const settle = () => new Promise((r) => setTimeout(r, 10));

// App Bar must scope to the open thread, not the channel root.
registered.appBar.action();
await settle();
if (calls.length !== 1) throw new Error('panel POST not fired from App Bar');
if (!calls[0].url.includes('/api/v1/panel')) throw new Error('wrong endpoint');
if (calls[0].body.root_post_id !== 'root-0') throw new Error('App Bar did not resolve the open thread');

// Post menu must resolve a reply to its root post.
calls.length = 0;
registered.postMenu.action('p1');
await settle();
if (calls.length !== 1) throw new Error('panel POST not fired from post menu');
if (calls[0].body.root_post_id !== 'root-0') throw new Error('post menu did not resolve the root');

console.log('smoke OK: app bar + post menu register and post the panel');
