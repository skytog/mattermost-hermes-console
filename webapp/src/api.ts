// Thin API client. The browser only ever sends the Mattermost session; the
// actor (Mattermost-User-ID) is attached by the server plugin.

const BASE = '/plugins/com.nicox.hermes-console/api/v1';

export class ApiError extends Error {
    constructor(public status: number, public code: string, message: string) {
        super(message);
    }
}

async function post<T>(path: string, body: unknown): Promise<T> {
    const res = await fetch(BASE + path, {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
        body: JSON.stringify(body),
    });
    if (!res.ok) {
        let code = 'error';
        let message = res.statusText;
        try {
            const parsed = await res.json();
            code = parsed.error ?? code;
            message = parsed.message ?? message;
        } catch {
            /* keep defaults */
        }
        throw new ApiError(res.status, code, message);
    }
    return (await res.json()) as T;
}

export const api = {
    /** Post a control panel into a channel or thread. */
    panel: (channelId: string, rootPostId: string | null) =>
        post<{ post_id: string }>('/panel', {
            channel_id: channelId,
            root_post_id: rootPostId ?? '',
        }),
};
