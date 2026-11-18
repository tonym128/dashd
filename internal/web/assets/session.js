// session.js — the session detail page's few controls. No inline handlers:
// the CSP forbids them.
(function () {
    'use strict';
    const toastEl = document.getElementById('toast');
    let timer = null;
    function toast(msg) {
        if (!toastEl) return;
        toastEl.textContent = msg;
        toastEl.classList.add('is-visible');
        clearTimeout(timer);
        timer = setTimeout(() => toastEl.classList.remove('is-visible'), 2200);
    }

    // Share bars: widths come from data attributes so the markup carries no
    // style attributes.
    document.querySelectorAll('[data-width]').forEach(el => {
        el.style.width = el.dataset.width;
    });

    document.addEventListener('click', async e => {
        const copy = e.target.closest('[data-copy-text]');
        if (copy) {
            const text = copy.dataset.copyText || '';
            try {
                await navigator.clipboard.writeText(text);
                toast('Copied resume command');
            } catch (_) {
                window.prompt('Copy the resume command:', text);
            }
            return;
        }
        const forget = e.target.closest('[data-forget-session]');
        if (forget) {
            if (!window.confirm('Delete this session and all of its calls from the database? This cannot be undone.')) return;
            try {
                const resp = await fetch('/api/admin/forget-session', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json', 'X-Dashd-Request': '1' },
                    body: JSON.stringify({ uid: forget.dataset.forgetSession }),
                });
                if (!resp.ok) throw new Error(resp.status === 403 ? 'admin access required' : 'status ' + resp.status);
                toast('Session forgotten');
                setTimeout(() => { location.href = '/'; }, 800);
            } catch (err) {
                toast('Could not forget session: ' + err.message);
            }
        }
    });
})();
