// ================================================================
// Stiva Console — pure helpers
// ================================================================
// Logic with no DOM or network access, kept apart from app.js so it can be
// unit-tested with Node (see ../jstest). Loaded before app.js, which reads it
// from window.StivaLib.

((root) => {
    const MiB = 1024 * 1024;
    const MIN_PART_SIZE = 5 * MiB; // S3 minimum for every part but the last
    const MAX_PART_SIZE = 5 * 1024 * MiB; // S3 maximum part size
    const MAX_PARTS = 10000; // S3 maximum parts per upload

    // partSizeFor picks the multipart part size for a file: the 5 MiB minimum
    // where that fits within 10,000 parts, otherwise the smallest whole-MiB
    // size that does. A fixed 5 MiB used to cap console uploads at ~48.8 GiB.
    // Returns null for a file too large for S3 multipart at all.
    function partSizeFor(fileSize) {
        const needed = Math.ceil(fileSize / MAX_PARTS / MiB) * MiB;
        const size = Math.max(MIN_PART_SIZE, needed);
        return size > MAX_PART_SIZE ? null : size;
    }

    // validateFolderName returns why a folder name is unusable, or '' when it
    // is fine. A name becomes one path segment of object keys, so separators,
    // dot segments and control characters are refused.
    function validateFolderName(name) {
        const trimmed = (name || '').trim();
        if (!trimmed) return 'Enter a folder name.';
        if (trimmed === '.' || trimmed === '..') return 'A folder cannot be named "." or "..".';
        if (/[/\\]/.test(trimmed)) return 'A folder name cannot contain "/" or "\\".';
        // biome-ignore lint/suspicious/noControlCharactersInRegex: reject control characters in object key segments.
        if (/[\u0000-\u001f\u007f]/.test(trimmed))
            return 'A folder name cannot contain control characters.';
        if (new TextEncoder().encode(trimmed).length > 255)
            return 'A folder name can be at most 255 bytes.';
        return '';
    }

    // objectKeyFor builds the key for a file uploaded into prefix. relativePath
    // is the file's path inside a dropped or selected folder (just its name
    // for a single file), so folder uploads keep their structure.
    function objectKeyFor(prefix, relativePath) {
        const clean = String(relativePath).replace(/\\/g, '/').replace(/^\/+/, '');
        return prefix + clean;
    }

    // retryDelay is the wait before retry number attempt (0-based) of a
    // failed part upload.
    function retryDelay(attempt) {
        return Math.min(500 * 2 ** attempt, 8000);
    }

    // apiErrorMessage turns a failed console API response into a message for
    // the user: the server's own explanation when it sent one.
    function apiErrorMessage(status, bodyText, fallback) {
        if (status === 0) return `${fallback}: the server could not be reached`;
        try {
            const data = JSON.parse(bodyText);
            if (data && typeof data.error === 'string' && data.error) return data.error;
        } catch (_) {
            // Not JSON: fall through.
        }
        if (status === 413) return 'The file is too large for this server';
        return `${fallback} (HTTP ${status})`;
    }

    function formatSize(bytes) {
        if (!Number.isFinite(bytes) || bytes < 0) return '—';
        if (bytes === 0) return '0 B';
        const units = ['B', 'KB', 'MB', 'GB', 'TB'];
        const i = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
        const size = (bytes / 1024 ** i).toFixed(i > 0 ? 1 : 0);
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
            if (
                [
                    'application/json',
                    'application/xml',
                    'application/javascript',
                    'application/x-javascript',
                    'application/yaml',
                    'application/x-yaml',
                    'application/x-sh',
                ].includes(ct)
            )
                return 'text';
        }

        const ext = '.' + filename.split('.').pop().toLowerCase();
        if (['.png', '.jpg', '.jpeg', '.gif', '.svg', '.webp', '.bmp', '.ico'].includes(ext))
            return 'image';
        if (['.mp4', '.webm', '.ogg', '.mov'].includes(ext)) return 'video';
        if (['.mp3', '.wav', '.m4a', '.aac'].includes(ext)) return 'audio';
        if (ext === '.pdf') return 'pdf';
        if (
            [
                '.txt',
                '.json',
                '.js',
                '.ts',
                '.go',
                '.html',
                '.css',
                '.md',
                '.log',
                '.env',
                '.yml',
                '.yaml',
                '.xml',
                '.ini',
                '.conf',
                '.sh',
                '.py',
            ].includes(ext)
        )
            return 'text';
        return 'none';
    }

    const lib = {
        partSizeFor,
        validateFolderName,
        objectKeyFor,
        retryDelay,
        apiErrorMessage,
        formatSize,
        escapeHtml,
        previewKind,
    };

    if (typeof module !== 'undefined' && module.exports) {
        module.exports = lib;
    } else {
        root.StivaLib = lib;
    }
})(typeof window !== 'undefined' ? window : this);
