// app.js — page features layered on dashboard.js.
//
// dashboard.js owns the tables and charts; this file owns the controls around
// them: saved views, auto-refresh, the shortcuts dialog, the period comparison,
// the tags table, export links and the admin actions. It is a classic script
// loaded after dashboard.js, so it shares that file's globals (escapeHtml,
// currentFilterParams, dashboardData) and adds no inline handlers: the CSP
// forbids them.
(function () {
    'use strict';
    if (typeof document === 'undefined') return;

    const data = window.dashboardData || {};
    const toast = msg => {
        if (typeof window.showDashdToast === 'function') window.showDashdToast(msg);
    };
    const store = {
        get(key, fallback) {
            try {
                const v = localStorage.getItem(key);
                return v === null ? fallback : v;
            } catch (_) { return fallback; }
        },
        set(key, value) {
            try { localStorage.setItem(key, value); } catch (_) {}
        },
    };
    const money = (v, digits) => '$' + Number(v || 0).toFixed(digits === undefined ? 2 : digits);
    const filterQuery = () => (typeof currentFilterParams === 'function'
        ? currentFilterParams() : new URLSearchParams(location.search));

    // A state-changing request. The custom header is what a cross-site form
    // cannot send, and the server requires it for cookie-authenticated POSTs.
    async function dashdPost(url, body) {
        const resp = await fetch(url, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json', 'X-Dashd-Request': '1' },
            body: JSON.stringify(body || {}),
        });
        let payload = null;
        try { payload = await resp.json(); } catch (_) {}
        if (!resp.ok) {
            const reason = resp.status === 403 ? 'admin access required' : 'status ' + resp.status;
            throw new Error(reason);
        }
        return payload || {};
    }
    window.dashdPost = dashdPost;

    // ------------------------------------------------------------ "Updated 2m ago"
    const meta = document.getElementById('generated-meta');
    function ago(seconds) {
        if (seconds < 45) return 'just now';
        if (seconds < 3600) return Math.round(seconds / 60) + 'm ago';
        if (seconds < 86400) return Math.round(seconds / 3600) + 'h ago';
        return Math.round(seconds / 86400) + 'd ago';
    }
    if (meta && meta.dataset && meta.dataset.epoch) {
        const epoch = Number(meta.dataset.epoch);
        const paint = () => { meta.textContent = 'Updated ' + ago(Date.now() / 1000 - epoch); };
        paint();
        setInterval(paint, 30000);
    }

    // ------------------------------------------------------------ auto-refresh
    const auto = document.getElementById('auto-refresh');
    if (auto) {
        auto.value = store.get('dashd:autorefresh', '0');
        let timer = null;
        const arm = () => {
            clearInterval(timer);
            const secs = Number(auto.value) || 0;
            if (secs > 0) {
                timer = setInterval(() => {
                    // Never reload a tab nobody is looking at, or under someone typing.
                    if (document.visibilityState && document.visibilityState !== 'visible') return;
                    const tag = (document.activeElement && document.activeElement.tagName || '').toLowerCase();
                    if (tag === 'input' || tag === 'textarea') return;
                    rememberScroll();
                    location.reload();
                }, secs * 1000);
            }
        };
        auto.addEventListener('change', () => {
            store.set('dashd:autorefresh', auto.value);
            arm();
            toast(Number(auto.value) ? 'Auto-refresh every ' + auto.options[auto.selectedIndex].text.replace('Every ', '') : 'Auto-refresh off');
        });
        arm();
    }

    // ------------------------------------------------------------ scroll memory
    // Applying a filter is a navigation; remembering the scroll position makes
    // it feel like an in-place update rather than a jump to the top.
    function rememberScroll() {
        try { sessionStorage.setItem('dashd:scroll', String(window.scrollY || 0)); } catch (_) {}
    }
    try {
        const y = Number(sessionStorage.getItem('dashd:scroll'));
        sessionStorage.removeItem('dashd:scroll');
        if (y > 0 && window.scrollTo) {
            requestAnimationFrame(() => window.scrollTo(0, y));
        }
    } catch (_) {}
    const form = document.getElementById('filter-form');
    if (form) form.addEventListener('submit', rememberScroll);
    document.addEventListener('click', e => {
        const t = e.target;
        if (t && t.closest && (t.closest('[data-date-preset]') || t.closest('[data-remove-filter-key]'))) {
            rememberScroll();
        }
    }, true);

    // ------------------------------------------------------------ saved views
    const savedSel = document.getElementById('saved-views');
    const saveBtn = document.getElementById('save-view');
    const delBtn = document.getElementById('delete-view');
    function loadViews() {
        try { return JSON.parse(store.get('dashd:views', '{}')) || {}; } catch (_) { return {}; }
    }
    function paintViews() {
        if (!savedSel) return;
        const views = loadViews();
        const here = location.search.replace(/^\?/, '');
        savedSel.innerHTML = '<option value="">Saved views…</option>' + Object.keys(views).sort().map(name =>
            `<option value="${escapeHtml(name)}"${views[name] === here ? ' selected' : ''}>${escapeHtml(name)}</option>`
        ).join('');
        if (delBtn) delBtn.hidden = !savedSel.value;
    }
    if (savedSel) {
        paintViews();
        savedSel.addEventListener('change', () => {
            const views = loadViews();
            if (!savedSel.value) { if (delBtn) delBtn.hidden = true; return; }
            const qs = views[savedSel.value] || '';
            rememberScroll();
            location.href = qs ? '/?' + qs : '/';
        });
    }
    if (saveBtn) {
        saveBtn.addEventListener('click', () => {
            const name = (window.prompt('Name this view (filters and date range are saved in this browser):') || '').trim();
            if (!name) return;
            const views = loadViews();
            const q = new URLSearchParams(location.search);
            ['aw', 'am', 'as'].forEach(k => q.delete(k));
            views[name] = q.toString();
            store.set('dashd:views', JSON.stringify(views));
            paintViews();
            toast('Saved view “' + name + '”');
        });
    }
    if (delBtn) {
        delBtn.addEventListener('click', () => {
            const views = loadViews();
            if (!savedSel.value) return;
            delete views[savedSel.value];
            store.set('dashd:views', JSON.stringify(views));
            paintViews();
            toast('Deleted saved view');
        });
    }

    // ------------------------------------------------------------ export links
    // Every Export link carries the page's current filter, so a download is the
    // whole filtered set, not just the rows on screen.
    document.querySelectorAll('[data-export]').forEach(a => {
        const params = filterQuery();
        params.set('kind', a.dataset.export);
        params.set('format', 'csv');
        a.setAttribute('href', '/api/export?' + params.toString());
        a.setAttribute('download', '');
    });

    // ------------------------------------------------------------ timezone
    const tz = document.getElementById('tz-badge');
    if (tz && data.timezone) tz.textContent = data.timezone;

    // ------------------------------------------------------------ comparison
    const cmpEl = document.getElementById('comparison-content');
    const cmpBadge = document.getElementById('comparison-badge');
    const cmp = data.comparison;
    if (cmpEl && cmp && cmp.current && cmp.prior) {
        const delta = (cur, prior) => {
            if (!prior) return cur ? '<span class="burn-flat">new</span>' : '<span class="burn-flat">–</span>';
            const pct = (cur - prior) / prior * 100;
            const cls = pct > 0 ? 'burn-up' : pct < 0 ? 'burn-down' : 'burn-flat';
            return `<span class="${cls}">${pct > 0 ? '+' : ''}${pct.toFixed(0)}%</span>`;
        };
        if (cmpBadge) cmpBadge.textContent = `${cmp.currentFrom} → ${cmp.currentTo} vs ${cmp.priorFrom} → ${cmp.priorTo}`;
        const row = (label, key, fmt) => `<tr><td>${label}</td><td class="numeric">${fmt(cmp.current[key])}</td>` +
            `<td class="numeric">${fmt(cmp.prior[key])}</td><td class="numeric">${delta(cmp.current[key], cmp.prior[key])}</td></tr>`;
        const n = v => Number(v || 0).toLocaleString();
        const models = (cmp.models || []).map(m =>
            `<tr><td>${escapeHtml(m.name)}</td><td class="numeric">${money(m.current)}</td><td class="numeric">${money(m.prior)}</td>` +
            `<td class="numeric"><span class="${m.delta > 0 ? 'burn-up' : m.delta < 0 ? 'burn-down' : 'burn-flat'}">${m.delta > 0 ? '+' : ''}${money(m.delta)}</span></td></tr>`
        ).join('');
        cmpEl.innerHTML = `
            <div class="comparison-grid">
              <table class="comparison-table">
                <thead><tr><th>${escapeHtml(cmp.label)}</th><th class="numeric">This period</th><th class="numeric">Previous</th><th class="numeric">Change</th></tr></thead>
                <tbody>
                  ${row('Cost', 'cost', v => money(v))}
                  ${row('Calls', 'calls', n)}
                  ${row('Sessions', 'sessions', n)}
                  ${row('Tokens', 'tokens', n)}
                  ${row('Cost per call', 'per_call', v => money(v, 4))}
                </tbody>
              </table>
              ${models ? `<table class="comparison-table">
                <thead><tr><th>Biggest movers</th><th class="numeric">This period</th><th class="numeric">Previous</th><th class="numeric">Change</th></tr></thead>
                <tbody>${models}</tbody></table>` : ''}
            </div>`;
    } else if (cmpEl) {
        const section = document.getElementById('comparison-section');
        if (section) section.hidden = true;
    }

    // ------------------------------------------------------------ tags
    const tagsBody = document.getElementById('tags-tbody');
    if (tagsBody) {
        const tags = data.tags || [];
        tagsBody.innerHTML = tags.length ? tags.map(t => {
            const link = t.tag === '(untagged)' ? escapeHtml(t.tag)
                : `<a href="/?tag=${encodeURIComponent(t.tag)}">${escapeHtml(t.tag)}</a>`;
            return `<tr><td>${link}</td><td class="numeric">${t.projects}</td><td class="numeric">${t.sessions}</td>` +
                `<td class="numeric">${Number(t.calls).toLocaleString()}</td><td class="numeric cost">${money(t.cost)}</td>` +
                `<td class="numeric">${money(t.per_session)}</td><td class="numeric">${Number(t.pct || 0).toFixed(1)}%</td></tr>`;
        }).join('') : '<tr><td colspan="7" class="empty-cell">No tagged projects in this view.</td></tr>';
    }

    // ------------------------------------------------------------ shortcuts dialog
    const dialog = document.getElementById('shortcuts-dialog');
    const helpBtn = document.getElementById('shortcuts-help');
    let lastFocus = null;
    function showShortcuts() {
        if (!dialog) return;
        lastFocus = document.activeElement;
        dialog.hidden = false;
        const close = dialog.querySelector('[data-close-shortcuts]');
        if (close && close.focus) close.focus();
    }
    function hideShortcuts() {
        if (!dialog || dialog.hidden) return;
        dialog.hidden = true;
        if (lastFocus && lastFocus.focus) lastFocus.focus();
    }
    window.showDashdShortcuts = showShortcuts;
    if (helpBtn) helpBtn.addEventListener('click', showShortcuts);
    if (dialog) {
        dialog.addEventListener('click', e => {
            if (e.target === dialog || (e.target.closest && e.target.closest('[data-close-shortcuts]'))) hideShortcuts();
        });
    }
    document.addEventListener('keydown', e => { if (e.key === 'Escape') hideShortcuts(); });

    // ------------------------------------------------------------ admin actions
    const reprice = document.getElementById('reprice-btn');
    if (reprice) {
        reprice.addEventListener('click', async () => {
            if (!window.confirm('Recompute the cost of every stored call from the current price table?')) return;
            reprice.disabled = true;
            const started = Date.now();
            const tick = setInterval(() => {
                reprice.textContent = 'Repricing… ' + Math.round((Date.now() - started) / 1000) + 's';
            }, 500);
            try {
                const res = await dashdPost('/api/reprice');
                toast('Repriced: ' + (res.repriced || 0) + ' calls changed');
                setTimeout(() => location.reload(), 900);
            } catch (err) {
                toast('Reprice failed: ' + err.message);
                reprice.disabled = false;
            } finally {
                clearInterval(tick);
                reprice.textContent = 'Reprice all calls';
            }
        });
    }
    const pruneForm = document.getElementById('admin-prune-form');
    if (pruneForm) {
        pruneForm.addEventListener('submit', async e => {
            e.preventDefault();
            const before = document.getElementById('prune-before').value;
            const vacuum = document.getElementById('prune-vacuum').checked;
            if (!before || !window.confirm('Permanently delete every call before ' + before + '? Back up first: this cannot be undone.')) return;
            try {
                const res = await dashdPost('/api/admin/prune', { before, vacuum });
                toast('Pruned ' + (res.pruned || 0) + ' calls');
                setTimeout(() => location.reload(), 900);
            } catch (err) {
                toast('Prune failed: ' + err.message);
            }
        });
    }
    const renameForm = document.getElementById('admin-rename-form');
    if (renameForm) {
        renameForm.addEventListener('submit', async e => {
            e.preventDefault();
            const from = document.getElementById('rename-from').value;
            const to = document.getElementById('rename-to').value.trim();
            if (!from || !to || !window.confirm('Move every row from “' + from + '” to “' + to + '”?')) return;
            try {
                const res = await dashdPost('/api/admin/rename-machine', { from, to });
                toast('Renamed: ' + (res.calls || 0) + ' calls moved');
                setTimeout(() => location.reload(), 900);
            } catch (err) {
                toast('Rename failed: ' + err.message);
            }
        });
    }
})();
