'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

test('dismissing a confirmation cancels it and removes earlier listeners', async () => {
    const nodes = new Map();
    const node = (id) => {
        if (!nodes.has(id))
            nodes.set(id, {
                listeners: new Set(),
                addEventListener(_, fn) {
                    this.listeners.add(fn);
                },
                removeEventListener(_, fn) {
                    this.listeners.delete(fn);
                },
                querySelectorAll() {
                    return [];
                },
            });
        return nodes.get(id);
    };
    const { createConfirmation } = require('../static/dialogs.js');
    const context = {
        confirmPremium: createConfirmation({
            document: { getElementById: node },
            openModal() {},
            closeModal() {},
        }),
    };
    const first = context.confirmPremium('First');
    node('delete-modal').cancelConfirmation();
    assert.equal(await first, false);
    assert.equal(node('confirm-delete-btn').listeners.size, 0);
    const second = context.confirmPremium('Second');
    for (const listener of [...node('confirm-delete-btn').listeners]) listener();
    assert.equal(await second, true);
    assert.equal(node('confirm-delete-btn').listeners.size, 0);
});

test('multipart failure joins other workers before aborting server state', async () => {
    const source = fs.readFileSync(require.resolve('../static/transfers.js'), 'utf8');
    const begin = source.indexOf('    async function uploadLargeFile(');
    const end = source.indexOf('        return { uploadSmallFile, uploadLargeFile };', begin);
    let active = 0;
    let aborted = false;
    const context = vm.createContext({
        AbortController,
        partSizeFor: () => 5,
        PART_ATTEMPTS: 1,
        PART_CONCURRENCY: 3,
        UploadCancelled: class extends Error {},
        FormData: class {
            append() {}
        },
        setUploadProgress() {},
        retryDelay: () => 0,
        sleep: () => Promise.resolve(),
        async api(_method, path) {
            if (path.endsWith('/abort')) {
                assert.equal(active, 0);
                aborted = true;
            }
            return { ok: true, json: async () => ({ uploadId: 'test' }) };
        },
        async sendForm() {
            active++;
            const first = active === 1;
            await new Promise((resolve) => setTimeout(resolve, first ? 0 : 20));
            active--;
            if (first) throw Object.assign(new Error('rejected'), { status: 400 });
            return JSON.stringify({ partNumber: 2, etag: 'etag' });
        },
    });
    vm.runInContext(source.slice(begin, end), context);
    await assert.rejects(
        context.uploadLargeFile(
            { bucket: 'original', xhrs: new Set() },
            {
                size: 20,
                slice() {
                    return {};
                },
            },
            'key',
            1,
            1,
            'name',
        ),
        /rejected/,
    );
    assert.equal(aborted, true);
    assert.equal(active, 0);
});
