const { test, expect } = require('@playwright/test');

const item = (key, extra = {}) => ({
    key,
    size: 7,
    lastModified: '2026-10-01T00:00:00Z',
    contentType: 'text/plain',
    ...extra,
});
const listing = (items) => ({ items, isTruncated: false });
const gate = () => {
    let release;
    const wait = new Promise((resolve) => {
        release = resolve;
    });
    return { wait, release };
};

async function setup(page, handler = () => null) {
    const errors = [];
    page.on('pageerror', (error) => errors.push(error.message));
    await page.addInitScript(() => localStorage.setItem('stiva_token', 'fixture'));
    await page.route('**/api/**', async (route) => {
        if (await handler(route)) return;
        const url = new URL(route.request().url());
        let data = {};
        if (url.pathname === '/api/buckets')
            data = ['alpha', 'beta'].map((name) => ({
                name,
                creationDate: '2026-10-01',
                objectCount: 2,
            }));
        else if (url.pathname === '/api/config') data = { s3Port: 9000 };
        else if (url.pathname === '/api/storage/usage') data = { usage: null, refreshing: true };
        else if (url.pathname.endsWith('/objects'))
            data = listing([item('one.txt'), item('two.txt')]);
        await route.fulfill({ json: data });
    });
    await page.goto('/');
    await expect(page.locator('.bucket-card')).toHaveCount(2);
    return errors;
}
async function openAlpha(page) {
    await page.locator('.bucket-card').filter({ hasText: 'alpha' }).click();
    await expect(page.locator('.preview-link').first()).toBeVisible();
}

test('Escape cancels deletion; the next confirmation deletes only its own key', async ({
    page,
}) => {
    const deletes = [];
    const errors = await setup(page, async (route) => {
        if (route.request().method() !== 'DELETE') return false;
        deletes.push(new URL(route.request().url()).searchParams.get('key'));
        await route.fulfill({ json: {} });
        return true;
    });
    const card = page.locator('.bucket-card').filter({ hasText: 'alpha' });
    await card.focus();
    await page.keyboard.press('Enter');
    await expect(page.locator('.preview-link').first()).toBeVisible();
    await page.locator('.delete-obj-btn').nth(0).click();
    await page.keyboard.press('Escape');
    await expect(page.locator('#delete-modal')).toBeHidden();
    await page.locator('.delete-obj-btn').nth(1).click();
    await page.locator('#confirm-delete-btn').click();
    await expect.poll(() => deletes).toEqual(['two.txt']);
    expect(errors).toEqual([]);
});

test('late listings cannot replace a newer search result', async ({ page }) => {
    const slow = gate();
    const arrived = gate();
    await setup(page, async (route) => {
        const url = new URL(route.request().url());
        if (!url.pathname.endsWith('/objects')) return false;
        const prefix = url.searchParams.get('searchPrefix');
        if (prefix === 'slow') {
            arrived.release();
            await slow.wait;
        }
        await route.fulfill({ json: listing([item(prefix ? `${prefix}.txt` : 'one.txt')]) });
        return true;
    });
    await openAlpha(page);
    await page.locator('#search-input').fill('slow');
    await arrived.wait;
    await page.locator('#search-input').fill('fast');
    await expect(page.locator('.preview-link')).toHaveText('fast.txt');
    slow.release();
    await page.waitForTimeout(150);
    await expect(page.locator('.preview-link')).toHaveText('fast.txt');
});

test('closing a pending preview prevents late content and blob URLs', async ({ page }) => {
    const slow = gate();
    const arrived = gate();
    await setup(page, async (route) => {
        if (!new URL(route.request().url()).pathname.endsWith('/objects/download')) return false;
        arrived.release();
        await slow.wait;
        await route.fulfill({ body: 'preview', contentType: 'text/plain' }).catch(() => {});
        return true;
    });
    await page.evaluate(() => {
        window.createdBlobs = 0;
        const create = URL.createObjectURL;
        URL.createObjectURL = (...args) => {
            window.createdBlobs++;
            return create(...args);
        };
    });
    await openAlpha(page);
    await page.locator('.preview-link').first().click();
    await arrived.wait;
    await page.keyboard.press('Escape');
    slow.release();
    await expect(page.locator('#preview-modal')).toBeHidden();
    await page.waitForTimeout(100);
    expect(await page.evaluate(() => window.createdBlobs)).toBe(0);
});

test('unsupported preview names render literally', async ({ page }) => {
    await setup(page, async (route) => {
        const path = new URL(route.request().url()).pathname;
        if (path.endsWith('/objects/download')) {
            await route.fulfill({ body: 'data', contentType: 'application/octet-stream' });
            return true;
        }
        if (!path.endsWith('/objects')) return false;
        await route.fulfill({
            json: listing([item('evil.<img src=x>', { contentType: 'application/octet-stream' })]),
        });
        return true;
    });
    await openAlpha(page);
    await page.locator('.preview-link').click();
    await expect(page.locator('#preview-content-container')).toContainText('<img src=x>');
    await expect(page.locator('#preview-content-container img')).toHaveCount(0);
});

test('folder deletion visits every page using bounded batches', async ({ page }) => {
    let keys = Array.from({ length: 2500 }, (_, i) => `folder/${String(i).padStart(5, '0')}`);
    const batches = [];
    await setup(page, async (route) => {
        const req = route.request();
        const url = new URL(req.url());
        if (url.pathname.endsWith('/objects/delete')) {
            const batch = req.postDataJSON().keys;
            batches.push(batch.length);
            keys = keys.filter((key) => !batch.includes(key));
            await route.fulfill({ json: { deleted: batch, errors: [] } });
            return true;
        }
        if (!url.pathname.endsWith('/objects')) return false;
        if (url.searchParams.get('recursive') !== '1') {
            await route.fulfill({ json: listing([{ key: 'folder/', isPrefix: true }]) });
            return true;
        }
        const token = url.searchParams.get('continuationToken') || '';
        const remaining = keys.filter((key) => key > token);
        const batch = remaining.slice(0, 1000);
        await route.fulfill({
            json: {
                items: batch.map((key) => item(key)),
                isTruncated: remaining.length > batch.length,
                nextContinuationToken: batch.at(-1),
            },
        });
        return true;
    });
    await page.locator('.bucket-card').filter({ hasText: 'alpha' }).click();
    await page.locator('.delete-folder-btn').click();
    await page.locator('#confirm-delete-btn').click();
    await expect.poll(() => keys.length).toBe(0);
    expect(batches).toEqual([1000, 1000, 500]);
});

test('multi-file upload retains its original bucket during navigation', async ({ page }) => {
    const slow = gate();
    const arrived = gate();
    const targets = [];
    await setup(page, async (route) => {
        const url = new URL(route.request().url());
        if (!url.pathname.endsWith('/objects/upload')) return false;
        targets.push(url.pathname);
        if (targets.length === 1) {
            arrived.release();
            await slow.wait;
        }
        await route.fulfill({ json: {} });
        return true;
    });
    await openAlpha(page);
    await page.locator('#file-input').setInputFiles(
        ['first.txt', 'second.txt'].map((name) => ({
            name,
            mimeType: 'text/plain',
            buffer: Buffer.from('payload'),
        })),
    );
    await arrived.wait;
    await page.locator('#breadcrumb button').first().click();
    await page.locator('.bucket-card').filter({ hasText: 'beta' }).click();
    slow.release();
    await expect.poll(() => targets.length).toBe(2);
    expect(targets).toEqual([
        '/api/buckets/alpha/objects/upload',
        '/api/buckets/alpha/objects/upload',
    ]);
});

test('multipart upload retries throttled parts and completion without aborting', async ({
    page,
}) => {
    const attempts = new Map();
    let completions = 0;
    let aborts = 0;
    await setup(page, async (route) => {
        const req = route.request();
        const path = new URL(req.url()).pathname;
        if (path.endsWith('/multipart/initiate')) {
            await route.fulfill({ json: { uploadId: 'fixture' } });
            return true;
        }
        if (path.endsWith('/multipart/upload-part')) {
            const number = Number(req.postData().match(/name="partNumber"\r\n\r\n(\d+)/)[1]);
            const count = (attempts.get(number) || 0) + 1;
            attempts.set(number, count);
            if (number === 1 && count === 1) {
                await route.fulfill({
                    status: 429,
                    headers: { 'Retry-After': '1' },
                    json: { error: 'too many requests' },
                });
            } else await route.fulfill({ json: { partNumber: number, etag: `etag-${number}` } });
            return true;
        }
        if (path.endsWith('/multipart/complete')) {
            completions++;
            await route.fulfill(
                completions === 1
                    ? {
                          status: 429,
                          headers: { 'Retry-After': '1' },
                          json: { error: 'too many requests' },
                      }
                    : { json: {} },
            );
            return true;
        }
        if (path.endsWith('/multipart/abort')) {
            aborts++;
            await route.fulfill({ json: {} });
            return true;
        }
        return false;
    });
    await openAlpha(page);
    await page.locator('#file-input').setInputFiles({
        name: 'large.bin',
        mimeType: 'application/octet-stream',
        buffer: Buffer.alloc(11 * 1024 * 1024),
    });
    await expect(page.locator('#toast-container')).toContainText('uploaded successfully', {
        timeout: 15000,
    });
    expect(attempts.get(1)).toBe(2);
    expect(completions).toBe(2);
    expect(aborts).toBe(0);
});

test('cancel interrupts a long multipart retry wait', async ({ page }) => {
    const denied = gate();
    let attempts = 0;
    let aborts = 0;
    await setup(page, async (route) => {
        const path = new URL(route.request().url()).pathname;
        if (path.endsWith('/multipart/initiate')) {
            await route.fulfill({ json: { uploadId: 'fixture' } });
            return true;
        }
        if (path.endsWith('/multipart/upload-part')) {
            attempts++;
            await route.fulfill({
                status: 429,
                headers: { 'Retry-After': '60' },
                json: { error: 'too many requests' },
            });
            denied.release();
            return true;
        }
        if (path.endsWith('/multipart/abort')) {
            aborts++;
            await route.fulfill({ json: {} });
            return true;
        }
        return false;
    });
    await openAlpha(page);
    await page.locator('#file-input').setInputFiles({
        name: 'large.bin',
        mimeType: 'application/octet-stream',
        buffer: Buffer.alloc(11 * 1024 * 1024),
    });
    await denied.wait;
    await page.locator('#upload-cancel-btn').click();
    await expect.poll(() => aborts, { timeout: 3000 }).toBe(1);
    await expect(page.locator('#upload-progress')).toBeHidden();
    expect(attempts).toBeLessThanOrEqual(3);
});

test('shared SVG icons resolve into visible geometry', async ({ page }) => {
    await setup(page);
    const icon = page.locator('.bucket-card-icon svg').first();
    await expect(icon.locator('use')).toHaveAttribute('href', 'icons.svg#folder');
    await expect.poll(() => icon.evaluate((element) => element.getBBox().width)).toBeGreaterThan(0);
    await openAlpha(page);
    const trash = page.locator('.delete-obj-btn svg').first();
    await expect
        .poll(() => trash.evaluate((element) => element.getBBox().height))
        .toBeGreaterThan(0);
    await expect(trash).toHaveAttribute('aria-hidden', 'true');
});
