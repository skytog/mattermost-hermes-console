// Selected operation target (SPEC §5.1). Kept module-level so the RHS survives
// the Mattermost thread view opening/closing over the same region.

export type Target = { channelId: string; rootPostId: string | null };

type Store = {
    getState: () => any;
    dispatch: (a: any) => void;
};

let current: Target | null = null;
const listeners = new Set<() => void>();

function emit() {
    for (const l of listeners) l();
}

export const target = {
    subscribe(l: () => void) {
        listeners.add(l);
        return () => listeners.delete(l);
    },
    /** Stable snapshot for useSyncExternalStore. */
    get(): Target | null {
        return current;
    },
    set(next: Target | null) {
        current = next;
        emit();
    },
    clear() {
        this.set(null);
    },
    currentChannelId(store: Store): string {
        return store.getState().entities.channels.currentChannelId;
    },
    /** Team the user is currently viewing (thread selection is per-team). */
    currentTeamId(store: Store): string {
        return store.getState().entities.teams.currentTeamId;
    },
    /**
     * Root post of the thread the user currently has open, if any.
     * RHS selected post wins; otherwise the thread sidebar's selection.
     */
    threadRootFromView(store: Store): string | null {
        const s = store.getState();
        const selectedPostId: string | undefined = s.views?.rhs?.selectedPostId;
        if (selectedPostId) {
            const post = s.entities.posts.posts[selectedPostId];
            if (post) return post.root_id || post.id;
        }
        const teamId = this.currentTeamId(store);
        const selected = s.views?.threads?.selectedThreadIdInTeam?.[teamId];
        return selected || null;
    },
    /** App Bar entry: prefer the open thread, else the channel root (SPEC §5.1). */
    setFromAppBar(store: Store): Target {
        const next = {
            channelId: this.currentChannelId(store),
            rootPostId: this.threadRootFromView(store),
        };
        this.set(next);
        return next;
    },
    /** Post menu entry: a reply resolves to its root post (SPEC §5.1). */
    setFromPost(store: Store, postId: string): Target | null {
        const posts = store.getState().entities.posts.posts;
        const post = posts[postId];
        if (!post) return null;
        const next = { channelId: post.channel_id, rootPostId: post.root_id || post.id };
        this.set(next);
        return next;
    },
    channelName(store: Store, channelId: string): string {
        const ch = store.getState().entities.channels.channels[channelId];
        return ch ? ch.display_name || ch.name : channelId;
    },
    teamName(store: Store, channelId: string): string {
        const ch = store.getState().entities.channels.channels[channelId];
        if (!ch) return '';
        const team = store.getState().entities.teams.teams[ch.team_id];
        return team ? team.display_name || team.name : '';
    },
};
