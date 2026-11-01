'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const { deleteFolderContents } = require('../static/folders.js');

test('folder deletion stays paged, retains bucket, and bounds failure details', async () => {
    const pending = Array.from({ length: 3500 }, (_, i) => `folder/${String(i).padStart(5, '0')}`);
    let largestBatch = 0;
    const result = await deleteFolderContents({
        bucket: 'original',
        prefix: 'folder/',
        async api(method, path, body) {
            assert.ok(path.startsWith('/api/buckets/original/objects'));
            if (method === 'GET') {
                const token =
                    new URL(`http://test${path}`).searchParams.get('continuationToken') || '';
                const remaining = pending.filter((key) => key > token);
                const keys = remaining.slice(0, 1000);
                return {
                    ok: true,
                    json: async () => ({
                        items: keys.map((key) => ({ key })),
                        isTruncated: remaining.length > keys.length,
                        nextContinuationToken: keys.at(-1),
                    }),
                };
            }
            largestBatch = Math.max(largestBatch, body.keys.length);
            return {
                ok: true,
                json: async () => ({
                    deleted: [],
                    errors: body.keys.map((key) => ({ key, error: 'denied' })),
                }),
            };
        },
    });
    assert.equal(largestBatch, 1000);
    assert.equal(result.failureCount, 3500);
    assert.equal(result.failures.length, 1);
});

test('folder deletion refuses nonadvancing pagination before deleting that page', async () => {
    let calls = 0;
    await assert.rejects(
        deleteFolderContents({
            bucket: 'original',
            prefix: 'folder/',
            async api() {
                calls++;
                return {
                    ok: true,
                    json: async () => ({ items: [{ key: 'folder/key' }], isTruncated: true }),
                };
            },
        }),
        /did not advance/,
    );
    assert.equal(calls, 1);
});
