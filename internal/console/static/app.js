// ================================================================
// Stiva Console — Application Logic
// ================================================================

(function () {
    'use strict';

    // ---- State ----
    let token = localStorage.getItem('stiva_token') || '';
    let currentBucket = '';
    let currentPrefix = '';
    let searchPrefix = '';
    let currentPageIndex = 0;
    let pageTokens = [''];
    let shareTargetKey = '';
    let currentPreviewBlobURL = '';
    let loadedItems = [];
    let currentSortField = 'name';
    let currentSortOrder = 'asc';

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

    const searchInput = $('#search-input');
    const paginationContainer = $('#pagination-container');
    const prevPageBtn = $('#prev-page-btn');
    const nextPageBtn = $('#next-page-btn');
    const pageIndicator = $('#page-indicator');

    const shareModal = $('#share-modal');
    const shareExpires = $('#share-expires');
    const shareUrlInput = $('#share-url-input');
    const copyShareUrlBtn = $('#copy-share-url-btn');

    const previewModal = $('#preview-modal');
    const previewTitle = $('#preview-title');
    const previewContentContainer = $('#preview-content-container');
    const previewFileSize = $('#preview-file-size');

    // ---- API Helpers ----

    async function api(method, path, body = null, isFormData = false) {
        const opts = {
            method,
            headers: {},
        };

        if (token) {
            opts.headers['Authorization'] = `Bearer ${token}`;
        }

        if (body && !isFormData) {
            opts.headers['Content-Type'] = 'application/json';
            opts.body = JSON.stringify(body);
        } else if (body && isFormData) {
            opts.body = body;
        }

        const resp = await fetch(path, opts);
        if (resp.status === 401) {
            logout();
            throw new Error('Session expired');
        }
        return resp;
    }

    // Wraps the try/await/if-ok/else-toast pattern repeated across nearly
    // every mutating console action (create bucket, delete object, toggle
    // public access, ...), which previously diverged slightly at each call
    // site in how errors were surfaced. Returns true on success.
    async function apiAction(method, path, { body = null, isFormData = false, successMsg, errorMsg, onSuccess, onError } = {}) {
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
        } catch (err) {
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
            if (!currentBucket) return;
            const publicVal = bucketPublicToggle.checked;
            await apiAction('POST', `/api/buckets/${currentBucket}/public?public=${publicVal}`, {
                successMsg: 'Bucket public access updated',
                errorMsg: 'Failed to update public access',
                onSuccess: () => {
                    if (bucketPublicBadge) {
                        bucketPublicBadge.style.display = publicVal ? 'inline-flex' : 'none';
                    }
                },
                onError: () => { bucketPublicToggle.checked = !publicVal; },
            });
        });
    }

    function logout() {
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
        } catch (err) {
            // Non-critical: the badge just keeps its static fallback text.
        }
    }

    // ---- Buckets ----

    async function showBucketsView() {
        bucketsView.classList.add('active');
        objectsView.classList.remove('active');
        currentBucket = '';
        currentPrefix = '';
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
            showToast('Failed to load buckets', 'error');
        }
    }

    function createBucketCard(bucket) {
        const card = document.createElement('div');
        card.className = 'bucket-card';

        const created = new Date(bucket.creationDate);
        const dateStr = created.toLocaleDateString('en-US', {
            year: 'numeric',
            month: 'short',
            day: 'numeric',
        });

        card.innerHTML = `
            <div class="bucket-card-header">
                <div class="bucket-card-icon">
                    <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"/></svg>
                </div>
                <button class="btn-icon btn-danger bucket-delete-btn" title="Delete bucket" aria-label="Delete bucket ${escapeHtml(bucket.name)}" data-bucket="${escapeHtml(bucket.name)}">
                    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/></svg>
                </button>
            </div>
            <div class="bucket-card-name">${escapeHtml(bucket.name)}</div>
            <div class="bucket-card-meta">
                <span>
                    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="3" y="4" width="18" height="18" rx="2" ry="2"/><line x1="16" y1="2" x2="16" y2="6"/><line x1="8" y1="2" x2="8" y2="6"/><line x1="3" y1="10" x2="21" y2="10"/></svg>
                    ${dateStr}
                </span>
                <span>
                    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M13 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9z"/><polyline points="13 2 13 9 20 9"/></svg>
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
            <div class="bucket-card-name" style="margin-top: 1rem;">
                <div class="skeleton skeleton-title"></div>
            </div>
            <div class="bucket-card-meta" style="margin-top: 0.5rem; display: flex; gap: 1.5rem;">
                <div class="skeleton skeleton-meta" style="width: 80px;"></div>
                <div class="skeleton skeleton-meta" style="width: 70px;"></div>
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

    // ---- Objects Browser ----

    function openBucket(name) {
        currentBucket = name;
        currentPrefix = '';
        searchPrefix = '';
        if (searchInput) searchInput.value = '';
        resetPagination();
        bucketsView.classList.remove('active');
        objectsView.classList.add('active');

        // Fetch and show bucket public status
        api('GET', `/api/buckets/${name}/public`)
            .then(r => r.json())
            .then(data => {
                const isPublic = !!data.public;
                if (bucketPublicToggle) bucketPublicToggle.checked = isPublic;
                if (bucketPublicBadge) bucketPublicBadge.style.display = isPublic ? 'inline-flex' : 'none';
            })
            .catch(() => {});

        loadObjects();
    }

    const itemsPerPage = 50;

    async function loadObjects(silent = false) {
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
                prefix: currentPrefix,
                delimiter: '/',
                maxKeys: itemsPerPage,
            });

            if (searchPrefix) {
                params.set('searchPrefix', searchPrefix);
            }

            if (currentPageIndex > 0 && pageTokens[currentPageIndex]) {
                params.set('continuationToken', pageTokens[currentPageIndex]);
            }

            const resp = await api('GET', `/api/buckets/${currentBucket}/objects?${params}`);
            const data = await resp.json();
            // Hide the folder placeholder: it used to appear as a stray file
            // inside every folder created from the console.
            loadedItems = (data.items || []).filter(
                (item) => item.isPrefix || !item.key.endsWith(FOLDER_PLACEHOLDER)
            );

            renderObjectsTable();

            const hasNext = data.isTruncated;
            if (hasNext) {
                pageTokens[currentPageIndex + 1] = data.nextContinuationToken;
            }

            if (currentPageIndex > 0 || hasNext) {
                paginationContainer.style.display = 'flex';
                prevPageBtn.disabled = (currentPageIndex === 0);
                nextPageBtn.disabled = !hasNext;
                pageIndicator.textContent = `Page ${currentPageIndex + 1}`;
            } else {
                paginationContainer.style.display = 'none';
            }
        } catch (err) {
            if (!silent) {
                objectsTbody.innerHTML = '';
                objectsTableContainer.style.display = 'none';
                objectsEmpty.style.display = 'block';
                showToast('Failed to load objects', 'error');
            }
        }
    }

    function renderObjectsTable() {
        objectsTbody.innerHTML = '';

        if (loadedItems.length === 0) {
            objectsEmpty.style.display = 'block';
            objectsTableContainer.style.display = 'none';
            paginationContainer.style.display = 'none';
            objectCount.textContent = searchPrefix ? 'No matching items' : 'Empty';

            const headers = {
                name: $('#th-name'),
                size: $('#th-size'),
                date: $('#th-date')
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
        objectCount.textContent =
            `${loadedItems.length} item${loadedItems.length !== 1 ? 's' : ''}${scopeNote}`;

        const folders = loadedItems.filter(item => item.isPrefix);
        const files = loadedItems.filter(item => !item.isPrefix);

        folders.sort((a, b) => a.key.localeCompare(b.key));

        files.sort((a, b) => {
            let valA, valB;
            if (currentSortField === 'name') {
                valA = a.key.toLowerCase();
                valB = b.key.toLowerCase();
            } else if (currentSortField === 'size') {
                valA = a.size || 0;
                valB = b.size || 0;
            } else if (currentSortField === 'date') {
                valA = a.lastModified ? new Date(a.lastModified).getTime() : 0;
                valB = b.lastModified ? new Date(b.lastModified).getTime() : 0;
            }

            if (valA < valB) return currentSortOrder === 'asc' ? -1 : 1;
            if (valA > valB) return currentSortOrder === 'asc' ? 1 : -1;
            return 0;
        });

        const sortedItems = [...folders, ...files];

        sortedItems.forEach((item) => {
            objectsTbody.appendChild(createObjectRow(item));
        });

        const headers = {
            name: $('#th-name'),
            size: $('#th-size'),
            date: $('#th-date')
        };

        for (const [field, th] of Object.entries(headers)) {
            if (!th) continue;
            th.classList.remove('sort-asc', 'sort-desc');
            if (field === currentSortField) {
                th.classList.add(currentSortOrder === 'asc' ? 'sort-asc' : 'sort-desc');
                th.setAttribute('aria-sort', currentSortOrder === 'asc' ? 'ascending' : 'descending');
            } else {
                th.setAttribute('aria-sort', 'none');
            }
        }

        const selectAllCheckbox = $('#select-all-objects');
        if (selectAllCheckbox) {
            const allCheckboxes = Array.from(objectsTbody.querySelectorAll('.object-select'));
            const allChecked = allCheckboxes.length > 0 && allCheckboxes.every(c => c.checked);
            const anyChecked = allCheckboxes.some(c => c.checked);
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
                <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/><line x1="10" y1="11" x2="10" y2="17"/><line x1="14" y1="11" x2="14" y2="17"/></svg>
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
            const displayName = item.key.replace(currentPrefix, '').replace(/\/$/, '');
            tr.innerHTML = `
                <td></td>
                <td>
                    <div class="file-name">
                        <div class="file-icon folder">
                            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"/></svg>
                        </div>
                        <span class="file-name-text folder-link" data-prefix="${escapeHtml(item.key)}">${escapeHtml(displayName)}/</span>
                    </div>
                </td>
                <td>—</td>
                <td>—</td>
                <td></td>
            `;

            tr.querySelector('.folder-link').addEventListener('click', () => {
                currentPrefix = item.key;
                searchPrefix = '';
                if (searchInput) searchInput.value = '';
                resetPagination();
                loadObjects();
            });
        } else {
            // File
            const displayName = item.key.replace(currentPrefix, '');
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
                            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M13 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9z"/><polyline points="13 2 13 9 20 9"/></svg>
                        </div>
                        <span class="file-name-text preview-link" data-key="${escapeHtml(item.key)}">${escapeHtml(displayName)}</span>
                    </div>
                </td>
                <td>${size}</td>
                <td>${modified}</td>
                <td>
                    <div class="file-actions">
                        <button class="btn-icon share-btn" title="Share Link" aria-label="Share ${escapeHtml(displayName)}" data-key="${escapeHtml(item.key)}">
                            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M10 13a5 5 0 0 0 7.54-.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71"/><path d="M14 11a5 5 0 0 0-7.54.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71"/><line x1="8" y1="16" x2="16" y2="8"/></svg>
                        </button>
                        <button class="btn-icon download-btn" title="Download" aria-label="Download ${escapeHtml(displayName)}" data-key="${escapeHtml(item.key)}">
                            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/></svg>
                        </button>
                        <button class="btn-icon btn-danger delete-obj-btn" title="Delete" aria-label="Delete ${escapeHtml(displayName)}" data-key="${escapeHtml(item.key)}">
                            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/></svg>
                        </button>
                    </div>
                </td>
            `;

            tr.querySelector('.object-select').addEventListener('change', () => {
                updateBatchDeleteBtnVisibility();
                const allCheckboxes = Array.from(objectsTbody.querySelectorAll('.object-select'));
                const allChecked = allCheckboxes.length > 0 && allCheckboxes.every(c => c.checked);
                const anyChecked = allCheckboxes.some(c => c.checked);
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
                const ok = await confirmPremium(`Delete "${displayName}"?`);
                if (!ok) return;
                await apiAction('DELETE', `/api/buckets/${currentBucket}/objects?key=${encodeURIComponent(item.key)}`, {
                    successMsg: `"${displayName}" deleted`,
                    errorMsg: 'Failed to delete object',
                    onSuccess: loadObjects,
                });
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
                    <div class="skeleton skeleton-text" style="width: 140px; height: 1.2rem;"></div>
                </div>
            </td>
            <td><div class="skeleton skeleton-text" style="width: 60px;"></div></td>
            <td><div class="skeleton skeleton-text" style="width: 120px;"></div></td>
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
        if (!currentPrefix) {
            const bucketSpan = document.createElement('span');
            bucketSpan.className = 'breadcrumb-item current';
            bucketSpan.textContent = currentBucket;
            breadcrumb.appendChild(bucketSpan);
        } else {
            const bucketBtn = document.createElement('button');
            bucketBtn.className = 'breadcrumb-item';
            bucketBtn.textContent = currentBucket;
            bucketBtn.addEventListener('click', () => {
                currentPrefix = '';
                searchPrefix = '';
                if (searchInput) searchInput.value = '';
                resetPagination();
                loadObjects();
            });
            breadcrumb.appendChild(bucketBtn);

            // Prefix parts
            const parts = currentPrefix.split('/').filter(Boolean);
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
                        currentPrefix = prefix;
                        searchPrefix = '';
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

    fileInput.addEventListener('change', (e) => {
        if (e.target.files.length > 0) {
            uploadFiles(Array.from(e.target.files));
            fileInput.value = '';
        }
    });

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
        if (e.dataTransfer.files.length > 0) {
            uploadFiles(Array.from(e.dataTransfer.files));
        }
    });

    const CHUNK_SIZE = 5 * 1024 * 1024; // 5MB
    const MULTIPART_THRESHOLD = 10 * 1024 * 1024; // 10MB

    async function uploadFiles(files) {
        for (let i = 0; i < files.length; i++) {
            const file = files[i];
            const key = currentPrefix + file.name;

            uploadProgress.style.display = 'block';
            uploadPercent.textContent = `0% (1/${files.length})`;

            try {
                if (file.size >= MULTIPART_THRESHOLD) {
                    await uploadLargeFile(file, key, i + 1, files.length);
                } else {
                    await uploadSmallFile(file, key, i + 1, files.length);
                }
                showToast(`"${file.name}" uploaded successfully`, 'success');
            } catch (err) {
                showToast(`Failed to upload "${file.name}": ${err.message}`, 'error');
            }
        }

        uploadProgress.style.display = 'none';
        uploadProgressFill.style.width = '0%';
        resetPagination();
        loadObjects();
    }

    async function uploadSmallFile(file, key, index, total) {
        const formData = new FormData();
        formData.append('file', file);

        await new Promise((resolve, reject) => {
            const xhr = new XMLHttpRequest();
            // The key travels in the query string: the server streams the
            // multipart body straight to storage and so cannot rely on a form
            // field that arrives after the file part.
            xhr.open('POST', `/api/buckets/${currentBucket}/objects/upload?key=${encodeURIComponent(key)}`);
            xhr.setRequestHeader('Authorization', `Bearer ${token}`);

            xhr.upload.onprogress = (e) => {
                if (e.lengthComputable) {
                    const pct = Math.round((e.loaded / e.total) * 100);
                    uploadProgressFill.style.width = pct + '%';
                    uploadPercent.textContent = `${pct}% (${index}/${total})`;
                }
            };

            xhr.onload = () => {
                if (xhr.status >= 200 && xhr.status < 300) {
                    resolve();
                } else {
                    reject(new Error('Upload failed'));
                }
            };

            xhr.onerror = () => reject(new Error('Upload failed'));
            xhr.send(formData);
        });
    }

    async function uploadLargeFile(file, key, index, total) {
        const initResp = await api('POST', `/api/buckets/${currentBucket}/multipart/initiate`, {
            key,
            contentType: file.type || 'application/octet-stream'
        });
        if (!initResp.ok) {
            const data = await initResp.json();
            throw new Error(data.error || 'Failed to initiate multipart upload');
        }
        const { uploadId } = await initResp.json();

        const numChunks = Math.ceil(file.size / CHUNK_SIZE);
        const results = [];
        const activeUploads = new Map();
        const chunkProgress = new Array(numChunks + 1).fill(0);

        const updateProgress = () => {
            const totalUploaded = chunkProgress.reduce((sum, val) => sum + val, 0);
            const pct = Math.min(Math.round((totalUploaded / file.size) * 100), 99);
            uploadProgressFill.style.width = pct + '%';
            uploadPercent.textContent = `${pct}% (${index}/${total})`;
        };

        const uploadChunk = async (partNum) => {
            const start = (partNum - 1) * CHUNK_SIZE;
            const end = Math.min(start + CHUNK_SIZE, file.size);
            const blob = file.slice(start, end);
            const chunkSize = end - start;

            const formData = new FormData();
            formData.append('uploadId', uploadId);
            formData.append('key', key);
            formData.append('partNumber', partNum.toString());
            formData.append('file', blob);

            return new Promise((resolve, reject) => {
                const xhr = new XMLHttpRequest();
                activeUploads.set(partNum, xhr);

                xhr.open('POST', `/api/buckets/${currentBucket}/multipart/upload-part`);
                xhr.setRequestHeader('Authorization', `Bearer ${token}`);

                xhr.upload.onprogress = (e) => {
                    if (e.lengthComputable) {
                        chunkProgress[partNum] = e.loaded;
                        updateProgress();
                    }
                };

                xhr.onload = () => {
                    activeUploads.delete(partNum);
                    if (xhr.status >= 200 && xhr.status < 300) {
                        try {
                            const res = JSON.parse(xhr.responseText);
                            chunkProgress[partNum] = chunkSize;
                            updateProgress();
                            resolve(res);
                        } catch (e) {
                            reject(new Error(`Part ${partNum} response parse error`));
                        }
                    } else {
                        reject(new Error(`Part ${partNum} failed with status ${xhr.status}`));
                    }
                };

                xhr.onerror = () => {
                    activeUploads.delete(partNum);
                    reject(new Error(`Network error on part ${partNum}`));
                };

                xhr.send(formData);
            });
        };

        const chunkIndices = Array.from({ length: numChunks }, (_, i) => i + 1);
        
        try {
            const uploadQueue = async () => {
                while (chunkIndices.length > 0) {
                    const partNum = chunkIndices.shift();
                    const partResult = await uploadChunk(partNum);
                    results.push(partResult);
                }
            };

            const workers = Array.from({ length: Math.min(3, numChunks) }, () => uploadQueue());
            await Promise.all(workers);

            results.sort((a, b) => a.partNumber - b.partNumber);

            const completeResp = await api('POST', `/api/buckets/${currentBucket}/multipart/complete`, {
                uploadId,
                key,
                parts: results.map(p => ({ partNumber: p.partNumber, etag: p.etag }))
            });

            if (!completeResp.ok) {
                const data = await completeResp.json();
                throw new Error(data.error || 'Failed to complete multipart upload');
            }

            uploadProgressFill.style.width = '100%';
            uploadPercent.textContent = `100% (${index}/${total})`;
        } catch (err) {
            activeUploads.forEach(xhr => xhr.abort());
            
            await api('POST', `/api/buckets/${currentBucket}/multipart/abort`, {
                uploadId,
                key
            }).catch(() => {});
            
            throw err;
        }
    }

    // Fetches through the console's own /objects/download endpoint (same
    // origin, authenticated with the session token) rather than a presigned
    // S3-API URL. The presigned URL points at a different port by default,
    // and browsers ignore the <a download> attribute for cross-origin URLs —
    // the file would just navigate/render inline instead of downloading. The
    // console endpoint also always sets Content-Disposition: attachment.
    async function downloadObject(key) {
        try {
            const resp = await api('GET', `/api/buckets/${currentBucket}/objects/download?key=${encodeURIComponent(key)}`);
            if (!resp.ok) {
                throw new Error('Failed to download file');
            }
            const blob = await resp.blob();
            const blobURL = URL.createObjectURL(blob);

            const a = document.createElement('a');
            a.href = blobURL;
            a.download = key.split('/').pop();
            document.body.appendChild(a);
            a.click();
            document.body.removeChild(a);
            URL.revokeObjectURL(blobURL);
        } catch (err) {
            showToast('Failed to download: ' + err.message, 'error');
        }
    }

    // ---- Create Folder ----

    createFolderBtn.addEventListener('click', () => {
        openModal('create-folder-modal');
        $('#new-folder-name').value = '';
        $('#new-folder-name').focus();
    });

    $('#confirm-create-folder').addEventListener('click', async () => {
        const name = $('#new-folder-name').value.trim();
        if (!name) return;

        const key = currentPrefix + name + '/';

        // Create a "folder" by uploading a zero-byte placeholder object.
        const formData = new FormData();
        formData.append('file', new Blob(['']), '.keep');

        const placeholderKey = key + FOLDER_PLACEHOLDER;

        await apiAction('POST',
            `/api/buckets/${currentBucket}/objects/upload?key=${encodeURIComponent(placeholderKey)}`,
            {
                body: formData,
                isFormData: true,
                successMsg: `Folder "${name}" created`,
                errorMsg: 'Failed to create folder',
                onSuccess: () => {
                    closeModal('create-folder-modal');
                    loadObjects();
                },
            });
    });

    // ---- Modal Helpers ----

    // Element that had focus before a modal opened, so it can be restored.
    let focusBeforeModal = null;

    const FOCUSABLE = 'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])';

    function isAnyModalOpen() {
        return Array.from($$('.modal-overlay')).some(
            (m) => m.style.display && m.style.display !== 'none'
        );
    }

    // Keep focus inside the open dialog: without this, Tab walks into the page
    // behind the modal, which is both confusing and a screen-reader trap.
    function trapFocus(e) {
        if (e.key !== 'Tab') return;
        const modal = Array.from($$('.modal-overlay')).find(
            (m) => m.style.display && m.style.display !== 'none'
        );
        if (!modal) return;

        const focusable = Array.from(modal.querySelectorAll(FOCUSABLE)).filter(
            (el) => !el.disabled && el.offsetParent !== null
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
        modal.style.display = 'none';
        modal.setAttribute('aria-hidden', 'true');

        if (id === 'preview-modal') {
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
            const resp = await api('GET', `/api/buckets/${currentBucket}/objects/presign?key=${encodeURIComponent(shareTargetKey)}&expires=${expires}&download=1`);
            if (resp.ok) {
                const data = await resp.json();
                shareUrlInput.value = data.url;
            } else {
                shareUrlInput.value = 'Failed to generate link';
                showToast('Failed to generate pre-signed link', 'error');
            }
        } catch (err) {
            shareUrlInput.value = 'Failed to generate link';
            showToast('Failed to generate pre-signed link', 'error');
        }
    }

    shareExpires.addEventListener('change', updateShareLink);

    copyShareUrlBtn.addEventListener('click', () => {
        shareUrlInput.select();
        shareUrlInput.setSelectionRange(0, 99999);
        navigator.clipboard.writeText(shareUrlInput.value)
            .then(() => showToast('Link copied to clipboard', 'success'))
            .catch(() => showToast('Failed to copy link', 'error'));
    });

    // Maps a stored Content-Type to the preview kind. Falls back to the file
    // extension, which used to be the only signal — so an image saved without
    // an extension, or a .txt holding JSON, was previewed by its name rather
    // than by what the server actually recorded.
    function previewKind(contentType, filename) {
        const ct = (contentType || '').split(';')[0].trim().toLowerCase();
        if (ct && ct !== 'application/octet-stream') {
            if (ct.startsWith('image/')) return 'image';
            if (ct.startsWith('video/')) return 'video';
            if (ct.startsWith('audio/')) return 'audio';
            if (ct === 'application/pdf') return 'pdf';
            if (ct.startsWith('text/')) return 'text';
            if (['application/json', 'application/xml', 'application/javascript',
                 'application/x-javascript', 'application/yaml', 'application/x-yaml',
                 'application/x-sh'].includes(ct)) return 'text';
        }

        const ext = '.' + filename.split('.').pop().toLowerCase();
        if (['.png', '.jpg', '.jpeg', '.gif', '.svg', '.webp', '.bmp', '.ico'].includes(ext)) return 'image';
        if (['.mp4', '.webm', '.ogg', '.mov'].includes(ext)) return 'video';
        if (['.mp3', '.wav', '.m4a', '.aac'].includes(ext)) return 'audio';
        if (ext === '.pdf') return 'pdf';
        if (['.txt', '.json', '.js', '.ts', '.go', '.html', '.css', '.md', '.log', '.env',
             '.yml', '.yaml', '.xml', '.ini', '.conf', '.sh', '.py'].includes(ext)) return 'text';
        return 'none';
    }

    async function showPreview(key, size, contentType) {
        const filename = key.split('/').pop();
        previewTitle.textContent = `Preview: ${filename}`;
        previewFileSize.textContent = formatSize(size);

        const maxPreviewSize = 10 * 1024 * 1024; // 10MB
        if (size > maxPreviewSize) {
            previewContentContainer.innerHTML = `
                <div style="padding: 3rem 1.5rem; text-align: center; background: rgba(255, 255, 255, 0.02); border-radius: 6px; border: 1px solid var(--border); width: 100%;">
                    <div style="font-size: 3rem; margin-bottom: 1rem; opacity: 0.5;">⚠️</div>
                    <p style="margin-bottom: 1.5rem; color: var(--text-secondary);">This file is too large to preview (${formatSize(size)}). Preview limit is 10 MB.</p>
                    <button id="preview-download-btn" class="btn btn-primary">Download File</button>
                </div>
            `;
            openModal('preview-modal');
            document.getElementById('preview-download-btn').addEventListener('click', () => {
                downloadObject(key);
            });
            return;
        }

        previewContentContainer.innerHTML = '<div style="color: var(--text-secondary); padding: 2rem;">Loading preview...</div>';
        openModal('preview-modal');

        try {
            // Fetched through the console's own /objects/download endpoint
            // (same origin, authenticated) rather than a presigned S3-API
            // URL: that URL points at a different port by default, which the
            // console's own CSP (img-src/media-src/frame-src/connect-src
            // 'self') then blocks, breaking every preview type below. blob:
            // URLs are already permitted by the CSP for img/media/frame-src.
            const resp = await api('GET', `/api/buckets/${currentBucket}/objects/download?key=${encodeURIComponent(key)}`);
            if (!resp.ok) {
                throw new Error('Failed to fetch file for preview');
            }
            const blob = await resp.blob();
            if (currentPreviewBlobURL) {
                URL.revokeObjectURL(currentPreviewBlobURL);
            }
            currentPreviewBlobURL = URL.createObjectURL(blob);
            const blobURL = currentPreviewBlobURL;

            const kind = previewKind(contentType, filename);
            const ext = '.' + filename.split('.').pop().toLowerCase();

            if (kind === 'image') {
                previewContentContainer.innerHTML = `<img src="${blobURL}" style="max-width: 100%; max-height: 60vh; object-fit: contain; border-radius: 6px; border: 1px solid var(--border);">`;
            } else if (kind === 'video') {
                previewContentContainer.innerHTML = `<video src="${blobURL}" controls autoplay style="max-width: 100%; max-height: 60vh; border-radius: 6px; border: 1px solid var(--border);"></video>`;
            } else if (kind === 'audio') {
                previewContentContainer.innerHTML = `
                    <div style="padding: 2rem; width: 100%; display: flex; justify-content: center; background: rgba(255, 255, 255, 0.03); border-radius: 6px; border: 1px solid var(--border);">
                        <audio src="${blobURL}" controls autoplay style="width: 100%; max-width: 400px;"></audio>
                    </div>
                `;
            } else if (kind === 'pdf') {
                previewContentContainer.innerHTML = `<iframe src="${blobURL}" sandbox="allow-scripts" style="width: 100%; height: 60vh; border: none; border-radius: 6px; background: white;"></iframe>`;
            } else if (kind === 'text') {
                const isTruncated = size > 256 * 1024;
                let text = await blob.slice(0, 256 * 1024).text();
                let escapedText = escapeHtml(text);
                if (isTruncated) {
                    escapedText += '\n\n--- [TRUNCATED] File is larger than 256 KB. Showing the first 256 KB. ---';
                }
                previewContentContainer.innerHTML = `
                    <pre style="width: 100%; text-align: left; padding: 1.25rem; background: var(--input-bg); border-radius: 6px; overflow: auto; max-height: 60vh; font-family: monospace; white-space: pre-wrap; font-size: 0.875rem; border: 1px solid var(--border); margin: 0; color: var(--text);">${escapedText}</pre>
                `;
            } else {
                previewContentContainer.innerHTML = `
                    <div style="padding: 3rem 1.5rem; text-align: center; background: rgba(255, 255, 255, 0.02); border-radius: 6px; border: 1px solid var(--border); width: 100%;">
                        <div style="font-size: 3rem; margin-bottom: 1rem; opacity: 0.5;">📄</div>
                        <p style="margin-bottom: 1.5rem; color: var(--text-secondary);">No preview available for this file type (${ext}).</p>
                        <button id="preview-download-btn" class="btn btn-primary">Download File</button>
                    </div>
                `;
                document.getElementById('preview-download-btn').addEventListener('click', () => {
                    downloadObject(key);
                });
            }
        } catch (err) {
            previewContentContainer.innerHTML = `<div style="color: var(--danger); padding: 2rem;">Error loading preview: ${escapeHtml(err.message)}</div>`;
        }
    }

    // Close modals via close buttons and overlay click
    $$('[data-close]').forEach((btn) => {
        btn.addEventListener('click', () => closeModal(btn.dataset.close));
    });

    function resetPagination() {
        currentPageIndex = 0;
        pageTokens = [''];
    }

    prevPageBtn.addEventListener('click', () => {
        if (currentPageIndex > 0) {
            currentPageIndex--;
            loadObjects();
        }
    });

    nextPageBtn.addEventListener('click', () => {
        currentPageIndex++;
        loadObjects();
    });

    let searchDebounceTimeout = null;
    searchInput.addEventListener('input', (e) => {
        clearTimeout(searchDebounceTimeout);
        searchDebounceTimeout = setTimeout(() => {
            searchPrefix = e.target.value.trim();
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
        if (currentSortField === field) {
            currentSortOrder = currentSortOrder === 'asc' ? 'desc' : 'asc';
        } else {
            currentSortField = field;
            currentSortOrder = 'asc';
        }
        renderObjectsTable();
    }

    // Sortable headers must be operable by keyboard, not just the mouse, and
    // must announce their state via aria-sort.
    function makeSortable(th, field) {
        if (!th) return;
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
            rowCheckboxes.forEach(cb => {
                cb.checked = checked;
            });
            updateBatchDeleteBtnVisibility();
        });
    }

    const batchDeleteBtn = $('#batch-delete-btn');
    if (batchDeleteBtn) {
        batchDeleteBtn.addEventListener('click', async () => {
            const checkedBoxes = objectsTbody.querySelectorAll('.object-select:checked');
            const keys = Array.from(checkedBoxes).map(cb => cb.dataset.key);
            if (keys.length === 0) return;

            const ok = await confirmPremium(`Are you sure you want to delete the ${keys.length} selected object(s)?`);
            if (!ok) return;

            batchDeleteBtn.disabled = true;
            const originalHTML = batchDeleteBtn.innerHTML;
            batchDeleteBtn.textContent = 'Deleting...';

            const results = [];
            try {
                const batchSize = 5;
                for (let i = 0; i < keys.length; i += batchSize) {
                    const chunk = keys.slice(i, i + batchSize);
                    const chunkPromises = chunk.map(key =>
                        api('DELETE', `/api/buckets/${currentBucket}/objects?key=${encodeURIComponent(key)}`)
                    );
                    const chunkResults = await Promise.all(chunkPromises);
                    results.push(...chunkResults);
                }
                const succeeded = results.filter(r => r.ok).length;
                const failed = results.length - succeeded;

                if (succeeded > 0) {
                    showToast(`Successfully deleted ${succeeded} object(s)`, 'success');
                }
                if (failed > 0) {
                    showToast(`Failed to delete ${failed} object(s)`, 'error');
                }
                loadObjects();
            } catch (err) {
                showToast('An error occurred during batch deletion', 'error');
                loadObjects();
            } finally {
                batchDeleteBtn.disabled = false;
                batchDeleteBtn.innerHTML = originalHTML;
            }
        });
    }

    // ---- Utilities ----

    function confirmPremium(message) {
        return new Promise((resolve) => {
            const modal = document.getElementById('delete-modal');
            const msgEl = document.getElementById('delete-modal-msg');
            const confirmBtn = document.getElementById('confirm-delete-btn');
            
            msgEl.textContent = message;
            openModal('delete-modal');
            
            const handleConfirm = () => {
                cleanup();
                resolve(true);
            };
            
            const handleCancel = () => {
                cleanup();
                resolve(false);
            };
            
            const cleanup = () => {
                confirmBtn.removeEventListener('click', handleConfirm);
                modal.querySelectorAll('[data-close="delete-modal"]').forEach(btn => {
                    btn.removeEventListener('click', handleCancel);
                });
                modal.removeEventListener('click', handleOverlayClick);
                closeModal('delete-modal');
            };
            
            const handleOverlayClick = (e) => {
                if (e.target === modal) {
                    handleCancel();
                }
            };
            
            confirmBtn.addEventListener('click', handleConfirm);
            modal.querySelectorAll('[data-close="delete-modal"]').forEach(btn => {
                btn.addEventListener('click', handleCancel);
            });
            modal.addEventListener('click', handleOverlayClick);
        });
    }

    function formatSize(bytes) {
        if (bytes === 0) return '0 B';
        const units = ['B', 'KB', 'MB', 'GB', 'TB'];
        const i = Math.floor(Math.log(bytes) / Math.log(1024));
        const size = (bytes / Math.pow(1024, i)).toFixed(i > 0 ? 1 : 0);
        return `${size} ${units[i]}`;
    }

    // Escapes for element text *and* quoted-attribute contexts. The previous
    // textContent -> innerHTML round-trip relied on the HTML serializer, which
    // escapes only & < > and leaves quotes intact. Since this value is
    // interpolated into double-quoted attributes (data-key, aria-label, ...),
    // an object key containing a double quote could close the attribute early
    // and append an inline event handler.
    function escapeHtml(str) {
        return String(str)
            .replace(/&/g, '&amp;')
            .replace(/</g, '&lt;')
            .replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;')
            .replace(/'/g, '&#39;');
    }

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
            if (!token || !currentBucket || document.visibilityState !== 'visible') return;

            // Modals are opened with `display: flex`, so the previous check for
            // `display: block` never matched and the table refreshed underneath
            // open dialogs.
            if (isAnyModalOpen()) return;

            const activeEl = document.activeElement;
            if (activeEl && (activeEl.tagName === 'INPUT' || activeEl.tagName === 'TEXTAREA')) return;

            // Refreshing while the user is mid-selection would silently discard
            // the checkboxes they were about to act on.
            if (objectsTbody.querySelector('.object-select:checked')) return;

            loadObjects(true);
        }, 10000);
    }

    init();
})();
