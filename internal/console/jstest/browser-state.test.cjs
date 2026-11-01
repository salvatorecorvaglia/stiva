'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const { createBrowserState } = require('../static/browser-state.js');

test('a new listing aborts the old request and preserves its immutable target', () => {
    const browser = createBrowserState();
    browser.state.bucket = 'original';
    const first = browser.beginRequest();
    browser.state.bucket = 'other';
    const second = browser.beginRequest();
    assert.equal(first.bucket, 'original');
    assert.equal(first.signal.aborted, true);
    assert.equal(first.isCurrent(), false);
    assert.equal(second.isCurrent(), true);
});

test('page, search and prefix changes invalidate responses before another fetch', () => {
    for (const [field, value] of [
        ['pageIndex', 1],
        ['searchPrefix', 'query'],
        ['prefix', 'folder/'],
    ]) {
        const browser = createBrowserState();
        const request = browser.beginRequest();
        browser.state[field] = value;
        assert.equal(request.isCurrent(), false, field);
    }
});

test('pagination reset and logout cancellation leave independent browser instances intact', () => {
    const first = createBrowserState();
    const second = createBrowserState();
    first.state.pageIndex = 1;
    first.state.pageTokens.push('next');
    const request = first.beginRequest();
    assert.equal(request.continuationToken, 'next');
    first.resetPagination();
    assert.equal(request.signal.aborted, true);
    assert.deepEqual(first.state.pageTokens, ['']);
    const otherRequest = second.beginRequest();
    first.invalidate();
    assert.equal(otherRequest.isCurrent(), true);
});
