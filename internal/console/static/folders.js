// Process one listing page at a time. Continuation tokens remain valid when
// already-visited keys are deleted; no whole-folder list is retained in memory.
((root) => {
    async function deleteFolderContents({ api, bucket, prefix }) {
        const base = `/api/buckets/${encodeURIComponent(bucket)}/objects`;
        let continuation = '';
        let deleted = 0;
        let failureCount = 0;
        const failures = [];
        for (;;) {
            const params = new URLSearchParams({ prefix, recursive: '1', maxKeys: '1000' });
            if (continuation) params.set('continuationToken', continuation);
            const response = await api('GET', `${base}?${params}`);
            if (!response.ok)
                throw new Error(
                    `Folder listing failed (HTTP ${response.status}); ${deleted} objects deleted`,
                );
            const page = await response.json();
            const next = page.nextContinuationToken;
            if (page.isTruncated && (!next || next === continuation)) {
                throw new Error(`Folder listing did not advance; ${deleted} objects deleted`);
            }
            const keys = page.items.filter((item) => !item.isPrefix).map((item) => item.key);
            if (keys.length > 0) {
                const result = await api('POST', `${base}/delete`, { keys });
                if (!result.ok)
                    throw new Error(
                        `Folder deletion failed (HTTP ${result.status}); ${deleted} objects deleted`,
                    );
                const data = await result.json();
                deleted += data.deleted.length;
                failureCount += data.errors.length;
                if (failures.length === 0 && data.errors.length) failures.push(data.errors[0]);
            }
            if (!page.isTruncated) return { deleted, failures, failureCount };
            continuation = next;
        }
    }
    const folders = { deleteFolderContents };
    if (typeof module !== 'undefined' && module.exports) module.exports = folders;
    else root.StivaFolders = folders;
})(typeof window !== 'undefined' ? window : this);
