// tables.js — sorting for every table dashboard.js does not sort itself.
//
// Those tables are drawn from data and sort it (th[data-sort]); the rest are
// rows already on the page, so they are sorted as rows. A cell is compared by
// its data-sort-value when it has one, otherwise by what its text means: money,
// 12.3k, 1h02m, "5m ago" and 41% as quantities, everything else as text.
(function () {
    'use strict';
    if (typeof document === 'undefined' || !document.querySelectorAll || typeof MutationObserver !== 'function') return;

    const SUFFIX = { k: 1e3, K: 1e3, M: 1e6, B: 1e9, T: 1e12 };
    const UNIT = { ms: 0.001, s: 1, m: 60, h: 3600, d: 86400 };
    const DURATION = /^\+?(\d+(?:\.\d+)?(?:ms|[smhd])\s*)+(ago)?$/;

    // A number, or null when the text is not a quantity.
    function quantity(text) {
        const t = text.trim();
        if (t === 'just now' || t === 'live') return 0;
        if (DURATION.test(t)) {
            let total = 0;
            t.replace(/(\d+(?:\.\d+)?)(ms|[smhd])/g, (_, n, u) => { total += Number(n) * UNIT[u]; return ''; });
            return total;
        }
        const m = /^([+-]?)(-?)\$?([\d,]+(?:\.\d+)?)\s*([kKMBT]?)(?:%|×| tok\/s)?$/.exec(t);
        if (!m) return null;
        const n = Number(m[3].replace(/,/g, '')) * (SUFFIX[m[4]] || 1);
        return m[1] === '-' || m[2] === '-' ? -n : n;
    }

    function keyOf(row, col) {
        const cell = row.cells[col];
        if (!cell) return { n: null, s: '' };
        const raw = cell.dataset.sortValue !== undefined ? cell.dataset.sortValue : cell.textContent;
        const s = raw.replace(/\s+/g, ' ').trim();
        return { n: quantity(s), s: s.toLowerCase() };
    }

    // Blank and dash cells go last whichever way the column is sorted.
    const blank = k => k.s === '' || k.s === '–' || k.s === '—' || k.s === '-' || k.s === 'unknown' || k.s === 'never';

    function compare(a, b) {
        if (a.n !== null && b.n !== null) return a.n - b.n;
        if (a.n !== null) return -1;
        if (b.n !== null) return 1;
        return a.s < b.s ? -1 : a.s > b.s ? 1 : 0;
    }

    const chosen = {}; // table id -> {col, asc}, so a refreshed table stays sorted

    function enhance(table) {
        if (table.dataset.sortable || table.querySelector('th[data-sort]')) return;
        const heads = Array.from(table.querySelectorAll('thead th'));
        const body = table.tBodies[0];
        if (!heads.length || !body) return;
        table.dataset.sortable = '1';
        let state = chosen[table.id] || null;

        const observer = new MutationObserver(() => apply());
        function apply() {
            heads.forEach((th, i) => {
                const icon = th.querySelector('.sort-icon');
                if (!icon) return;
                const on = !!state && state.col === i;
                th.classList.toggle('sorted', on);
                icon.textContent = on && state.asc ? '▲' : '▼';
                th.setAttribute('aria-sort', on ? (state.asc ? 'ascending' : 'descending') : 'none');
            });
            if (!state) return;
            observer.disconnect();
            const rows = Array.from(body.rows).map((row, i) => ({
                row, i, fixed: row.cells.length === 1 && row.cells[0].colSpan > 1, key: keyOf(row, state.col),
            }));
            rows.sort((a, b) => {
                if (a.fixed !== b.fixed) return a.fixed ? 1 : -1;
                const ab = blank(a.key), bb = blank(b.key);
                if (ab !== bb) return ab ? 1 : -1;
                const c = compare(a.key, b.key);
                return c ? (state.asc ? c : -c) : a.i - b.i;
            });
            rows.forEach(r => body.appendChild(r.row));
            observer.observe(body, { childList: true });
        }

        heads.forEach((th, col) => {
            // A heading with no words is a column of buttons.
            if (!th.textContent.trim()) return;
            const btn = document.createElement('button');
            btn.type = 'button';
            btn.className = 'sort-btn';
            while (th.firstChild) btn.appendChild(th.firstChild);
            const icon = document.createElement('span');
            icon.className = 'sort-icon';
            icon.textContent = '▼';
            btn.appendChild(document.createTextNode(' '));
            btn.appendChild(icon);
            th.appendChild(btn);
            btn.addEventListener('click', () => {
                if (state && state.col === col) state = { col, asc: !state.asc };
                // Quantities and times start with the largest, names with A.
                else state = { col, asc: !(th.classList.contains('numeric') || /^(when|time|window|from|date|day|used)$/i.test(th.textContent.replace(/[▲▼]/g, '').trim())) };
                if (table.id) chosen[table.id] = state;
                apply();
            });
        });
        apply();
        observer.observe(body, { childList: true });
    }

    function enhanceAll() { document.querySelectorAll('table').forEach(enhance); }
    enhanceAll();
    if (typeof dashdRefreshers !== 'undefined') dashdRefreshers.push(enhanceAll);
})();
