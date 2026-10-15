// Unit tests for internal/console/static/lib.js, run by `node --test` (see
// ../js_test.go). They live outside static/ so they are not embedded and
// served with the console.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const lib = require('../static/lib.js');

const MiB = 1024 * 1024;

test('partSizeFor keeps multipart uploads within the 10,000-part limit', () => {
    // Small and ordinary files use the 5 MiB S3 minimum.
    assert.equal(lib.partSizeFor(12 * MiB), 5 * MiB);
    assert.equal(lib.partSizeFor(40 * 1024 * MiB), 5 * MiB);
    // A fixed 5 MiB part used to cap console uploads at ~48.8 GiB.
    for (const size of [60 * 1024 * MiB, 500 * 1024 * MiB, 4 * 1024 * 1024 * MiB]) {
        const part = lib.partSizeFor(size);
        assert.ok(Math.ceil(size / part) <= 10000, `${size} bytes needs ${Math.ceil(size / part)} parts`);
        assert.equal(part % MiB, 0, 'parts are whole MiB');
    }
    // Beyond 10,000 parts of the 5 GiB maximum, S3 cannot hold the object.
    assert.equal(lib.partSizeFor(10000 * 5 * 1024 * MiB + 1), null);
});

test('validateFolderName rejects names that are not a single path segment', () => {
    assert.equal(lib.validateFolderName('reports-2026'), '');
    assert.equal(lib.validateFolderName('Ünïcödé földer'), '');
    for (const bad of ['', '   ', '.', '..', 'a/b', 'a\\b', 'tab\there', 'x'.repeat(256)]) {
        assert.notEqual(lib.validateFolderName(bad), '', `${JSON.stringify(bad)} should be rejected`);
    }
});

test('objectKeyFor keeps the folder structure of dropped folders', () => {
    assert.equal(lib.objectKeyFor('', 'photo.jpg'), 'photo.jpg');
    assert.equal(lib.objectKeyFor('albums/', 'trip/day1/photo.jpg'), 'albums/trip/day1/photo.jpg');
    assert.equal(lib.objectKeyFor('albums/', '/trip\\day1\\photo.jpg'), 'albums/trip/day1/photo.jpg');
});

test('retryDelay backs off exponentially with a cap', () => {
    assert.equal(lib.retryDelay(0), 500);
    assert.equal(lib.retryDelay(1), 1000);
    assert.equal(lib.retryDelay(2), 2000);
    assert.equal(lib.retryDelay(10), 8000);
});

test('apiErrorMessage surfaces the server message', () => {
    assert.equal(lib.apiErrorMessage(404, '{"error":"The specified bucket does not exist"}', 'Upload failed'),
        'The specified bucket does not exist');
    assert.equal(lib.apiErrorMessage(413, '', 'Upload failed'), 'The file is too large for this server');
    assert.equal(lib.apiErrorMessage(500, '<html>', 'Upload failed'), 'Upload failed (HTTP 500)');
    assert.equal(lib.apiErrorMessage(0, '', 'Upload failed'), 'Upload failed: the server could not be reached');
});

test('formatSize handles missing and edge values', () => {
    assert.equal(lib.formatSize(0), '0 B');
    assert.equal(lib.formatSize(1536), '1.5 KB');
    assert.equal(lib.formatSize(undefined), '—');
    assert.equal(lib.formatSize(-1), '—');
});

test('escapeHtml escapes markup and attribute quotes', () => {
    assert.equal(lib.escapeHtml(`<a href="x" title='y'>&</a>`),
        '&lt;a href=&quot;x&quot; title=&#39;y&#39;&gt;&amp;&lt;/a&gt;');
});

test('previewKind prefers the stored content type, then the extension', () => {
    assert.equal(lib.previewKind('image/png', 'noext'), 'image');
    assert.equal(lib.previewKind('application/octet-stream', 'clip.mp4'), 'video');
    assert.equal(lib.previewKind('application/json', 'data.bin'), 'text');
    assert.equal(lib.previewKind('', 'archive.zip'), 'none');
});
