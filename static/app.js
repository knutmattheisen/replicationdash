/* ═══════════════════════════════════════════════════════════════
   MergeDash v0.1 — Frontend Application
   Status cards on top, detail tables always visible below
   ═══════════════════════════════════════════════════════════════ */

(function () {
    'use strict';

    let refreshTimer = null;
    let refreshInterval = 10;
    let currentData = null;
    let sortState = {};
    let troubleshootingData = [];

    const $ = (sel) => document.querySelector(sel);
    const $$ = (sel) => document.querySelectorAll(sel);

    document.addEventListener('DOMContentLoaded', init);

    function init() {
        setupRefreshControl();
        setupServerControl();
        setupModal();
        setupTroubleshootingPanel();
        setupColumnResize();
        setupSectionToggles();
        fetchData();
        startRefresh();
    }

    // ── Refresh ────────────────────────────────────────────────
    function setupRefreshControl() {
        const sel = $('#refresh-select');
        sel.addEventListener('change', () => {
            refreshInterval = parseInt(sel.value);
            startRefresh();
        });
    }

    function startRefresh() {
        if (refreshTimer) clearInterval(refreshTimer);
        if (refreshInterval > 0) {
            refreshTimer = setInterval(fetchData, refreshInterval * 1000);
        }
    }

    // ── Server Control ─────────────────────────────────────────
    function setupServerControl() {
        const sel = $('#server-select');
        sel.addEventListener('change', async () => {
            const name = sel.value;
            if (!name) return;
            try {
                const r = await fetch('/api/switch?name=' + encodeURIComponent(name));
                const d = await r.json();
                if (d.error) throw new Error(d.error);
                fetchData();
            } catch (e) {
                showToast('Fehler: ' + e.message);
            }
        });
    }

    function updateServerDropdown(servers, current) {
        const sel = $('#server-select');
        sel.innerHTML = '';
        if (!servers || servers.length === 0) {
            sel.innerHTML = '<option value="">— Kein Server —</option>';
            return;
        }
        servers.forEach(name => {
            const opt = document.createElement('option');
            opt.value = name;
            opt.textContent = name;
            if (name === current) opt.selected = true;
            sel.appendChild(opt);
        });
    }

    // ── Collapsible Sections ───────────────────────────────────
    function setupSectionToggles() {
        $$('.section-header').forEach(hdr => {
            hdr.addEventListener('click', () => {
                const targetId = hdr.dataset.toggle;
                const body = document.getElementById(targetId);
                if (!body) return;
                hdr.classList.toggle('collapsed');
                body.classList.toggle('collapsed');
            });
        });
    }

    // ── Data Fetch ─────────────────────────────────────────────
    async function fetchData() {
        try {
            const r = await fetch('/api/all');
            const data = await r.json();

            if (data.no_servers) {
                showNoServers(true);
                updateServerDropdown([], '');
                return;
            }
            if (data.error) {
                showToast('Fehler: ' + data.error);
                return;
            }

            showNoServers(false);
            currentData = data;
            troubleshootingData = data.troubleshooting || [];

            updateServerDropdown(data.available_servers, data.current_server);
            updateHealth(data.health);
            updateTimestamp(data.timestamp);
            updateBadges(data);
            renderStatusCards(data);
            renderSessions(data.sessions);
            renderConflicts(data.conflicts);
            renderBlocking(data.blocking);
        } catch (e) {
            console.error('Fetch error:', e);
        }
    }

    function showNoServers(show) {
        const msg = $('#no-servers-msg');
        const cards = $('#status-cards');
        const sections = $$('.detail-section');
        if (show) {
            msg.classList.remove('hidden');
            cards.classList.add('hidden');
            sections.forEach(s => s.classList.add('hidden'));
        } else {
            msg.classList.add('hidden');
            cards.classList.remove('hidden');
            sections.forEach(s => s.classList.remove('hidden'));
        }
    }

    // ── Health Ampel ───────────────────────────────────────────
    function updateHealth(health) {
        const el = $('#health-indicator');
        const text = el.querySelector('.health-text');
        const tooltip = $('#health-tooltip');
        el.dataset.level = health.level;
        text.textContent = health.summary;

        let html = '<div class="tt-title">' + escHtml(health.summary) + '</div>';
        (health.details || []).forEach(d => {
            html += '<div class="tt-item">' + escHtml(d) + '</div>';
        });
        tooltip.innerHTML = html;
    }

    function updateTimestamp(ts) {
        $('#timestamp').textContent = ts || '';
    }

    function updateBadges(data) {
        const bSess = $('#badge-sessions');
        const bConf = $('#badge-conflicts');
        const bBlock = $('#badge-blocking');

        bSess.textContent = data.session_count || 0;
        bConf.textContent = data.conflict_count || 0;
        bBlock.textContent = data.blocking_count || 0;

        bSess.className = 'badge' + (data.failed_sessions > 0 ? ' crit' : '');
        bConf.className = 'badge' + (data.conflict_count > 5 ? ' warn' : data.conflict_count > 0 ? ' warn' : '');
        bBlock.className = 'badge' + (data.blocking_count > 0 ? ' crit' : '');
    }

    // ═══════════════════════════════════════════════════════════
    // STATUS CARDS — aggregated per publication + subscriber
    // ═══════════════════════════════════════════════════════════

    function renderStatusCards(data) {
        const container = $('#cards-container');
        const sessions = data.sessions || [];
        const conflicts = data.conflicts || [];
        const blocking = data.blocking || [];

        if (sessions.length === 0) {
            container.innerHTML =
                '<div class="status-card no-data">' +
                '<span class="no-data-icon">⏸</span>' +
                '<span class="no-data-text">Keine Merge-Sessions in den letzten 60 Minuten</span>' +
                '</div>';
            return;
        }

        // Group sessions by publication + subscriber
        const groups = {};
        sessions.forEach(s => {
            const key = (s.publication_name || '?') + ' → ' + (s.subscriber || '?');
            if (!groups[key]) {
                groups[key] = {
                    pub: s.publication_name || '?',
                    sub: s.subscriber || '?',
                    sessions: [],
                    lastStatus: null,
                    lastDuration: 0,
                    maxDuration: 0,
                    totalErrors: 0,
                    totalUploads: 0,
                    totalDownloads: 0,
                    lastMessage: '',
                    lastStart: ''
                };
            }
            const g = groups[key];
            g.sessions.push(s);
            g.totalErrors += (s.error_count || 0);
            g.totalUploads += (s.upload_inserts || 0) + (s.upload_updates || 0) + (s.upload_deletes || 0);
            g.totalDownloads += (s.download_inserts || 0) + (s.download_updates || 0) + (s.download_deletes || 0);

            const dur = s.duration_seconds || 0;
            if (dur > g.maxDuration) g.maxDuration = dur;

            // Track the most recent session
            if (!g.lastStart || s.start_time > g.lastStart) {
                g.lastStart = s.start_time;
                g.lastStatus = s.run_status;
                g.lastDuration = dur;
                g.lastMessage = s.last_message || '';
            }
        });

        // Count conflicts per origin
        const conflictCounts = {};
        conflicts.forEach(c => {
            const origin = c.origin_datasource || '?';
            conflictCounts[origin] = (conflictCounts[origin] || 0) + 1;
        });

        // Count blocking per subscriber-related program
        const blockingCount = blocking.length;

        // Build cards
        let html = '';
        const sortedKeys = Object.keys(groups).sort();

        sortedKeys.forEach(key => {
            const g = groups[key];
            const level = cardLevel(g, conflictCounts, blockingCount);
            const statusInfo = cardStatusInfo(g);
            const conflictsForSub = conflictCounts[g.sub] || 0;

            html += '<div class="status-card" data-level="' + level + '">';

            // Header: pub name + status badge
            html += '<div class="card-header">';
            html += '<div class="card-pub-name">' + escHtml(g.pub) + '</div>';
            html += '<span class="card-status-badge ' + statusInfo.cls + '">' + statusInfo.text + '</span>';
            html += '</div>';

            // Subscriber
            html += '<div class="card-subscriber">→ ' + escHtml(g.sub) + '</div>';

            // Metrics
            html += '<div class="card-metrics">';

            // Duration
            const durClass = g.maxDuration > 300 ? 'v-red' : g.maxDuration > 120 ? 'v-yellow' : 'v-green';
            html += '<div class="card-metric">';
            html += '<div class="card-metric-value ' + durClass + '">' + formatDuration(g.maxDuration) + '</div>';
            html += '<div class="card-metric-label">Max Dauer</div>';
            html += '</div>';

            // Errors
            const errClass = g.totalErrors > 0 ? 'v-red' : 'v-green';
            html += '<div class="card-metric">';
            html += '<div class="card-metric-value ' + errClass + '">' + g.totalErrors + '</div>';
            html += '<div class="card-metric-label">Errors</div>';
            html += '</div>';

            // Conflicts
            const confClass = conflictsForSub > 0 ? 'v-yellow' : 'v-muted';
            html += '<div class="card-metric">';
            html += '<div class="card-metric-value ' + confClass + '">' + conflictsForSub + '</div>';
            html += '<div class="card-metric-label">Konflikte</div>';
            html += '</div>';

            html += '</div>'; // card-metrics

            // Footer: last message
            if (g.lastMessage) {
                const msgClass = g.totalErrors > 0 ? 'msg-error' : '';
                html += '<div class="card-footer"><span class="' + msgClass + '">' + escHtml(g.lastMessage) + '</span></div>';
            }

            html += '</div>'; // status-card
        });

        container.innerHTML = html;
    }

    function cardLevel(group, conflictCounts, blockingCount) {
        // Red: failed session, or errors
        if (group.lastStatus === 6 || group.totalErrors > 0) return 'red';
        // Red: blocking active
        if (blockingCount > 0) return 'red';
        // Yellow: high latency or retrying
        if (group.maxDuration > 300 || group.lastStatus === 5) return 'yellow';
        // Yellow: conflicts
        const confCount = conflictCounts[group.sub] || 0;
        if (confCount > 0) return 'yellow';
        // Green
        return 'green';
    }

    function cardStatusInfo(group) {
        const st = group.lastStatus;
        if (st === 6) return { text: 'FAILED', cls: 's-crit' };
        if (st === 5) return { text: 'RETRY', cls: 's-warn' };
        if (st === 3 || st === 1) return { text: 'RUNNING', cls: 's-running' };
        if (group.totalErrors > 0) return { text: 'ERRORS', cls: 's-crit' };
        if (group.maxDuration > 300) return { text: 'SLOW', cls: 's-warn' };
        if (st === 2) return { text: 'OK', cls: 's-ok' };
        return { text: 'IDLE', cls: 's-ok' };
    }

    function formatDuration(seconds) {
        if (seconds < 60) return seconds + 's';
        const min = Math.floor(seconds / 60);
        const sec = seconds % 60;
        if (min < 60) return min + 'm ' + sec + 's';
        const hrs = Math.floor(min / 60);
        const remMin = min % 60;
        return hrs + 'h ' + remMin + 'm';
    }

    // ═══════════════════════════════════════════════════════════
    // DETAIL TABLES
    // ═══════════════════════════════════════════════════════════

    function renderSessions(sessions) {
        const tbody = $('#table-sessions tbody');
        if (!sessions || sessions.length === 0) {
            tbody.innerHTML = '<tr><td colspan="14" style="text-align:center;color:var(--text-muted);padding:30px;">Keine Sessions</td></tr>';
            return;
        }
        const sorted = applySorting('table-sessions', sessions);
        let html = '';
        sorted.forEach(s => {
            const statusClass = getStatusClass(s.run_status_text);
            const durClass = (s.duration_seconds > 300) ? 'cell-warn' : '';
            const errClass = (s.error_count > 0) ? 'cell-crit' : '';
            const durClick = (s.duration_seconds > 300) ? ' data-ts-trigger="latency_high"' : '';
            const errClick = (s.error_count > 0 && s.run_status === 6) ? ' data-ts-trigger="session_error"' : '';

            html += '<tr>';
            html += td(s.publication_name);
            html += td(s.subscriber);
            html += '<td class="' + statusClass + '">' + escHtml(s.run_status_text) + '</td>';
            html += '<td class="' + durClass + '"' + durClick + '>' + fmtNum(s.duration_seconds) + '</td>';
            html += td(fmtFloat(s.delivery_rate));
            html += td(fmtNum(s.upload_inserts));
            html += td(fmtNum(s.upload_updates));
            html += td(fmtNum(s.upload_deletes));
            html += td(fmtNum(s.download_inserts));
            html += td(fmtNum(s.download_updates));
            html += td(fmtNum(s.download_deletes));
            html += '<td class="' + errClass + '"' + errClick + '>' + fmtNum(s.error_count) + '</td>';
            html += td(s.start_time);
            html += td(s.last_message);
            html += '</tr>';
        });
        tbody.innerHTML = html;
        attachCellHandlers(tbody);
    }

    function renderConflicts(conflicts) {
        const tbody = $('#table-conflicts tbody');
        if (!conflicts || conflicts.length === 0) {
            tbody.innerHTML = '<tr><td colspan="5" style="text-align:center;color:var(--text-muted);padding:30px;">Keine Konflikte</td></tr>';
            return;
        }
        const sorted = applySorting('table-conflicts', conflicts);
        let html = '';
        sorted.forEach(c => {
            const typeText = c.conflict_type_text || '';
            const isTrigger = typeText.includes('Update') ? 'conflict_update' : typeText.includes('Delete') ? 'conflict_delete' : '';
            const cls = isTrigger ? 'cell-warn' : '';
            const tsTrigger = isTrigger ? ' data-ts-trigger="' + isTrigger + '"' : '';
            html += '<tr>';
            html += td(c.conflict_table);
            html += td(c.origin_datasource);
            html += '<td class="' + cls + '"' + tsTrigger + '>' + escHtml(typeText) + '</td>';
            html += td(c.reason_text);
            html += td(c.create_time);
            html += '</tr>';
        });
        tbody.innerHTML = html;
        attachCellHandlers(tbody);
    }

    function renderBlocking(blocking) {
        const tbody = $('#table-blocking tbody');
        if (!blocking || blocking.length === 0) {
            tbody.innerHTML = '<tr><td colspan="11" style="text-align:center;color:var(--text-muted);padding:30px;">Keine Blockaden</td></tr>';
            return;
        }
        const sorted = applySorting('table-blocking', blocking);
        let html = '';
        sorted.forEach(b => {
            const replRelated = (b.replication_related === 1 || b.replication_related === true);
            const replClass = replRelated ? 'repl-yes' : 'repl-no';
            const waitClass = (b.wait_time_ms > 30000) ? 'cell-crit' : (b.wait_time_ms > 5000) ? 'cell-warn' : '';
            const tsTrigger = replRelated ? ' data-ts-trigger="blocking_repl_agent"' : '';
            html += '<tr>';
            html += td(b.blocked_spid);
            html += td(b.blocking_spid);
            html += td(b.wait_type);
            html += '<td class="' + waitClass + '"' + tsTrigger + '>' + fmtNum(b.wait_time_ms) + '</td>';
            html += td(b.blocked_program);
            html += td(b.blocker_program);
            html += td(b.blocker_host);
            html += td(b.blocker_login);
            html += td(b.blocked_sql_text);
            html += td(b.blocker_sql_text);
            html += '<td class="' + replClass + '">' + (replRelated ? 'JA' : 'Nein') + '</td>';
            html += '</tr>';
        });
        tbody.innerHTML = html;
        attachCellHandlers(tbody);
    }

    // ── Cell handlers ──────────────────────────────────────────
    function attachCellHandlers(tbody) {
        tbody.querySelectorAll('td').forEach(td => {
            td.addEventListener('click', () => {
                const trigger = td.dataset.tsTrigger;
                if (trigger) { openTroubleshooting(trigger); return; }
                const text = td.textContent;
                navigator.clipboard.writeText(text).then(() => {
                    td.classList.add('copied');
                    showToast('Kopiert: ' + text.substring(0, 80));
                    setTimeout(() => td.classList.remove('copied'), 400);
                });
            });
        });
    }

    // ── Sorting ────────────────────────────────────────────────
    function applySorting(tableId, data) {
        const state = sortState[tableId];
        if (!state) return data;
        const arr = [...data];
        const col = state.col;
        const dir = state.dir;
        const isNum = state.isNum;
        arr.sort((a, b) => {
            let va = a[col], vb = b[col];
            if (va == null) va = '';
            if (vb == null) vb = '';
            if (isNum) {
                va = parseFloat(va) || 0;
                vb = parseFloat(vb) || 0;
                return dir === 'asc' ? va - vb : vb - va;
            }
            va = String(va).toLowerCase();
            vb = String(vb).toLowerCase();
            if (va < vb) return dir === 'asc' ? -1 : 1;
            if (va > vb) return dir === 'asc' ? 1 : -1;
            return 0;
        });
        return arr;
    }

    // ── Column resize + sort ───────────────────────────────────
    function setupColumnResize() {
        $$('.data-table th').forEach(th => {
            if (th.hasAttribute('data-sortable')) {
                const arrow = document.createElement('span');
                arrow.className = 'sort-arrow';
                arrow.textContent = ' ↕';
                th.appendChild(arrow);
            }

            const handle = document.createElement('div');
            handle.className = 'col-resize';
            th.appendChild(handle);

            th.addEventListener('click', (e) => {
                if (e.target.classList.contains('col-resize')) return;
                if (!th.hasAttribute('data-sortable')) return;
                const table = th.closest('table');
                const tableId = table.id;
                const col = th.dataset.col;
                const isNum = th.dataset.type === 'number';
                const current = sortState[tableId];
                let dir = 'asc';
                if (current && current.col === col) dir = current.dir === 'asc' ? 'desc' : 'asc';
                sortState[tableId] = { col, dir, isNum };
                table.querySelectorAll('th').forEach(h => {
                    h.classList.remove('sort-asc', 'sort-desc');
                    const sa = h.querySelector('.sort-arrow');
                    if (sa) sa.textContent = ' ↕';
                });
                th.classList.add('sort-' + dir);
                const sa = th.querySelector('.sort-arrow');
                if (sa) sa.textContent = dir === 'asc' ? ' ▲' : ' ▼';
                if (currentData) {
                    if (tableId === 'table-sessions') renderSessions(currentData.sessions);
                    else if (tableId === 'table-conflicts') renderConflicts(currentData.conflicts);
                    else if (tableId === 'table-blocking') renderBlocking(currentData.blocking);
                }
            });

            let startX, startWidth;
            handle.addEventListener('mousedown', (e) => {
                e.preventDefault();
                e.stopPropagation();
                startX = e.pageX;
                startWidth = th.offsetWidth;
                const onMouseMove = (e2) => {
                    th.style.width = Math.max(40, startWidth + e2.pageX - startX) + 'px';
                    th.style.minWidth = th.style.width;
                };
                const onMouseUp = () => {
                    document.removeEventListener('mousemove', onMouseMove);
                    document.removeEventListener('mouseup', onMouseUp);
                };
                document.addEventListener('mousemove', onMouseMove);
                document.addEventListener('mouseup', onMouseUp);
            });
        });
    }

    // ── Modal ──────────────────────────────────────────────────
    function setupModal() {
        const overlay = $('#modal-overlay');
        const btnAdd = $('#btn-add-server');
        const btnClose = $('#modal-close');
        const btnSave = $('#btn-save-server');
        const btnRemove = $('#btn-remove-server');
        const authSel = $('#srv-auth');

        btnAdd.addEventListener('click', () => { clearModalFields(); overlay.classList.remove('hidden'); });
        btnClose.addEventListener('click', () => overlay.classList.add('hidden'));
        overlay.addEventListener('click', (e) => { if (e.target === overlay) overlay.classList.add('hidden'); });

        authSel.addEventListener('change', () => {
            $('#sql-auth-fields').classList.toggle('hidden', authSel.value !== 'sql');
        });

        btnSave.addEventListener('click', async () => {
            const sc = {
                name: $('#srv-name').value.trim(),
                host: $('#srv-host').value.trim(),
                port: parseInt($('#srv-port').value) || 1433,
                instance: $('#srv-instance').value.trim(),
                auth_mode: $('#srv-auth').value,
                user: $('#srv-user').value.trim(),
                password: $('#srv-password').value,
                distribution_db: $('#srv-distdb').value.trim() || 'distribution',
                publication_db: $('#srv-pubdb').value.trim() || 'master',
            };
            if (!sc.name && !sc.host) { showModalError('Name oder Host ist erforderlich.'); return; }
            if (!sc.name) sc.name = sc.host;
            btnSave.disabled = true;
            btnSave.textContent = 'Verbinde...';
            try {
                const r = await fetch('/api/server/add', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(sc) });
                const d = await r.json();
                if (d.error) throw new Error(d.error);
                overlay.classList.add('hidden');
                showToast('Server "' + sc.name + '" verbunden!');
                fetchData();
            } catch (e) { showModalError(e.message); }
            finally { btnSave.disabled = false; btnSave.textContent = 'Verbinden'; }
        });

        btnRemove.addEventListener('click', async () => {
            const name = $('#srv-name').value.trim() || $('#server-select').value;
            if (!name) return;
            if (!confirm('Server "' + name + '" wirklich entfernen?')) return;
            try {
                const r = await fetch('/api/server/remove?name=' + encodeURIComponent(name));
                const d = await r.json();
                if (d.error) throw new Error(d.error);
                overlay.classList.add('hidden');
                showToast('Server entfernt.');
                fetchData();
            } catch (e) { showModalError(e.message); }
        });
    }

    function clearModalFields() {
        $('#srv-name').value = '';
        $('#srv-host').value = '';
        $('#srv-port').value = '1433';
        $('#srv-instance').value = '';
        $('#srv-auth').value = 'windows';
        $('#srv-user').value = '';
        $('#srv-password').value = '';
        $('#srv-distdb').value = 'distribution';
        $('#srv-pubdb').value = '';
        $('#sql-auth-fields').classList.add('hidden');
        $('#modal-error').classList.add('hidden');
    }

    function showModalError(msg) {
        const el = $('#modal-error');
        el.textContent = msg;
        el.classList.remove('hidden');
    }

    // ── Troubleshooting Panel ──────────────────────────────────
    function setupTroubleshootingPanel() {
        $('#ts-close').addEventListener('click', closeTroubleshooting);
    }

    function openTroubleshooting(trigger) {
        const entry = troubleshootingData.find(t => t.trigger === trigger);
        if (!entry) return;
        $('#ts-title').textContent = entry.title;
        $('#ts-problem').textContent = entry.problem;
        const causesUl = $('#ts-causes');
        causesUl.innerHTML = '';
        (entry.causes || []).forEach(c => { const li = document.createElement('li'); li.textContent = c; causesUl.appendChild(li); });
        const solOl = $('#ts-solutions');
        solOl.innerHTML = '';
        (entry.solutions || []).forEach(s => { const li = document.createElement('li'); li.textContent = s; solOl.appendChild(li); });
        const panel = $('#ts-panel');
        panel.classList.remove('hidden');
        panel.offsetHeight;
        panel.classList.add('visible');
    }

    function closeTroubleshooting() {
        const panel = $('#ts-panel');
        panel.classList.remove('visible');
        setTimeout(() => panel.classList.add('hidden'), 250);
    }

    // ── Toast ──────────────────────────────────────────────────
    function showToast(msg) {
        const toast = $('#toast');
        toast.textContent = msg;
        toast.classList.remove('hidden');
        toast.classList.add('visible');
        setTimeout(() => {
            toast.classList.remove('visible');
            setTimeout(() => toast.classList.add('hidden'), 200);
        }, 2000);
    }

    // ── Helpers ────────────────────────────────────────────────
    function td(val) { return '<td>' + escHtml(val) + '</td>'; }
    function escHtml(v) {
        if (v == null) return '';
        return String(v).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    }
    function fmtNum(v) { if (v == null) return '0'; return Number(v).toLocaleString('de-DE'); }
    function fmtFloat(v) { if (v == null) return '0,0'; return Number(v).toLocaleString('de-DE', { minimumFractionDigits: 1, maximumFractionDigits: 1 }); }
    function getStatusClass(status) {
        if (!status) return '';
        const s = status.toLowerCase();
        if (s.includes('succeeded')) return 'status-succeeded';
        if (s.includes('inprogress') || s.includes('started')) return 'status-inprogress';
        if (s.includes('retry')) return 'status-retry';
        if (s.includes('failed')) return 'status-failed';
        if (s.includes('idle')) return 'status-idle';
        return '';
    }
})();
