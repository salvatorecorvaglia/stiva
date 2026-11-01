// ================================================================
// Stiva Console — Application Logic
// ================================================================

(() => {
    const {
        apiErrorMessage,
        icon,
        escapeHtml,
        formatSize,
        objectKeyFor,
        previewKind,
        validateFolderName,
    } = window.StivaLib;

    const { UploadCancelled, sleep } = window.StivaTransfers;
    const { uploadSmallFile, uploadLargeFile } = window.StivaTransfers.createClient({
        api,
        getToken: () => token,
        logout,
        errorFrom,
        setUploadProgress,
    });

    const browserState = window.StivaBrowser.createBrowserState();

    // ---- State ----
    let usageTimer = null;
    let usageGeneration = 0;
    let token = localStorage.getItem('stiva_token') || '';
    let shareTargetKey = '';
    let currentPreviewBlobURL = '';

    // Zero-byte marker object used to make an empty "folder" visible. It is an
    // implementation detail and must never be listed as a file.
    const FOLDER_PLACEHOLDER = '.stiva_folder';

    // ---- DOM Refs ----
    const $ = (sel) => document.querySelector(sel);
    const $$ = (sel) => document.querySelectorAll(sel);

    const loginScreen = $('#login-screen');
    const dashboardScreen = $('#dashboard-screen');
    const loginForm = $('#login-form');
    const loginError = $('#login-error');
    const loginBtn = $('#login-btn');
    const logoutBtn = $('#logout-btn');

    const bucketsView = $('#buckets-view');
    const objectsView = $('#objects-view');
    const bucketsGrid = $('#buckets-grid');
    const bucketsEmpty = $('#buckets-empty');
    const bucketCount = $('#bucket-count');

    const objectsTbody = $('#objects-tbody');
    const objectsEmpty = $('#objects-empty');
    const objectsTableContainer = $('#objects-table-container');
    const objectCount = $('#object-count');
    const breadcrumb = $('#breadcrumb');
    const bucketPublicToggle = $('#bucket-public-toggle');
    const bucketPublicBadge = $('#bucket-public-badge');

    const createBucketBtn = $('#create-bucket-btn');
    const uploadBtn = $('#upload-btn');
    const createFolderBtn = $('#create-folder-btn');
    const fileInput = $('#file-input');

    const dropZone = $('#drop-zone');
    const uploadProgress = $('#upload-progress');
    const uploadProgressFill = $('#upload-progress-fill');
    const uploadPercent = $('#upload-percent');
    const uploadFileName = $('#upload-file-name');
    const uploadCancelBtn = $('#upload-cancel-btn');
    const uploadFolderBtn = $('#upload-folder-btn');
    const folderInput = $('#folder-input');

    const searchInput = $('#search-input');
    const paginationContainer = $('#pagination-container');
    const prevPageBtn = $('#prev-page-btn');
    const nextPageBtn = $('#next-page-btn');
    const pageIndicator = $('#page-indicator');

    const _shareModal = $('#share-modal');
    const shareExpires = $('#share-expires');
    const shareUrlInput = $('#share-url-input');
    const copyShareUrlBtn = $('#copy-share-url-btn');

    const _previewModal = $('#preview-modal');
    const previewTitle = $('#preview-title');
    const previewContentContainer = $('#preview-content-container');
    const previewFileSize = $('#preview-file-size');

    const confirmPremium = window.StivaDialogs.createConfirmation({
        document,
        openModal,
        closeModal,
    });

    // ---- API Helpers ----

    async function api(method, path, body = null, isFormData = false, signal = undefined) {
        const opts = {
            method,
            signal,
            headers: {},
        };

        if (token) {
            opts.headers.Authorization = `Bearer ${token}`;
        }

        if (body && !isFormData) {
            opts.headers['Content-Type'] = 'application/json';
            opts.body = JSON.stringify(body);
        } else if (body && isFormData) {
            opts.body = body;
        }

        let resp;
        for (let attempt = 0; ; attempt++) {
            resp = await fetch(path, opts);
            if (resp.status !== 429 || attempt >= 5) break;
            const wait = Math.max(1000, Number(resp.headers.get('Retry-After')) * 1000 || 1000);
            await resp.body?.cancel();
            await sleep(Math.min(wait, 60000), signal);
            signal?.throwIfAborted();
        }
        // A 401 from /api/login means wrong credentials, not an expired
        // session. Treating it as expiry threw here, and the login form then
        // reported "Connection failed. Is the server running?" for a typo'd
        // password.
        if (resp.status === 401 && path !== '/api/login') {
            logout();
            throw new Error('Session expired');
        }
        return resp;
    }

    // errorFrom reads the server's explanation out of a failed response.
    async function errorFrom(resp, fallback) {
        const text = await resp.text().catch(() => '');
        return apiErrorMessage(resp.status, text, fallback);
    }

    // Wraps the try/await/if-ok/else-toast pattern repeated across nearly
    // every mutating console action (create bucket, delete object, toggle
    // public access, ...), which previously diverged slightly at each call
    // site in how errors were surfaced. Returns true on success.
    async function apiAction(
        method,
        path,
        { body = null, isFormData = false, successMsg, errorMsg, onSuccess, onError } = {},
    ) {
        try {
            const resp = await api(method, path, body, isFormData);
            if (resp.ok) {
                if (successMsg) showToast(successMsg, 'success');
                if (onSuccess) onSuccess();
                return true;
            }
            const data = await resp.json().catch(() => ({}));
            showToast(data.error || errorMsg || 'Request failed', 'error');
            if (onError) onError();
            return false;
        } catch (err) {
            showToast(errorMsg || err.message || 'Request failed', 'error');
            if (onError) onError();
            return false;
        }
    }

    // ---- Toast ----

    function showToast(message, type = 'info') {
        const container = $('#toast-container');
        const toast = document.createElement('div');
        toast.className = `toast ${type}`;
        // Announce to assistive tech: toasts are the only feedback for most
        // actions, and were previously silent to screen readers.
        toast.setAttribute('role', type === 'error' ? 'alert' : 'status');

        const icons = {
            success: '✓',
            error: '✕',
            info: 'ℹ',
        };

        toast.innerHTML = `<span>${icons[type] || ''}</span> ${escapeHtml(message)}`;
        container.appendChild(toast);

        setTimeout(() => {
            toast.style.animation = 'toastOut 0.3s ease forwards';
            setTimeout(() => toast.remove(), 300);
        }, 3500);
    }

    // ---- Auth ----

    loginForm.addEventListener('submit', async (e) => {
        e.preventDefault();
        const accessKey = $('#access-key').value.trim();
        const secretKey = $('#secret-key').value.trim();

        loginBtn.querySelector('.btn-text').style.display = 'none';
        loginBtn.querySelector('.btn-loader').style.display = 'inline-flex';
        loginError.style.display = 'none';

        try {
            const resp = await api('POST', '/api/login', { accessKey, secretKey });
            const data = await resp.json();

            if (resp.ok) {
                token = data.token;
                localStorage.setItem('stiva_token', token);
                showDashboard();
            } else {
                loginError.textContent = data.error || 'Invalid credentials';
                loginError.style.display = 'block';
            }
        } catch (_err) {
            loginError.textContent = 'Connection failed. Is the server running?';
            loginError.style.display = 'block';
        } finally {
            loginBtn.querySelector('.btn-text').style.display = 'inline';
            loginBtn.querySelector('.btn-loader').style.display = 'none';
        }
    });

    logoutBtn.addEventListener('click', logout);

    if (bucketPublicToggle) {
        bucketPublicToggle.addEventListener('change', async () => {
            if (!browserState.state.bucket) return;
            const publicVal = bucketPublicToggle.checked;
            // Opening a bucket to anonymous reads is the one irreversible-in-
            // effect setting here (anything read meanwhile is out), so it is
            // confirmed rather than applied on a single click.
            if (publicVal) {
                const ok = await confirmPremium(
                    `Make "${browserState.state.bucket}" public? Anyone who knows or guesses an object's URL will be able to download it without credentials.`,
                    {
                        title: 'Make Bucket Public',
                        confirmLabel: 'Make Public',
                        warning: 'Objects in this bucket will be readable by anyone.',
                    },
                );
                if (!ok) {
                    bucketPublicToggle.checked = false;
                    return;
                }
            }
            await apiAction(
                'POST',
                `/api/buckets/${browserState.state.bucket}/public?public=${publicVal}`,
                {
                    successMsg: 'Bucket public access updated',
                    errorMsg: 'Failed to update public access',
                    onSuccess: () => {
                        if (bucketPublicBadge) {
                            bucketPublicBadge.style.display = publicVal ? 'inline-flex' : 'none';
                        }
                    },
                    onError: () => {
                        bucketPublicToggle.checked = !publicVal;
                    },
                },
            );
        });
    }

    function logout() {
        browserState.invalidate();
        clearTimeout(usageTimer);
        usageGeneration++;
        token = '';
        localStorage.removeItem('stiva_token');
        dashboardScreen.classList.remove('active');
        loginScreen.classList.add('active');
        loginForm.reset();
    }

    // ---- Dashboard ----

    function showDashboard() {
        loginScreen.classList.remove('active');
        dashboardScreen.classList.add('active');
        showBucketsView();
        loadServerConfig();
        startUsagePolling();
    }

    // Populates the top-bar "S3 API: Port ####" badge from the server's
    // actual STIVA_S3_PORT instead of a hardcoded "9000" that went stale the
    // moment an operator changed the port.
    async function loadServerConfig() {
        try {
            const resp = await api('GET', '/api/config');
            if (!resp.ok) return;
            const data = await resp.json();
            const badge = document.querySelector('.server-badge');
            if (badge && data.s3Port) {
                badge.innerHTML = `<span class="status-dot"></span>S3 API: Port ${data.s3Port}`;
            }
        } catch (_err) {
            // Non-critical: the badge just keeps its static fallback text.
        }
    }

    // Poll cached snapshots only while signed in. One in-flight request per
    // session avoids overlapping refreshes and late responses after logout.
    function startUsagePolling() {
        clearTimeout(usageTimer);
        const generation = ++usageGeneration;
        const poll = async () => {
            let delay = 30000;
            try {
                const response = await api('GET', '/api/storage/usage');
                const data = await response.json();
                if (generation !== usageGeneration || !token) return;
                if (!response.ok) throw new Error(data.error || 'Storage summary unavailable');
                if (!data.usage) {
                    $('#usage-status').textContent = 'Calculating storage usage…';
                    delay = 5000;
                } else {
                    renderUsage(data);
                }
            } catch (error) {
                if (generation !== usageGeneration || !token) return;
                $('#usage-status').textContent =
                    `${error.message} Retrying automatically; any displayed figures are from the last snapshot.`;
            } finally {
                if (generation === usageGeneration && token) usageTimer = setTimeout(poll, delay);
            }
        };
        // Do not carry another session's snapshot into the initial loading state.
        for (const selector of [
            '#usage-bytes',
            '#usage-buckets',
            '#usage-objects',
            '#usage-versions',
            '#usage-free',
        ])
            $(selector).textContent = '—';
        $('#usage-breakdown').textContent = 'Current and previous versions';
        $('#usage-markers').textContent = 'Delete markers counted separately';
        $('#usage-disk-total').textContent = 'Storage volume';
        $('#usage-status').textContent = 'Calculating storage usage…';
        $('#bucket-usage-body').innerHTML =
            '<tr><td colspan="7">Calculating storage usage…</td></tr>';
        poll();
    }

    function renderUsage(snapshot) {
        const usage = snapshot.usage;
        const count = (value) => Number(value).toLocaleString();
        $('#usage-bytes').textContent = formatSize(usage.logicalBytes);
        $('#usage-breakdown').textContent =
            `${formatSize(usage.currentBytes)} current • ${formatSize(usage.previousBytes)} previous`;
        $('#usage-buckets').textContent = count(usage.buckets.length);
        $('#usage-objects').textContent = count(usage.objects);
        $('#usage-versions').textContent = count(usage.versions);
        $('#usage-markers').textContent = `${count(usage.deleteMarkers)} delete markers`;
        $('#usage-free').textContent = formatSize(usage.diskFreeBytes);
        $('#usage-disk-total').textContent =
            `of ${formatSize(usage.diskTotalBytes)} on the storage volume`;
        const updated = new Date(snapshot.updatedAt).toLocaleString();
        $('#usage-status').textContent =
            `${snapshot.error ? `${snapshot.error} ` : ''}Last calculated: ${updated}. Updates every minute.${snapshot.refreshing ? ' Refresh in progress…' : ''}`;
        const body = $('#bucket-usage-body');
        body.replaceChildren();
        if (!usage.buckets.length) {
            body.innerHTML = '<tr><td colspan="7">No buckets in this snapshot.</td></tr>';
        }
        for (const bucket of usage.buckets) {
            const row = document.createElement('tr');
            const nameCell = document.createElement('td');
            const link = document.createElement('button');
            link.type = 'button';
            link.className = 'btn btn-ghost btn-sm';
            link.textContent = bucket.name;
            link.addEventListener('click', () => openBucket(bucket.name));
            nameCell.append(link);
            row.append(nameCell);
            for (const value of [
                count(bucket.objects),
                count(bucket.versions),
                count(bucket.deleteMarkers),
                formatSize(bucket.currentBytes),
                formatSize(bucket.previousBytes),
                formatSize(bucket.logicalBytes),
            ]) {
                const cell = document.createElement('td');
                cell.textContent = value;
                row.append(cell);
            }
            body.append(row);
        }
    }

    // ---- Buckets ----

    async function showBucketsView() {
        browserState.invalidate();
        bucketsView.classList.add('active');
        objectsView.classList.remove('active');
        browserState.state.bucket = '';
        browserState.state.prefix = '';
        await loadBuckets();
    }

    async function loadBuckets() {
        bucketsGrid.innerHTML = '';
        bucketsEmpty.style.display = 'none';
        bucketCount.textContent = 'Loading buckets...';

        // Render 3 skeleton cards while loading
        for (let i = 0; i < 3; i++) {
            bucketsGrid.appendChild(createBucketSkeletonCard());
        }

        try {
            const resp = await api('GET', '/api/buckets');
            if (!resp.ok) throw new Error(await errorFrom(resp, 'Failed to load buckets'));
            const buckets = await resp.json();

            bucketsGrid.innerHTML = '';
            if (buckets.length === 0) {
                bucketsEmpty.style.display = 'block';
                bucketCount.textContent = 'No buckets';
            } else {
                bucketsEmpty.style.display = 'none';
                bucketCount.textContent = `${buckets.length} bucket${buckets.length !== 1 ? 's' : ''}`;
                buckets.forEach((b) => {
                    bucketsGrid.appendChild(createBucketCard(b));
                });
            }
        } catch (err) {
            bucketsGrid.innerHTML = '';
            bucketCount.textContent = 'Failed to load buckets';
            showToast(err.message || 'Failed to load buckets', 'error');
        }
    }

    function createBucketCard(bucket) {
        const card = document.createElement('div');
        card.className = 'bucket-card';
        card.tabIndex = 0;
        card.setAttribute('aria-label', `Open bucket ${bucket.name}`);
        card.addEventListener('keydown', (event) => {
            if (event.target === card && (event.key === 'Enter' || event.key === ' ')) {
                event.preventDefault();
                openBucket(bucket.name);
            }
        });

        const created = new Date(bucket.creationDate);
        const dateStr = created.toLocaleDateString('en-US', {
            year: 'numeric',
            month: 'short',
            day: 'numeric',
        });

        card.innerHTML = `
            <div class="bucket-card-header">
                <div class="bucket-card-icon">
                    ${icon('folder', 20)}
                </div>
                <button class="btn-icon btn-danger bucket-delete-btn" title="Delete bucket" aria-label="Delete bucket ${escapeHtml(bucket.name)}" data-bucket="${escapeHtml(bucket.name)}">
                    ${icon('trash', 16)}
                </button>
            </div>
            <div class="bucket-card-name">${escapeHtml(bucket.name)}${bucket.isPublic ? ' <span class="public-badge">Public</span>' : ''}</div>
            <div class="bucket-card-meta">
                <span>
                    ${icon('calendar', 14)}
                    ${dateStr}
                </span>
                <span>
                    ${icon('file', 14)}
                    ${bucket.objectCount} object${bucket.objectCount !== 1 ? 's' : ''}
                </span>
            </div>
        `;

        // Click to browse bucket
        card.addEventListener('click', (e) => {
            if (e.target.closest('.bucket-delete-btn')) return;
            openBucket(bucket.name);
        });

        // Delete button
        const deleteBtn = card.querySelector('.bucket-delete-btn');
        deleteBtn.addEventListener('click', async (e) => {
            e.stopPropagation();
            const ok = await confirmPremium(`Delete bucket "${bucket.name}"? It must be empty.`);
            if (!ok) return;
            await apiAction('DELETE', `/api/buckets/${bucket.name}`, {
                successMsg: `Bucket "${bucket.name}" deleted`,
                errorMsg: 'Failed to delete bucket',
                onSuccess: loadBuckets,
            });
        });

        return card;
    }

    function createBucketSkeletonCard() {
        const card = document.createElement('div');
        card.className = 'bucket-card skeleton-card';
        card.innerHTML = `
            <div class="bucket-card-header">
                <div class="skeleton skeleton-icon"></div>
            </div>
            <div class="bucket-card-name skeleton-gap-lg">
                <div class="skeleton skeleton-title"></div>
            </div>
            <div class="bucket-card-meta skeleton-gap-sm">
                <div class="skeleton skeleton-meta skeleton-w-80"></div>
                <div class="skeleton skeleton-meta skeleton-w-70"></div>
            </div>
        `;
        return card;
    }

    // ---- Create Bucket Modal ----

    createBucketBtn.addEventListener('click', () => {
        openModal('create-bucket-modal');
        $('#new-bucket-name').value = '';
        $('#new-bucket-name').focus();
    });

    $('#confirm-create-bucket').addEventListener('click', async () => {
        const nameInput = $('#new-bucket-name');
        const name = nameInput.value.trim();
        if (!name) return;

        // The input's `pattern` attribute only triggers HTML5 validation on
        // form submission — this isn't inside a <form>, so it never fired
        // and invalid names always round-tripped to the server before the
        // user saw any feedback. reportValidity() runs the same check and
        // shows the browser's native validation message.
        if (!nameInput.checkValidity()) {
            nameInput.reportValidity();
            return;
        }

        await apiAction('POST', '/api/buckets', {
            body: { name },
            successMsg: `Bucket "${name}" created`,
            errorMsg: 'Failed to create bucket',
            onSuccess: () => {
                closeModal('create-bucket-modal');
                loadBuckets();
            },
        });
    });

    // Keep complete server rules so editing here preserves actions configured by S3 clients.
    let lifecycleBucket = '';
    let lifecycleRules = [];
    let lifecycleBusy = false;
    let lifecycleGeneration = 0;

    function renderLifecycleRules() {
        const container = $('#lifecycle-rules');
        container.replaceChildren();
        if (!lifecycleRules.length) container.textContent = 'No expiration rules configured.';
        lifecycleRules.forEach((rule, index) => {
            const row = document.createElement('div');
            row.className = 'form-group';
            const prefix = rule.prefix || rule.filter?.and?.prefix || rule.filter?.prefix || '';
            const description = document.createElement('p');
            const actions = [];
            if (rule.expiration?.days) actions.push(`objects: ${rule.expiration.days} days`);
            if (rule.noncurrentVersionExpiration)
                actions.push(
                    `older versions: ${rule.noncurrentVersionExpiration.noncurrentDays} days`,
                );
            if (rule.abortIncompleteMultipartUpload)
                actions.push(
                    `incomplete uploads: ${rule.abortIncompleteMultipartUpload.daysAfterInitiation} days`,
                );
            description.textContent = `${rule.id || 'Unnamed rule'} — ${rule.status} — ${prefix || 'All objects'} — ${actions.join(', ') || 'No expiration action'}`;
            const toggle = document.createElement('button');
            toggle.type = 'button';
            toggle.className = 'btn btn-secondary btn-sm';
            toggle.textContent = rule.status.toLowerCase() === 'enabled' ? 'Disable' : 'Enable';
            toggle.disabled = lifecycleBusy;
            toggle.addEventListener('click', () => {
                const rules = structuredClone(lifecycleRules);
                rules[index].status =
                    rule.status.toLowerCase() === 'enabled' ? 'Disabled' : 'Enabled';
                saveLifecycleRules(rules);
            });
            const remove = document.createElement('button');
            remove.type = 'button';
            remove.className = 'btn btn-danger btn-sm';
            remove.textContent = 'Remove rule';
            remove.disabled = lifecycleBusy;
            remove.addEventListener('click', () =>
                saveLifecycleRules(lifecycleRules.filter((_, i) => i !== index)),
            );
            row.append(description, toggle, remove);
            container.append(row);
        });
        $('#lifecycle-add').disabled = lifecycleBusy;
    }

    async function saveLifecycleRules(rules) {
        if (lifecycleBusy) return;
        lifecycleBusy = true;
        renderLifecycleRules();
        $('#lifecycle-error').textContent = '';
        try {
            const response = await api(
                rules.length ? 'POST' : 'DELETE',
                `/api/buckets/${encodeURIComponent(lifecycleBucket)}/lifecycle`,
                rules.length ? { rules } : null,
            );
            if (!response.ok)
                throw new Error(await errorFrom(response, 'Failed to save expiration rules'));
            lifecycleRules = rules;
            showToast('Expiration rules updated', 'success');
        } catch (error) {
            $('#lifecycle-error').textContent = error.message;
        } finally {
            lifecycleBusy = false;
            renderLifecycleRules();
        }
    }

    $('#lifecycle-btn').addEventListener('click', async () => {
        if (lifecycleBusy) return;
        const generation = ++lifecycleGeneration;
        lifecycleBucket = browserState.state.bucket;
        lifecycleRules = [];
        $('#lifecycle-bucket').textContent = `Bucket: ${lifecycleBucket}`;
        $('#lifecycle-error').textContent = '';
        $('#lifecycle-prefix').value = '';
        $('#lifecycle-days').value = '30';
        $('#lifecycle-rules').textContent = 'Loading rules…';
        $('#lifecycle-add').disabled = true;
        openModal('lifecycle-modal');
        try {
            const response = await api(
                'GET',
                `/api/buckets/${encodeURIComponent(lifecycleBucket)}/lifecycle`,
            );
            const data = await response.json();
            if (generation !== lifecycleGeneration) return;
            if (
                !response.ok &&
                !(
                    response.status === 404 &&
                    data.error === 'The lifecycle configuration does not exist'
                )
            ) {
                throw new Error(data.error || 'Failed to load expiration rules');
            }
            lifecycleRules = data.rules || [];
            renderLifecycleRules();
        } catch (error) {
            if (generation !== lifecycleGeneration) return;
            $('#lifecycle-rules').textContent = '';
            $('#lifecycle-error').textContent = error.message;
        }
    });

    $('#lifecycle-form').addEventListener('submit', (event) => {
        event.preventDefault();
        if (lifecycleBusy || $('#lifecycle-add').disabled) return;
        const days = Number($('#lifecycle-days').value);
        if (!Number.isInteger(days) || days < 1 || days > 2147483647) return;
        saveLifecycleRules([
            ...lifecycleRules,
            {
                id: `console-${Date.now()}-${Math.random().toString(36).slice(2)}`,
                status: 'Disabled',
                filter: { prefix: $('#lifecycle-prefix').value },
                expiration: { days },
            },
        ]);
    });

    let versionsBucket = '';
    let versioningStatus = '';
    let versionsGeneration = 0;
    let versionsBusy = false;
    let versionsNext = null;
    let versionsPrefix = '';

    function versioningControls(ready) {
        $('#versioning-toggle').disabled = !ready;
        $('#versions-search').disabled = !ready;
        $('#versions-next').disabled = !ready || !versionsNext;
    }

    function renderVersions(items) {
        const list = $('#versions-list');
        list.replaceChildren();
        if (!items.length) list.textContent = 'No versions match this prefix.';
        for (const item of items) {
            const row = document.createElement('div');
            row.className = 'version-row';
            const name = document.createElement('strong');
            name.textContent = item.key;
            const detail = document.createElement('p');
            detail.textContent = `${item.versionId || 'null'} • ${item.lastModified} • ${item.isDeleteMarker ? 'Delete marker' : formatSize(item.size)}${item.isLatest ? ' • Latest' : ''}`;
            row.append(name, detail);
            if (!item.isDeleteMarker) {
                const download = document.createElement('button');
                download.type = 'button';
                download.className = 'btn btn-secondary btn-sm';
                download.textContent = 'Download Version';
                const bucket = versionsBucket;
                download.addEventListener('click', () =>
                    downloadObject(item.key, item.versionId || 'null', bucket),
                );
                row.append(download);
            }
            list.append(row);
        }
    }

    async function loadVersions(next = false) {
        const generation = ++versionsGeneration;
        versionsBusy = true;
        versioningControls(false);
        $('#versioning-error').textContent = '';
        $('#versions-list').textContent = 'Loading versions…';
        const params = new URLSearchParams({ prefix: versionsPrefix, maxKeys: '50' });
        if (next && versionsNext) {
            params.set('keyMarker', versionsNext.key);
            params.set('versionIdMarker', versionsNext.version);
        }
        versionsNext = null;
        try {
            const response = await api(
                'GET',
                `/api/buckets/${encodeURIComponent(versionsBucket)}/versions?${params}`,
            );
            if (!response.ok) throw new Error(await errorFrom(response, 'Failed to load versions'));
            const data = await response.json();
            if (generation !== versionsGeneration) return;
            renderVersions(data.items);
            if (data.isTruncated)
                versionsNext = { key: data.nextKeyMarker, version: data.nextVersionIdMarker };
        } catch (error) {
            if (generation !== versionsGeneration) return;
            $('#versions-list').textContent = '';
            $('#versioning-error').textContent = error.message;
        } finally {
            if (generation === versionsGeneration) {
                versionsBusy = false;
                versioningControls(true);
            }
        }
    }

    $('#versioning-btn').addEventListener('click', async () => {
        if (versionsBusy) return;
        versionsBucket = browserState.state.bucket;
        versionsPrefix = browserState.state.prefix;
        versionsNext = null;
        versioningStatus = '';
        const generation = ++versionsGeneration;
        versionsBusy = true;
        versioningControls(false);
        $('#versions-prefix').value = versionsPrefix;
        $('#versioning-bucket').textContent = `Bucket: ${versionsBucket}`;
        $('#versioning-status').textContent = 'Loading status…';
        $('#versioning-error').textContent = '';
        $('#versions-list').replaceChildren();
        openModal('versioning-modal');
        try {
            const response = await api(
                'GET',
                `/api/buckets/${encodeURIComponent(versionsBucket)}/versioning`,
            );
            if (!response.ok)
                throw new Error(await errorFrom(response, 'Failed to load versioning'));
            const data = await response.json();
            if (generation !== versionsGeneration) return;
            versioningStatus = data.status;
            updateVersioningStatus();
            await loadVersions();
        } catch (error) {
            $('#versioning-status').textContent = 'Status unavailable';
            $('#versioning-error').textContent = error.message;
        } finally {
            versionsBusy = false;
        }
    });

    function updateVersioningStatus() {
        $('#versioning-status').textContent = `Versioning: ${versioningStatus || 'Disabled'}`;
        $('#versioning-toggle').textContent =
            versioningStatus === 'Enabled' ? 'Suspend Versioning' : 'Enable Versioning';
    }

    $('#versioning-toggle').addEventListener('click', async () => {
        if (versionsBusy) return;
        versionsBusy = true;
        versioningControls(false);
        $('#versioning-error').textContent = '';
        try {
            const status = versioningStatus === 'Enabled' ? 'Suspended' : 'Enabled';
            const response = await api(
                'POST',
                `/api/buckets/${encodeURIComponent(versionsBucket)}/versioning`,
                { status },
            );
            if (!response.ok)
                throw new Error(await errorFrom(response, 'Failed to update versioning'));
            versioningStatus = status;
            updateVersioningStatus();
            showToast(`Versioning ${status.toLowerCase()}`, 'success');
        } catch (error) {
            $('#versioning-error').textContent = error.message;
        } finally {
            versionsBusy = false;
            versioningControls(true);
        }
    });
    $('#versions-filter').addEventListener('submit', (event) => {
        event.preventDefault();
        if (versionsBusy || $('#versions-search').disabled) return;
        versionsPrefix = $('#versions-prefix').value;
        loadVersions();
    });
    $('#versions-next').addEventListener('click', () => {
        if (!versionsBusy && versionsNext) loadVersions(true);
    });

    // ---- Objects Browser ----

    function openBucket(name) {
        browserState.state.bucket = name;
        browserState.state.prefix = '';
        browserState.state.searchPrefix = '';
        if (searchInput) searchInput.value = '';
        resetPagination();
        bucketsView.classList.remove('active');
        objectsView.classList.add('active');

        // Fetch and show bucket public status
        api('GET', `/api/buckets/${name}/public`)
            .then((r) => r.json())
            .then((data) => {
                if (browserState.state.bucket !== name) return;
                const isPublic = !!data.public;
                if (bucketPublicToggle) bucketPublicToggle.checked = isPublic;
                if (bucketPublicBadge)
                    bucketPublicBadge.style.display = isPublic ? 'inline-flex' : 'none';
            })
            .catch(() => {});

        loadObjects();
    }

    const itemsPerPage = 50;

    async function loadObjects(silent = false) {
        const request = browserState.beginRequest();
        updateBreadcrumb();

        if (!silent) {
            // Render 5 skeleton rows while loading
            objectsEmpty.style.display = 'none';
            objectsTableContainer.style.display = 'block';
            objectsTbody.innerHTML = '';
            for (let i = 0; i < 5; i++) {
                objectsTbody.appendChild(createObjectSkeletonRow());
            }
            paginationContainer.style.display = 'none';
        }

        try {
            const params = new URLSearchParams({
                prefix: request.prefix,
                delimiter: '/',
                maxKeys: itemsPerPage,
            });

            if (browserState.state.searchPrefix) {
                params.set('searchPrefix', request.searchPrefix);
            }

            if (request.continuationToken)
                params.set('continuationToken', request.continuationToken);

            const resp = await api(
                'GET',
                `/api/buckets/${request.bucket}/objects?${params}`,
                null,
                false,
                request.signal,
            );
            if (!resp.ok) throw new Error(await errorFrom(resp, 'Failed to load objects'));
            const data = await resp.json();
            if (!request.isCurrent()) return;
            // Hide the folder placeholder: it used to appear as a stray file
            // inside every folder created from the console.
            browserState.state.items = (data.items || []).filter(
                (item) => item.isPrefix || !item.key.endsWith(FOLDER_PLACEHOLDER),
            );

            renderObjectsTable();

            const hasNext = data.isTruncated;
            if (hasNext) {
                browserState.state.pageTokens[browserState.state.pageIndex + 1] =
                    data.nextContinuationToken;
            }

            if (browserState.state.pageIndex > 0 || hasNext) {
                paginationContainer.style.display = 'flex';
                prevPageBtn.disabled = browserState.state.pageIndex === 0;
                nextPageBtn.disabled = !hasNext;
                pageIndicator.textContent = `Page ${browserState.state.pageIndex + 1}`;
            } else {
                paginationContainer.style.display = 'none';
            }
        } catch (err) {
            if (!silent && request.isCurrent()) {
                objectsTbody.innerHTML = '';
                objectsTableContainer.style.display = 'none';
                objectsEmpty.style.display = 'block';
                showToast(err.message || 'Failed to load objects', 'error');
            }
        }
    }

    function renderObjectsTable() {
        objectsTbody.innerHTML = '';

        if (browserState.state.items.length === 0) {
            objectsEmpty.style.display = 'block';
            objectsTableContainer.style.display = 'none';
            paginationContainer.style.display = 'none';
            objectCount.textContent = browserState.state.searchPrefix
                ? 'No matching items'
                : 'Empty';

            const headers = {
                name: $('#th-name'),
                size: $('#th-size'),
                date: $('#th-date'),
            };
            for (const th of Object.values(headers)) {
                if (th) th.classList.remove('sort-asc', 'sort-desc');
            }
            const selectAllCheckbox = $('#select-all-objects');
            if (selectAllCheckbox) {
                selectAllCheckbox.checked = false;
                selectAllCheckbox.indeterminate = false;
            }
            updateBatchDeleteBtnVisibility();
            return;
        }

        objectsEmpty.style.display = 'none';
        objectsTableContainer.style.display = 'block';
        // Sorting is applied to the loaded page only, not the whole bucket. Say
        // so rather than letting the column arrows imply a global ordering.
        const scopeNote = paginationContainer.style.display !== 'none' ? ' on this page' : '';
        objectCount.textContent = `${browserState.state.items.length} item${browserState.state.items.length !== 1 ? 's' : ''}${scopeNote}`;

        const folders = browserState.state.items.filter((item) => item.isPrefix);
        const files = browserState.state.items.filter((item) => !item.isPrefix);

        folders.sort((a, b) => a.key.localeCompare(b.key));

        files.sort((a, b) => {
            let valA, valB;
            if (browserState.state.sortField === 'name') {
                valA = a.key.toLowerCase();
                valB = b.key.toLowerCase();
            } else if (browserState.state.sortField === 'size') {
                valA = a.size || 0;
                valB = b.size || 0;
            } else if (browserState.state.sortField === 'date') {
                valA = a.lastModified ? new Date(a.lastModified).getTime() : 0;
                valB = b.lastModified ? new Date(b.lastModified).getTime() : 0;
            }

            if (valA < valB) return browserState.state.sortOrder === 'asc' ? -1 : 1;
            if (valA > valB) return browserState.state.sortOrder === 'asc' ? 1 : -1;
            return 0;
        });

        const sortedItems = [...folders, ...files];

        sortedItems.forEach((item) => {
            objectsTbody.appendChild(createObjectRow(item));
        });

        const headers = {
            name: $('#th-name'),
            size: $('#th-size'),
            date: $('#th-date'),
        };

        for (const [field, th] of Object.entries(headers)) {
            if (!th) continue;
            th.classList.remove('sort-asc', 'sort-desc');
            if (field === browserState.state.sortField) {
                th.classList.add(browserState.state.sortOrder === 'asc' ? 'sort-asc' : 'sort-desc');
                th.setAttribute(
                    'aria-sort',
                    browserState.state.sortOrder === 'asc' ? 'ascending' : 'descending',
                );
            } else {
                th.setAttribute('aria-sort', 'none');
            }
        }

        const selectAllCheckbox = $('#select-all-objects');
        if (selectAllCheckbox) {
            const allCheckboxes = Array.from(objectsTbody.querySelectorAll('.object-select'));
            const allChecked = allCheckboxes.length > 0 && allCheckboxes.every((c) => c.checked);
            const anyChecked = allCheckboxes.some((c) => c.checked);
            selectAllCheckbox.checked = allChecked;
            selectAllCheckbox.indeterminate = anyChecked && !allChecked;
        }
        updateBatchDeleteBtnVisibility();
    }

    function updateBatchDeleteBtnVisibility() {
        const batchDeleteBtn = $('#batch-delete-btn');
        if (!batchDeleteBtn) return;

        const checkedBoxes = objectsTbody.querySelectorAll('.object-select:checked');
        const count = checkedBoxes.length;

        if (count > 0) {
            batchDeleteBtn.style.display = 'inline-flex';
            batchDeleteBtn.innerHTML = `
                ${icon('trash-selected', 16)}
                Delete Selected (${count})
            `;
        } else {
            batchDeleteBtn.style.display = 'none';
        }
    }

    function createObjectRow(item) {
        const tr = document.createElement('tr');

        if (item.isPrefix) {
            // Folder
            const displayName = item.key.replace(browserState.state.prefix, '').replace(/\/$/, '');
            tr.innerHTML = `
                <td></td>
                <td>
                    <div class="file-name">
                        <div class="file-icon folder">
                            ${icon('folder', 16)}
                        </div>
                        <span class="file-name-text folder-link" data-prefix="${escapeHtml(item.key)}">${escapeHtml(displayName)}/</span>
                    </div>
                </td>
                <td>—</td>
                <td>—</td>
                <td>
                    <div class="file-actions">
                        <button class="btn-icon btn-danger delete-folder-btn" title="Delete folder" aria-label="Delete folder ${escapeHtml(displayName)}">
                            ${icon('trash', 16)}
                        </button>
                    </div>
                </td>
            `;

            // Folders could not be deleted from the console at all.
            tr.querySelector('.delete-folder-btn').addEventListener('click', () => {
                deleteFolder(item.key, displayName);
            });

            tr.querySelector('.folder-link').addEventListener('click', () => {
                browserState.state.prefix = item.key;
                browserState.state.searchPrefix = '';
                if (searchInput) searchInput.value = '';
                resetPagination();
                loadObjects();
            });
        } else {
            // File
            const displayName = item.key.replace(browserState.state.prefix, '');
            const size = formatSize(item.size);
            const modified = item.lastModified
                ? new Date(item.lastModified).toLocaleString('en-US', {
                      year: 'numeric',
                      month: 'short',
                      day: 'numeric',
                      hour: '2-digit',
                      minute: '2-digit',
                  })
                : '—';

            tr.innerHTML = `
                <td>
                    <input type="checkbox" class="object-select" data-key="${escapeHtml(item.key)}" aria-label="Select ${escapeHtml(displayName)}">
                </td>
                <td>
                    <div class="file-name">
                        <div class="file-icon file">
                            ${icon('file', 16)}
                        </div>
                        <button type="button" class="file-name-text preview-link" data-key="${escapeHtml(item.key)}">${escapeHtml(displayName)}</button>
                    </div>
                </td>
                <td>${size}</td>
                <td>${modified}</td>
                <td>
                    <div class="file-actions">
                        <button class="btn-icon share-btn" title="Share Link" aria-label="Share ${escapeHtml(displayName)}" data-key="${escapeHtml(item.key)}">
                            ${icon('share', 16)}
                        </button>
                        <button class="btn-icon download-btn" title="Download" aria-label="Download ${escapeHtml(displayName)}" data-key="${escapeHtml(item.key)}">
                            ${icon('download', 16)}
                        </button>
                        <button class="btn-icon btn-danger delete-obj-btn" title="Delete" aria-label="Delete ${escapeHtml(displayName)}" data-key="${escapeHtml(item.key)}">
                            ${icon('trash', 16)}
                        </button>
                    </div>
                </td>
            `;

            tr.querySelector('.object-select').addEventListener('change', () => {
                updateBatchDeleteBtnVisibility();
                const allCheckboxes = Array.from(objectsTbody.querySelectorAll('.object-select'));
                const allChecked =
                    allCheckboxes.length > 0 && allCheckboxes.every((c) => c.checked);
                const anyChecked = allCheckboxes.some((c) => c.checked);
                const selectAll = $('#select-all-objects');
                if (selectAll) {
                    selectAll.checked = allChecked;
                    selectAll.indeterminate = anyChecked && !allChecked;
                }
            });

            tr.querySelector('.preview-link').addEventListener('click', () => {
                showPreview(item.key, item.size, item.contentType);
            });

            tr.querySelector('.share-btn').addEventListener('click', () => {
                showShareModal(item.key);
            });

            tr.querySelector('.download-btn').addEventListener('click', () => {
                downloadObject(item.key);
            });

            tr.querySelector('.delete-obj-btn').addEventListener('click', async () => {
                const bucket = browserState.state.bucket;
                const ok = await confirmPremium(`Delete "${displayName}"?`);
                if (!ok) return;
                await apiAction(
                    'DELETE',
                    `/api/buckets/${bucket}/objects?key=${encodeURIComponent(item.key)}`,
                    {
                        successMsg: `"${displayName}" deleted`,
                        errorMsg: 'Failed to delete object',
                        onSuccess: loadObjects,
                    },
                );
            });
        }

        return tr;
    }

    function createObjectSkeletonRow() {
        const tr = document.createElement('tr');
        tr.innerHTML = `
            <td><div class="skeleton skeleton-checkbox"></div></td>
            <td>
                <div class="file-name">
                    <div class="skeleton skeleton-file-icon"></div>
                    <div class="skeleton skeleton-text skeleton-w-140 skeleton-tall"></div>
                </div>
            </td>
            <td><div class="skeleton skeleton-text skeleton-w-60"></div></td>
            <td><div class="skeleton skeleton-text skeleton-w-120"></div></td>
            <td></td>
        `;
        return tr;
    }

    function updateBreadcrumb() {
        breadcrumb.innerHTML = '';

        // Root: Buckets
        const rootBtn = document.createElement('button');
        rootBtn.className = 'breadcrumb-item';
        rootBtn.textContent = 'Buckets';
        rootBtn.addEventListener('click', () => showBucketsView());
        breadcrumb.appendChild(rootBtn);

        addBreadcrumbSep();

        // Bucket name
        if (!browserState.state.prefix) {
            const bucketSpan = document.createElement('span');
            bucketSpan.className = 'breadcrumb-item current';
            bucketSpan.textContent = browserState.state.bucket;
            breadcrumb.appendChild(bucketSpan);
        } else {
            const bucketBtn = document.createElement('button');
            bucketBtn.className = 'breadcrumb-item';
            bucketBtn.textContent = browserState.state.bucket;
            bucketBtn.addEventListener('click', () => {
                browserState.state.prefix = '';
                browserState.state.searchPrefix = '';
                if (searchInput) searchInput.value = '';
                resetPagination();
                loadObjects();
            });
            breadcrumb.appendChild(bucketBtn);

            // Prefix parts
            const parts = browserState.state.prefix.split('/').filter(Boolean);
            parts.forEach((part, i) => {
                addBreadcrumbSep();
                const prefix = parts.slice(0, i + 1).join('/') + '/';
                if (i === parts.length - 1) {
                    const span = document.createElement('span');
                    span.className = 'breadcrumb-item current';
                    span.textContent = part;
                    breadcrumb.appendChild(span);
                } else {
                    const btn = document.createElement('button');
                    btn.className = 'breadcrumb-item';
                    btn.textContent = part;
                    btn.addEventListener('click', () => {
                        browserState.state.prefix = prefix;
                        browserState.state.searchPrefix = '';
                        if (searchInput) searchInput.value = '';
                        resetPagination();
                        loadObjects();
                    });
                    breadcrumb.appendChild(btn);
                }
            });
        }
    }

    function addBreadcrumbSep() {
        const sep = document.createElement('span');
        sep.className = 'breadcrumb-sep';
        sep.textContent = '/';
        breadcrumb.appendChild(sep);
    }

    // ---- File Upload ----

    uploadBtn.addEventListener('click', () => fileInput.click());
    uploadFolderBtn.addEventListener('click', () => folderInput.click());

    // Each file with its path relative to what was chosen: just its name for
    // a single file, "folder/sub/name" for a file inside a chosen folder.
    const filesFromInput = (input) =>
        Array.from(input.files).map((file) => ({
            file,
            relPath: file.webkitRelativePath || file.name,
        }));

    for (const input of [fileInput, folderInput]) {
        input.addEventListener('change', () => {
            if (input.files.length > 0) {
                uploadFiles(filesFromInput(input));
                input.value = '';
            }
        });
    }

    // Drag and drop
    const objectsViewEl = objectsView;
    objectsViewEl.addEventListener('dragover', (e) => {
        e.preventDefault();
        dropZone.style.display = 'block';
        objectsTableContainer.classList.add('drag-over');
    });

    objectsViewEl.addEventListener('dragleave', (e) => {
        if (!objectsViewEl.contains(e.relatedTarget)) {
            dropZone.style.display = 'none';
            objectsTableContainer.classList.remove('drag-over');
        }
    });

    objectsViewEl.addEventListener('drop', (e) => {
        e.preventDefault();
        dropZone.style.display = 'none';
        objectsTableContainer.classList.remove('drag-over');

        // Dropped folders used to upload nothing, or flatten to bare file
        // names. Walk them through the entries API to keep their structure.
        // The entries must be taken synchronously, inside the event.
        const items = e.dataTransfer.items;
        const entries = items
            ? Array.from(items)
                  .map((i) => i.webkitGetAsEntry?.())
                  .filter(Boolean)
            : [];
        if (entries.length > 0) {
            collectEntries(entries)
                .then((list) => {
                    if (list.length > 0) uploadFiles(list);
                })
                .catch((err) =>
                    showToast(`Could not read the dropped items: ${err.message}`, 'error'),
                );
        } else if (e.dataTransfer.files.length > 0) {
            uploadFiles(
                Array.from(e.dataTransfer.files).map((file) => ({ file, relPath: file.name })),
            );
        }
    });

    // collectEntries flattens dropped files and folders into {file, relPath}.
    async function collectEntries(entries) {
        const out = [];
        async function walk(entry, path) {
            if (entry.isFile) {
                const file = await new Promise((resolve, reject) => entry.file(resolve, reject));
                out.push({ file, relPath: path + file.name });
            } else if (entry.isDirectory) {
                const reader = entry.createReader();
                // readEntries returns a directory in batches; read until empty.
                for (;;) {
                    const batch = await new Promise((resolve, reject) =>
                        reader.readEntries(resolve, reject),
                    );
                    if (batch.length === 0) break;
                    for (const child of batch) {
                        await walk(child, path + entry.name + '/');
                    }
                }
            }
        }
        for (const entry of entries) {
            await walk(entry, '');
        }
        return out;
    }

    const MULTIPART_THRESHOLD = 10 * 1024 * 1024; // 10MB

    // The upload in progress, if any: whether it was cancelled, and its
    // in-flight requests so Cancel can abort them.
    let activeUpload = null;

    uploadCancelBtn.addEventListener('click', () => {
        if (!activeUpload) return;
        activeUpload.cancelled = true;
        activeUpload.cancelController?.abort();
        activeUpload.xhrs.forEach((xhr) => {
            xhr.abort();
        });
    });

    function setUploadProgress(pct, index, total, name) {
        uploadProgressFill.style.width = pct + '%';
        uploadPercent.textContent = `${pct}% (${index}/${total})`;
        uploadFileName.textContent = name;
    }

    async function uploadFiles(items) {
        if (activeUpload) {
            showToast('Another upload is still in progress', 'info');
            return;
        }
        const upload = {
            cancelled: false,
            xhrs: new Set(),
            bucket: browserState.state.bucket,
            prefix: browserState.state.prefix,
        };
        activeUpload = upload;
        uploadProgress.style.display = 'block';

        let uploaded = 0;
        try {
            for (let i = 0; i < items.length && !upload.cancelled; i++) {
                upload.cancelController = new AbortController();
                const { file, relPath } = items[i];
                const key = objectKeyFor(upload.prefix, relPath);
                setUploadProgress(0, i + 1, items.length, relPath);
                try {
                    if (file.size >= MULTIPART_THRESHOLD) {
                        await uploadLargeFile(upload, file, key, i + 1, items.length, relPath);
                    } else {
                        await uploadSmallFile(upload, file, key, i + 1, items.length, relPath);
                    }
                    uploaded++;
                } catch (err) {
                    if (err instanceof UploadCancelled || upload.cancelled) break;
                    showToast(`Failed to upload "${relPath}": ${err.message}`, 'error');
                }
            }
        } finally {
            activeUpload = null;
            uploadProgress.style.display = 'none';
            uploadProgressFill.style.width = '0%';
            // One summary rather than a toast per file, which a folder of
            // hundreds of files turned into an unreadable stack.
            if (upload.cancelled) {
                showToast(
                    `Upload cancelled (${uploaded} of ${items.length} file${items.length !== 1 ? 's' : ''} uploaded)`,
                    'info',
                );
            } else if (uploaded > 0) {
                showToast(
                    items.length === 1
                        ? `"${items[0].relPath}" uploaded successfully`
                        : `${uploaded} of ${items.length} files uploaded`,
                    uploaded === items.length ? 'success' : 'info',
                );
            }
            resetPagination();
            loadObjects();
        }
    }

    // Downloads go through a short-lived, single-use link on the console's
    // own origin, which the browser then downloads natively. The object used
    // to be fetched whole into memory and saved from a blob, which failed on
    // large files; and a presigned S3-API URL is on another port, where
    // browsers ignore <a download>.
    async function downloadObject(key, versionId = '', bucket = browserState.state.bucket) {
        try {
            const resp = await api(
                'POST',
                `/api/buckets/${encodeURIComponent(bucket)}/objects/download-link`,
                {
                    key,
                    versionId,
                },
            );
            if (!resp.ok) {
                throw new Error(await errorFrom(resp, 'Download failed'));
            }
            const { url } = await resp.json();
            const a = document.createElement('a');
            a.href = url;
            a.download = key.split('/').pop();
            document.body.appendChild(a);
            a.click();
            a.remove();
        } catch (err) {
            showToast('Failed to download: ' + err.message, 'error');
        }
    }

    // ---- Bulk delete ----

    const BATCH_DELETE_SIZE = 1000;

    // deleteKeys deletes keys in batches, one request per 1,000 keys — the
    // console used to send one DELETE per object — and reports the outcome.
    async function deleteKeys(keys, bucket = browserState.state.bucket) {
        let deleted = 0;
        const failures = [];
        for (let i = 0; i < keys.length; i += BATCH_DELETE_SIZE) {
            const resp = await api('POST', `/api/buckets/${bucket}/objects/delete`, {
                keys: keys.slice(i, i + BATCH_DELETE_SIZE),
            });
            if (!resp.ok) throw new Error(await errorFrom(resp, 'Delete failed'));
            const data = await resp.json();
            deleted += data.deleted.length;
            failures.push(...data.errors);
        }
        return { deleted, failures };
    }

    function reportDeletion({ deleted, failures, failureCount = failures.length }) {
        if (deleted > 0)
            showToast(`Deleted ${deleted} object${deleted !== 1 ? 's' : ''}`, 'success');
        if (failures.length > 0) {
            showToast(
                `Failed to delete ${failureCount} object${failureCount !== 1 ? 's' : ''}: ${failures[0].error}`,
                'error',
            );
        }
    }

    async function deleteFolder(prefix, name) {
        const bucket = browserState.state.bucket;
        const ok = await confirmPremium(`Delete the folder "${name}" and all objects under it?`);
        if (!ok) return;
        try {
            reportDeletion(await window.StivaFolders.deleteFolderContents({ api, bucket, prefix }));
        } catch (err) {
            showToast(err.message, 'error');
        }
        if (bucket === browserState.state.bucket) loadObjects();
    }

    // ---- Create Folder ----

    createFolderBtn.addEventListener('click', () => {
        openModal('create-folder-modal');
        $('#new-folder-error').style.display = 'none';
        $('#new-folder-name').value = '';
        $('#new-folder-name').focus();
    });

    $('#confirm-create-folder').addEventListener('click', async () => {
        const name = $('#new-folder-name').value.trim();
        // Names were not checked at all, so "a/b" or ".." produced keys that
        // did not match the folder the user asked for.
        const problem = validateFolderName(name);
        const folderError = $('#new-folder-error');
        folderError.textContent = problem;
        folderError.style.display = problem ? 'block' : 'none';
        if (problem) return;

        const key = browserState.state.prefix + name + '/';

        // Create a "folder" by uploading a zero-byte placeholder object.
        const formData = new FormData();
        formData.append('file', new Blob(['']), '.keep');

        const placeholderKey = key + FOLDER_PLACEHOLDER;

        await apiAction(
            'POST',
            `/api/buckets/${browserState.state.bucket}/objects/upload?key=${encodeURIComponent(placeholderKey)}`,
            {
                body: formData,
                isFormData: true,
                successMsg: `Folder "${name}" created`,
                errorMsg: 'Failed to create folder',
                onSuccess: () => {
                    closeModal('create-folder-modal');
                    loadObjects();
                },
            },
        );
    });

    // ---- Modal Helpers ----

    // Element that had focus before a modal opened, so it can be restored.
    let focusBeforeModal = null;

    const FOCUSABLE = 'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])';

    function isAnyModalOpen() {
        return Array.from($$('.modal-overlay')).some(
            (m) => m.style.display && m.style.display !== 'none',
        );
    }

    // Keep focus inside the open dialog: without this, Tab walks into the page
    // behind the modal, which is both confusing and a screen-reader trap.
    function trapFocus(e) {
        if (e.key !== 'Tab') return;
        const modal = Array.from($$('.modal-overlay')).find(
            (m) => m.style.display && m.style.display !== 'none',
        );
        if (!modal) return;

        const focusable = Array.from(modal.querySelectorAll(FOCUSABLE)).filter(
            (el) => !el.disabled && el.offsetParent !== null,
        );
        if (focusable.length === 0) return;

        const first = focusable[0];
        const last = focusable[focusable.length - 1];

        if (e.shiftKey && document.activeElement === first) {
            e.preventDefault();
            last.focus();
        } else if (!e.shiftKey && document.activeElement === last) {
            e.preventDefault();
            first.focus();
        }
    }

    function openModal(id) {
        const modal = document.getElementById(id);
        focusBeforeModal = document.activeElement;
        modal.style.display = 'flex';
        modal.setAttribute('aria-hidden', 'false');

        // Move focus into the dialog so keyboard and screen-reader users land
        // on the content rather than being left behind it.
        const focusable = modal.querySelector(FOCUSABLE);
        if (focusable) focusable.focus();

        document.addEventListener('keydown', trapFocus);
    }

    function closeModal(id) {
        const modal = document.getElementById(id);
        if (modal.cancelConfirmation) {
            const cancel = modal.cancelConfirmation;
            modal.cancelConfirmation = null;
            cancel();
            return;
        }
        modal.style.display = 'none';
        modal.setAttribute('aria-hidden', 'true');

        if (id === 'preview-modal') {
            previewController?.abort();
            previewContentContainer.innerHTML = '';
            if (currentPreviewBlobURL) {
                URL.revokeObjectURL(currentPreviewBlobURL);
                currentPreviewBlobURL = '';
            }
        }
        if (id === 'share-modal') {
            shareTargetKey = '';
        }

        if (!isAnyModalOpen()) {
            document.removeEventListener('keydown', trapFocus);
        }

        // Return focus where the user left it.
        if (focusBeforeModal && typeof focusBeforeModal.focus === 'function') {
            focusBeforeModal.focus();
            focusBeforeModal = null;
        }
    }

    async function showShareModal(key) {
        shareTargetKey = key;
        shareExpires.value = '3600';
        shareUrlInput.value = 'Generating link...';
        openModal('share-modal');
        await updateShareLink();
    }

    async function updateShareLink() {
        if (!shareTargetKey) return;
        const expires = shareExpires.value;
        try {
            const resp = await api(
                'GET',
                `/api/buckets/${browserState.state.bucket}/objects/presign?key=${encodeURIComponent(shareTargetKey)}&expires=${expires}&download=1`,
            );
            if (resp.ok) {
                const data = await resp.json();
                shareUrlInput.value = data.url;
            } else {
                shareUrlInput.value = 'Failed to generate link';
                showToast('Failed to generate pre-signed link', 'error');
            }
        } catch (_err) {
            shareUrlInput.value = 'Failed to generate link';
            showToast('Failed to generate pre-signed link', 'error');
        }
    }

    shareExpires.addEventListener('change', updateShareLink);

    copyShareUrlBtn.addEventListener('click', () => {
        shareUrlInput.select();
        shareUrlInput.setSelectionRange(0, 99999);
        navigator.clipboard
            .writeText(shareUrlInput.value)
            .then(() => showToast('Link copied to clipboard', 'success'))
            .catch(() => showToast('Failed to copy link', 'error'));
    });

    let previewController = null;
    async function showPreview(key, size, contentType) {
        previewController?.abort();
        const controller = new AbortController();
        previewController = controller;
        const filename = key.split('/').pop();
        previewTitle.textContent = `Preview: ${filename}`;
        previewFileSize.textContent = formatSize(size);

        const maxPreviewSize = 10 * 1024 * 1024; // 10MB
        if (size > maxPreviewSize) {
            previewContentContainer.innerHTML = `
                <div class="preview-notice">
                    <div class="preview-notice-icon">⚠️</div>
                    <p class="preview-notice-text">This file is too large to preview (${formatSize(size)}). Preview limit is 10 MB.</p>
                    <button id="preview-download-btn" class="btn btn-primary">Download File</button>
                </div>
            `;
            openModal('preview-modal');
            document.getElementById('preview-download-btn').addEventListener('click', () => {
                downloadObject(key);
            });
            return;
        }

        previewContentContainer.innerHTML = '<div class="preview-status">Loading preview...</div>';
        openModal('preview-modal');

        try {
            // Fetched through the console's own /objects/download endpoint
            // (same origin, authenticated) rather than a presigned S3-API
            // URL: that URL points at a different port by default, which the
            // console's own CSP (img-src/media-src/frame-src/connect-src
            // 'self') then blocks, breaking every preview type below. blob:
            // URLs are already permitted by the CSP for img/media/frame-src.
            const resp = await api(
                'GET',
                `/api/buckets/${browserState.state.bucket}/objects/download?key=${encodeURIComponent(key)}`,
                null,
                false,
                controller.signal,
            );
            if (!resp.ok) {
                throw new Error('Failed to fetch file for preview');
            }
            const blob = await resp.blob();
            controller.signal.throwIfAborted();
            if (currentPreviewBlobURL) {
                URL.revokeObjectURL(currentPreviewBlobURL);
            }
            currentPreviewBlobURL = URL.createObjectURL(blob);
            const blobURL = currentPreviewBlobURL;

            const kind = previewKind(contentType, filename);
            const ext = '.' + filename.split('.').pop().toLowerCase();

            if (kind === 'image') {
                previewContentContainer.innerHTML = `<img src="${blobURL}" class="preview-media" alt="">`;
            } else if (kind === 'video') {
                previewContentContainer.innerHTML = `<video src="${blobURL}" class="preview-media" controls autoplay></video>`;
            } else if (kind === 'audio') {
                previewContentContainer.innerHTML = `
                    <div class="preview-audio-wrap">
                        <audio src="${blobURL}" class="preview-audio" controls autoplay></audio>
                    </div>
                `;
            } else if (kind === 'pdf') {
                previewContentContainer.innerHTML = `<iframe src="${blobURL}" class="preview-frame" sandbox="allow-scripts" title="PDF preview"></iframe>`;
            } else if (kind === 'text') {
                const isTruncated = size > 256 * 1024;
                const text = await blob.slice(0, 256 * 1024).text();
                controller.signal.throwIfAborted();
                let escapedText = escapeHtml(text);
                if (isTruncated) {
                    escapedText +=
                        '\n\n--- [TRUNCATED] File is larger than 256 KB. Showing the first 256 KB. ---';
                }
                previewContentContainer.innerHTML = `
                    <pre class="preview-text">${escapedText}</pre>
                `;
            } else {
                previewContentContainer.innerHTML = `
                    <div class="preview-notice">
                        <div class="preview-notice-icon">📄</div>
                        <p class="preview-notice-text">No preview available for this file type (${escapeHtml(ext)}).</p>
                        <button id="preview-download-btn" class="btn btn-primary">Download File</button>
                    </div>
                `;
                document.getElementById('preview-download-btn').addEventListener('click', () => {
                    downloadObject(key);
                });
            }
        } catch (err) {
            if (controller.signal.aborted) return;
            previewContentContainer.innerHTML = `<div class="preview-status preview-status-error">Error loading preview: ${escapeHtml(err.message)}</div>`;
        }
    }

    // Close modals via close buttons and overlay click
    $$('[data-close]').forEach((btn) => {
        btn.addEventListener('click', () => closeModal(btn.dataset.close));
    });

    function resetPagination() {
        browserState.resetPagination();
    }

    prevPageBtn.addEventListener('click', () => {
        if (browserState.state.pageIndex > 0) {
            browserState.state.pageIndex--;
            loadObjects();
        }
    });

    nextPageBtn.addEventListener('click', () => {
        browserState.state.pageIndex++;
        loadObjects();
    });

    let searchDebounceTimeout = null;
    searchInput.addEventListener('input', (e) => {
        clearTimeout(searchDebounceTimeout);
        searchDebounceTimeout = setTimeout(() => {
            browserState.state.searchPrefix = e.target.value.trim();
            resetPagination();
            loadObjects();
        }, 300);
    });

    // Route overlay and Escape dismissals through closeModal so focus is
    // restored and listeners are cleaned up, rather than just hiding the node.
    $$('.modal-overlay').forEach((overlay) => {
        overlay.addEventListener('click', (e) => {
            if (e.target === overlay) {
                closeModal(overlay.id);
            }
        });
    });

    window.addEventListener('keydown', (e) => {
        if (e.key !== 'Escape') return;
        $$('.modal-overlay').forEach((overlay) => {
            if (overlay.style.display && overlay.style.display !== 'none') {
                closeModal(overlay.id);
            }
        });
    });

    // Enter key in modal inputs
    $('#new-bucket-name').addEventListener('keypress', (e) => {
        if (e.key === 'Enter') {
            e.preventDefault();
            $('#confirm-create-bucket').click();
        }
    });

    $('#new-folder-name').addEventListener('keypress', (e) => {
        if (e.key === 'Enter') {
            e.preventDefault();
            $('#confirm-create-folder').click();
        }
    });

    const thName = $('#th-name');
    const thSize = $('#th-size');
    const thDate = $('#th-date');

    function handleSort(field) {
        if (browserState.state.sortField === field) {
            browserState.state.sortOrder = browserState.state.sortOrder === 'asc' ? 'desc' : 'asc';
        } else {
            browserState.state.sortField = field;
            browserState.state.sortOrder = 'asc';
        }
        renderObjectsTable();
    }

    // Sortable headers must be operable by keyboard, not just the mouse, and
    // must announce their state via aria-sort.
    function makeSortable(th, field) {
        if (!th) return;
        th.title = 'Sort this page';
        th.setAttribute('role', 'columnheader');
        th.setAttribute('tabindex', '0');
        th.setAttribute('aria-sort', 'none');
        th.addEventListener('click', () => handleSort(field));
        th.addEventListener('keydown', (e) => {
            if (e.key === 'Enter' || e.key === ' ') {
                e.preventDefault();
                handleSort(field);
            }
        });
    }

    makeSortable(thName, 'name');
    makeSortable(thSize, 'size');
    makeSortable(thDate, 'date');

    const selectAllCheckbox = $('#select-all-objects');
    if (selectAllCheckbox) {
        selectAllCheckbox.addEventListener('change', (e) => {
            const checked = e.target.checked;
            const rowCheckboxes = objectsTbody.querySelectorAll('.object-select');
            rowCheckboxes.forEach((cb) => {
                cb.checked = checked;
            });
            updateBatchDeleteBtnVisibility();
        });
    }

    const batchDeleteBtn = $('#batch-delete-btn');
    if (batchDeleteBtn) {
        batchDeleteBtn.addEventListener('click', async () => {
            const bucket = browserState.state.bucket;
            const checkedBoxes = objectsTbody.querySelectorAll('.object-select:checked');
            const keys = Array.from(checkedBoxes).map((cb) => cb.dataset.key);
            if (keys.length === 0) return;

            const ok = await confirmPremium(
                `Are you sure you want to delete the ${keys.length} selected object(s)?`,
            );
            if (!ok) return;

            batchDeleteBtn.disabled = true;
            const originalHTML = batchDeleteBtn.innerHTML;
            batchDeleteBtn.textContent = 'Deleting...';

            try {
                reportDeletion(await deleteKeys(keys, bucket));
                loadObjects();
            } catch (err) {
                showToast(err.message, 'error');
                loadObjects();
            } finally {
                batchDeleteBtn.disabled = false;
                batchDeleteBtn.innerHTML = originalHTML;
            }
        });
    }

    // ---- Utilities ----

    // ---- Init ----

    function init() {
        if (token) {
            // Verify token by trying to load buckets
            api('GET', '/api/buckets')
                .then((resp) => {
                    if (resp.ok) {
                        showDashboard();
                    } else {
                        logout();
                    }
                })
                .catch(() => logout());
        }

        // Auto-refresh objects list every 10 seconds silently.
        setInterval(() => {
            if (!token || !browserState.state.bucket || document.visibilityState !== 'visible')
                return;

            // Modals are opened with `display: flex`, so the previous check for
            // `display: block` never matched and the table refreshed underneath
            // open dialogs.
            if (isAnyModalOpen()) return;

            const activeEl = document.activeElement;
            if (activeEl && (activeEl.tagName === 'INPUT' || activeEl.tagName === 'TEXTAREA'))
                return;

            // Refreshing while the user is mid-selection would silently discard
            // the checkboxes they were about to act on.
            if (objectsTbody.querySelector('.object-select:checked')) return;

            loadObjects(true);
        }, 10000);
    }

    init();
})();
