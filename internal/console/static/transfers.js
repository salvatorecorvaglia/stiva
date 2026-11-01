// Transfer transport, retries, and worker ownership are independent of browser views.
((root) => {
    const lib =
        typeof module !== 'undefined' && module.exports ? require('./lib.js') : root.StivaLib;
    class UploadCancelled extends Error {}

    const sleep = (ms, signal) =>
        new Promise((resolve, reject) => {
            if (signal?.aborted) {
                reject(new UploadCancelled('Upload cancelled'));
                return;
            }
            const abort = () => {
                clearTimeout(timer);
                reject(new UploadCancelled('Upload cancelled'));
            };
            const timer = setTimeout(() => {
                signal?.removeEventListener('abort', abort);
                resolve();
            }, ms);
            signal?.addEventListener('abort', abort, { once: true });
        });

    function createClient({ api, getToken, logout, errorFrom, setUploadProgress }) {
        const { apiErrorMessage, partSizeFor, retryDelay } = lib;
        const PART_ATTEMPTS = 3;
        const PART_CONCURRENCY = 3;
        // sendForm POSTs multipart form data as part of an upload, reporting
        // progress, and rejects with the server's own error message (the status
        // is attached, for deciding whether a retry can help) or UploadCancelled.
        function sendForm(upload, url, formData, onProgress) {
            return new Promise((resolve, reject) => {
                if (upload.cancelled || upload.failed) {
                    reject(new UploadCancelled('Upload cancelled'));
                    return;
                }
                const xhr = new XMLHttpRequest();
                upload.xhrs.add(xhr);
                xhr.open('POST', url);
                xhr.setRequestHeader('Authorization', `Bearer ${getToken()}`);
                xhr.upload.onprogress = (e) => {
                    if (e.lengthComputable && onProgress) onProgress(e.loaded, e.total);
                };
                const fail = (status, text) => {
                    const err = new Error(apiErrorMessage(status, text, 'Upload failed'));
                    err.status = status;
                    err.retryAfter = Number(xhr.getResponseHeader('Retry-After')) * 1000 || 0;
                    reject(err);
                };
                xhr.onload = () => {
                    upload.xhrs.delete(xhr);
                    if (xhr.status >= 200 && xhr.status < 300) {
                        resolve(xhr.responseText);
                    } else if (xhr.status === 401) {
                        logout();
                        fail(401, '{"error":"Session expired"}');
                    } else {
                        fail(xhr.status, xhr.responseText);
                    }
                };
                xhr.onerror = () => {
                    upload.xhrs.delete(xhr);
                    fail(0, '');
                };
                xhr.onabort = () => {
                    upload.xhrs.delete(xhr);
                    reject(new UploadCancelled('Upload cancelled'));
                };
                xhr.send(formData);
            });
        }

        async function uploadSmallFile(upload, file, key, index, total, name) {
            const formData = new FormData();
            formData.append('file', file);
            // The key travels in the query string: the server streams the
            // multipart body straight to storage and so cannot rely on a form
            // field that arrives after the file part.
            await sendForm(
                upload,
                `/api/buckets/${upload.bucket}/objects/upload?key=${encodeURIComponent(key)}`,
                formData,
                (loaded, size) =>
                    setUploadProgress(Math.round((loaded / size) * 100), index, total, name),
            );
        }

        async function uploadLargeFile(upload, file, key, index, total, name) {
            upload.cancelController ||= new AbortController();
            const partSize = partSizeFor(file.size);
            if (!partSize) {
                throw new Error('This file is larger than S3 allows (10,000 parts of 5 GiB)');
            }

            const initResp = await api(
                'POST',
                `/api/buckets/${upload.bucket}/multipart/initiate`,
                {
                    key,
                    contentType: file.type || 'application/octet-stream',
                },
                false,
                upload.cancelController.signal,
            );
            if (!initResp.ok) {
                throw new Error(await errorFrom(initResp, 'Failed to start the upload'));
            }
            const { uploadId } = await initResp.json();

            const numParts = Math.ceil(file.size / partSize);
            const partProgress = new Array(numParts + 1).fill(0);
            const results = [];
            const updateProgress = () => {
                const sent = partProgress.reduce((sum, val) => sum + val, 0);
                setUploadProgress(
                    Math.min(Math.round((sent / file.size) * 100), 99),
                    index,
                    total,
                    name,
                );
            };

            // A part is retried with backoff when the failure is one a retry can
            // fix (network error, server error, timeout, rate limit). One dropped
            // request used to abort the whole upload.
            const uploadPart = async (partNum) => {
                const start = (partNum - 1) * partSize;
                const end = Math.min(start + partSize, file.size);
                let throttledWait = 0;
                for (let attempt = 0; ; attempt++) {
                    if (upload.cancelled || upload.failed)
                        throw new UploadCancelled('Upload cancelled');

                    const formData = new FormData();
                    formData.append('uploadId', uploadId);
                    formData.append('key', key);
                    formData.append('partNumber', partNum.toString());
                    formData.append('file', file.slice(start, end));

                    try {
                        const text = await sendForm(
                            upload,
                            `/api/buckets/${upload.bucket}/multipart/upload-part`,
                            formData,
                            (loaded) => {
                                partProgress[partNum] = loaded;
                                updateProgress();
                            },
                        );
                        partProgress[partNum] = end - start;
                        updateProgress();
                        return JSON.parse(text);
                    } catch (err) {
                        if (err instanceof UploadCancelled || upload.cancelled || upload.failed)
                            throw new UploadCancelled('Upload cancelled');
                        if (err.status === 429 && throttledWait < 10 * 60 * 1000) {
                            const wait = Math.max(1000, err.retryAfter);
                            throttledWait += wait;
                            partProgress[partNum] = 0;
                            updateProgress();
                            await sleep(wait, upload.cancelController.signal);
                            attempt--;
                            continue;
                        }
                        const retryable =
                            !err.status ||
                            err.status >= 500 ||
                            err.status === 408 ||
                            err.status === 429;
                        if (!retryable || attempt + 1 >= PART_ATTEMPTS) {
                            throw new Error(`Part ${partNum} of ${numParts}: ${err.message}`);
                        }
                        partProgress[partNum] = 0;
                        updateProgress();
                        await sleep(
                            Math.max(retryDelay(attempt), err.retryAfter),
                            upload.cancelController.signal,
                        );
                    }
                }
            };

            const queue = Array.from({ length: numParts }, (_, i) => i + 1);
            let workers = [];
            try {
                const worker = async () => {
                    while (queue.length > 0) {
                        results.push(await uploadPart(queue.shift()));
                    }
                };
                workers = Array.from({ length: Math.min(PART_CONCURRENCY, numParts) }, worker);
                await Promise.all(workers);
                if (upload.cancelled) throw new UploadCancelled('Upload cancelled');

                results.sort((a, b) => a.partNumber - b.partNumber);
                const completeResp = await api(
                    'POST',
                    `/api/buckets/${upload.bucket}/multipart/complete`,
                    {
                        uploadId,
                        key,
                        parts: results.map((p) => ({ partNumber: p.partNumber, etag: p.etag })),
                    },
                    false,
                    upload.cancelController.signal,
                );
                if (!completeResp.ok) {
                    throw new Error(await errorFrom(completeResp, 'Failed to complete the upload'));
                }
                setUploadProgress(100, index, total, name);
            } catch (err) {
                // Stop and join every worker before discarding server-side state.
                upload.failed = true;
                upload.cancelController.abort();
                queue.length = 0;
                upload.xhrs.forEach((xhr) => {
                    xhr.abort();
                });
                await Promise.allSettled(workers);
                upload.failed = false;
                await api('POST', `/api/buckets/${upload.bucket}/multipart/abort`, {
                    uploadId,
                    key,
                }).catch(() => {});
                throw err;
            }
        }

        return { uploadSmallFile, uploadLargeFile };
    }
    const transfers = { createClient, UploadCancelled, sleep };
    if (typeof module !== 'undefined' && module.exports) module.exports = transfers;
    else root.StivaTransfers = transfers;
})(typeof window !== 'undefined' ? window : this);
