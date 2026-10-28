import { api } from './api';
import { target } from './target';

// The panel lives in the channel/thread as a normal post, so it coexists with
// the thread view (the Mattermost right sidebar is a single slot). The webapp
// only provides entry points that post it.

const ICON =
    'data:image/svg+xml;utf8,' +
    encodeURIComponent(
        '<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="16" rx="2"/><path d="M7 9h10"/><path d="M7 13h6"/></svg>',
    );

class Plugin {
    initialize(registry: any, store: any) {
        const openPanel = (channelId: string, rootPostId: string | null) => {
            void api.panel(channelId, rootPostId).catch(() => undefined);
        };

        // App Bar entry: prefer the open thread, else the channel root (SPEC §5.1).
        registry.registerAppBarComponent({
            iconUrl: ICON,
            tooltipText: 'Hermes Console',
            action: () => {
                const t = target.setFromAppBar(store);
                openPanel(t.channelId, t.rootPostId);
            },
        });

        // Post menu entry: a reply resolves to its root post (SPEC §5.1).
        registry.registerPostDropdownMenuAction(
            'Open Hermes Console',
            (postId: string) => {
                const t = target.setFromPost(store, postId);
                if (t) openPanel(t.channelId, t.rootPostId);
            },
            () => true,
        );
    }
}

(window as any).registerPlugin('com.nicox.hermes-console', new Plugin());
