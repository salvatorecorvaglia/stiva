const { defineConfig } = require('@playwright/test');
module.exports = defineConfig({
    testDir: './internal/console/browser',
    testMatch: '*.spec.cjs',
    fullyParallel: true,
    workers: 2,
    use: { baseURL: 'http://127.0.0.1:19381', headless: true, trace: 'retain-on-failure' },
    webServer: {
        command: 'node internal/console/browser/server.cjs',
        url: 'http://127.0.0.1:19381',
        reuseExistingServer: false,
    },
});
