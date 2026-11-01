// Confirmation ownership is separate from modal presentation and focus management.
((root) => {
    function createConfirmation({ document, openModal, closeModal }) {
        return function confirmPremium(
            message,
            {
                title = 'Confirm Deletion',
                confirmLabel = 'Delete',
                warning = 'Deletion may create a recoverable delete marker when versioning is enabled. Explicit version deletion is permanent.',
            } = {},
        ) {
            return new Promise((resolve) => {
                const modal = document.getElementById('delete-modal');
                modal.cancelConfirmation?.();
                const msgEl = document.getElementById('delete-modal-msg');
                const confirmBtn = document.getElementById('confirm-delete-btn');

                msgEl.textContent = message;
                document.getElementById('delete-modal-title').textContent = title;
                document.getElementById('delete-modal-warning').textContent = warning;
                confirmBtn.textContent = confirmLabel;
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
                    modal.cancelConfirmation = null;
                    confirmBtn.removeEventListener('click', handleConfirm);
                    modal.querySelectorAll('[data-close="delete-modal"]').forEach((btn) => {
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

                modal.cancelConfirmation = handleCancel;
                confirmBtn.addEventListener('click', handleConfirm);
                modal.querySelectorAll('[data-close="delete-modal"]').forEach((btn) => {
                    btn.addEventListener('click', handleCancel);
                });
                modal.addEventListener('click', handleOverlayClick);
            });
        };
    }
    const dialogs = { createConfirmation };
    if (typeof module !== 'undefined' && module.exports) module.exports = dialogs;
    else root.StivaDialogs = dialogs;
})(typeof window !== 'undefined' ? window : this);
