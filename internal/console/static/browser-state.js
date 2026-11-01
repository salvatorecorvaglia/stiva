// Browser navigation and request ownership, independent of DOM rendering.
((root) => {
    function createBrowserState() {
        const state = {
            bucket: '',
            prefix: '',
            searchPrefix: '',
            pageIndex: 0,
            pageTokens: [''],
            items: [],
            sortField: 'name',
            sortOrder: 'asc',
        };
        let generation = 0;
        let controller = null;
        function invalidate() {
            generation++;
            controller?.abort();
            controller = null;
        }
        function resetPagination() {
            state.pageIndex = 0;
            state.pageTokens = [''];
            invalidate();
        }
        function beginRequest() {
            invalidate();
            controller = new AbortController();
            const owner = controller;
            const revision = generation;
            const snapshot = {
                bucket: state.bucket,
                prefix: state.prefix,
                searchPrefix: state.searchPrefix,
                pageIndex: state.pageIndex,
                continuationToken: state.pageTokens[state.pageIndex] || '',
            };
            return Object.freeze({
                ...snapshot,
                signal: owner.signal,
                isCurrent: () =>
                    !owner.signal.aborted &&
                    revision === generation &&
                    state.bucket === snapshot.bucket &&
                    state.prefix === snapshot.prefix &&
                    state.searchPrefix === snapshot.searchPrefix &&
                    state.pageIndex === snapshot.pageIndex,
            });
        }
        return { state, invalidate, resetPagination, beginRequest };
    }
    const browser = { createBrowserState };
    if (typeof module !== 'undefined' && module.exports) module.exports = browser;
    else root.StivaBrowser = browser;
})(typeof window !== 'undefined' ? window : this);
