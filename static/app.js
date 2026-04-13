/* ═══════════════════════════════════════════════════════════════
   ReplicationDash v0.2.0 — Frontend Application
   Topology pipeline, status cards from backend, root cause
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
        setupCardDetailPanel();
        setupColumnResize();
        setupSectionToggles();
        fetchData();
        startRefresh();
    }

    // ── Refresh ────────────────────────────────────────────────
    function setupRefreshControl() {
        $('#refresh-select').addEventListener('change', function () {
            refreshInterval = parseInt(this.value);
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
        $('#server-select').addEventListener('change', async function () {
            if (!this.value) return;
            try {
                const r = await fetch('/api/switch?name=' + encodeURIComponent(this.value));
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

    // ── Section Toggles ────────────────────────────────────────
    function setupSectionToggles() {
        $$('.section-header').forEach(hdr => {
            hdr.addEventListener('click', () => {
                const body = document.getElementById(hdr.dataset.toggle);
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
            updateRootCause(data.root_cause);
            updateTimestamp(data.timestamp);
            updateBadges(data);
            renderTopology(data.topology, data.cards);
            renderStatusCards(data.cards);
            renderSessions(data.sessions);
            renderConflicts(data.conflicts);
            renderBlocking(data.blocking);
        } catch (e) {
            console.error('Fetch error:', e);
        }
    }

    function showNoServers(show) {
        const msg = $('#no-servers-msg');
        const sections = [
            $('#topology-section'),
            $('#status-cards'),
            ...$$('.detail-section')
        ];
        if (show) {
            msg.classList.remove('hidden');
            sections.forEach(s => { if (s) s.classList.add('hidden'); });
        } else {
            msg.classList.add('hidden');
            sections.forEach(s => { if (s) s.classList.remove('hidden'); });
        }
    }

    // ── Health Ampel ───────────────────────────────────────────
    function updateHealth(health) {
        const el = $('#health-indicator');
        el.querySelector('.health-text').textContent = health.summary;
        el.dataset.level = health.level;

        let html = '<div class="tt-title">' + esc(health.summary) + '</div>';
        (health.details || []).forEach(d => {
            html += '<div class="tt-item">' + esc(d) + '</div>';
        });
        $('#health-tooltip').innerHTML = html;
    }

    // ── Root Cause Indicator ───────────────────────────────────
    function updateRootCause(rc) {
        const el = $('#root-cause-indicator');
        if (!rc) {
            el.classList.add('hidden');
            return;
        }
        el.classList.remove('hidden');
        el.dataset.type = rc.type;
        el.querySelector('.rc-icon').textContent = rc.icon;
        el.querySelector('.rc-label').textContent = rc.label;
        el.querySelector('.rc-tooltip').textContent = rc.explanation;
    }

    function updateTimestamp(ts) {
        $('#timestamp').textContent = ts || '';
    }

    function updateBadges(data) {
        const bS = $('#badge-sessions');
        const bC = $('#badge-conflicts');
        const bB = $('#badge-blocking');
        bS.textContent = data.session_count || 0;
        bC.textContent = data.conflict_count || 0;
        bB.textContent = data.blocking_count || 0;
        bS.className = 'badge' + (data.failed_sessions > 0 ? ' crit' : '');
        bC.className = 'badge' + (data.conflict_count > 5 ? ' warn' : data.conflict_count > 0 ? ' warn' : '');
        bB.className = 'badge' + (data.blocking_count > 0 ? ' crit' : '');
    }

    // ═══════════════════════════════════════════════════════════
    // TOPOLOGY PIPELINE
    // ═══════════════════════════════════════════════════════════

    function renderTopology(topo, cards) {
        const container = $('#topology-pipeline');
        if (!topo || !topo.nodes || topo.nodes.length === 0) {
            container.innerHTML = '';
            return;
        }

        const nodes = [...topo.nodes].sort((a, b) => a.weight - b.weight);
        const links = topo.links || [];

        // Compute per-node status from cards
        const nodeStatus = {};
        nodes.forEach(n => { nodeStatus[n.server_name] = 'green'; });

        (cards || []).forEach(c => {
            if (c.level === 'red') {
                nodeStatus[c.subscriber] = 'red';
            } else if (c.level === 'yellow') {
                nodeStatus[c.subscriber] = worstLevel(nodeStatus[c.subscriber], 'yellow');
            }
        });

        // Publisher gets worst of all subscribers
        const pubNode = nodes.find(n => n.role === 'publisher_distributor' || n.role === 'publisher');
        const subNodes = nodes.filter(n => n.role === 'subscriber');

        if (pubNode) {
            let pubStatus = 'green';
            subNodes.forEach(s => {
                pubStatus = worstLevel(pubStatus, nodeStatus[s.server_name] || 'green');
            });
            nodeStatus[pubNode.server_name] = pubStatus;
        }

        const roleIcons = {
            'publisher_distributor': '🖥️',
            'publisher': '📤',
            'distributor': '🔀',
            'subscriber': '📥'
        };

        // Star layout: publisher on top, subscribers below
        let html = '<div class="topo-star">';

        // Publisher (center top)
        if (pubNode) {
            const st = nodeStatus[pubNode.server_name] || 'green';
            html += '<div class="topo-star-hub">';
            html += renderTopoNode(pubNode, st, roleIcons);
            html += '</div>';
        }

        // Subscriber row
        html += '<div class="topo-star-spokes">';
        subNodes.forEach(sub => {
            const st = nodeStatus[sub.server_name] || 'green';

            // Determine arrow statuses from cards for this subscriber
            let uploadStatus = 'ok';
            let downloadStatus = 'ok';
            (cards || []).forEach(c => {
                if (c.subscriber === sub.server_name) {
                    uploadStatus = worstArrowStatus(uploadStatus, c.upload_status);
                    downloadStatus = worstArrowStatus(downloadStatus, c.download_status);
                }
            });

            html += '<div class="topo-spoke">';

            // Vertical arrows
            html += '<div class="topo-arrows-vertical">';
            html += '<div class="topo-arrow-v" data-status="' + downloadStatus + '">';
            html += '<span class="topo-arrow-label">▼ Down</span>';
            html += '<div class="topo-arrow-line-v"></div>';
            html += '</div>';
            html += '<div class="topo-arrow-v" data-status="' + uploadStatus + '">';
            html += '<div class="topo-arrow-line-v"></div>';
            html += '<span class="topo-arrow-label">▲ Up</span>';
            html += '</div>';
            html += '</div>';

            // Subscriber node
            html += renderTopoNode(sub, st, roleIcons);
            html += '</div>';
        });
        html += '</div>';

        html += '</div>';
        container.innerHTML = html;
    }

    function renderTopoNode(node, status, icons) {
        const icon = icons[node.role] || '🖥️';
        return '<div class="topo-node" data-status="' + status + '">' +
            '<div class="topo-node-status-dot ' + status + '"></div>' +
            '<div class="topo-node-icon">' + icon + '</div>' +
            '<div class="topo-node-name">' + esc(node.server_name) + '</div>' +
            '<div class="topo-node-role">' + esc(node.role_label) + '</div>' +
            '</div>';
    }

    function worstLevel(a, b) {
        const order = { green: 0, yellow: 1, red: 2 };
        return (order[b] || 0) > (order[a] || 0) ? b : a;
    }

    function worstArrowStatus(a, b) {
        const order = { ok: 0, running: 1, warn: 2, error: 3 };
        return (order[b] || 0) > (order[a] || 0) ? b : a;
    }

    // ═══════════════════════════════════════════════════════════
    // STATUS CARDS (from backend-computed data)
    // ═══════════════════════════════════════════════════════════

    function renderStatusCards(cards) {
        const container = $('#cards-container');

        if (!cards || cards.length === 0) {
            container.innerHTML =
                '<div class="status-card no-data">' +
                '<span class="no-data-icon">⏸</span>' +
                '<span class="no-data-text">Keine Replikations-Topologie erkannt</span>' +
                '</div>';
            return;
        }

        let html = '';
        cards.forEach(c => {
            html += '<div class="status-card" data-level="' + c.level + '" data-pub="' + esc(c.publication) + '" data-sub="' + esc(c.subscriber) + '">';

            html += '<div class="card-header">';
            html += '<div class="card-pub-name">' + esc(c.publication) + '</div>';
            html += '<span class="card-status-badge ' + c.status_class + '">' + esc(c.status_text) + '</span>';
            html += '</div>';

            html += '<div class="card-subscriber">→ ' + esc(c.subscriber) + '</div>';

            html += '<div class="card-metrics">';

            const durClass = c.max_duration > 300 ? 'v-red' : c.max_duration > 120 ? 'v-yellow' : 'v-green';
            html += '<div class="card-metric">';
            html += '<div class="card-metric-value ' + durClass + '">' + formatDuration(c.max_duration) + '</div>';
            html += '<div class="card-metric-label">Max Dauer</div>';
            html += '</div>';

            const errClass = c.total_errors > 0 ? 'v-red' : 'v-green';
            html += '<div class="card-metric">';
            html += '<div class="card-metric-value ' + errClass + '">' + c.total_errors + '</div>';
            html += '<div class="card-metric-label">Errors</div>';
            html += '</div>';

            const confClass = c.conflict_count > 0 ? 'v-yellow' : 'v-muted';
            html += '<div class="card-metric">';
            html += '<div class="card-metric-value ' + confClass + '">' + c.conflict_count + '</div>';
            html += '<div class="card-metric-label">Konflikte</div>';
            html += '</div>';

            html += '</div>';

            if (c.last_message) {
                const msgClass = c.total_errors > 0 ? 'msg-error' : '';
                html += '<div class="card-footer"><span class="' + msgClass + '">' + esc(c.last_message) + '</span></div>';
            }

            html += '</div>';
        });

        container.innerHTML = html;

        // Click on card → open detail panel
        container.querySelectorAll('.status-card[data-pub]').forEach(el => {
            el.style.cursor = 'pointer';
            el.addEventListener('click', () => {
                openCardDetail(el.dataset.pub, el.dataset.sub);
            });
        });
    }

    function formatDuration(seconds) {
        if (!seconds) return '—';
        if (seconds < 60) return seconds + 's';
        const min = Math.floor(seconds / 60);
        const sec = seconds % 60;
        if (min < 60) return min + 'm ' + sec + 's';
        const hrs = Math.floor(min / 60);
        return hrs + 'h ' + (min % 60) + 'm';
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
            const durTrigger = (s.duration_seconds > 300) ? ' data-ts-trigger="latency_high"' : '';
            const errTrigger = (s.error_count > 0 && s.run_status === 6) ? ' data-ts-trigger="session_error"' : '';

            html += '<tr>';
            html += td(s.publication_name);
            html += td(s.subscriber);
            html += '<td class="' + statusClass + '">' + esc(s.run_status_text) + '</td>';
            html += '<td class="' + durClass + '"' + durTrigger + '>' + fmtNum(s.duration_seconds) + '</td>';
            html += td(fmtFloat(s.delivery_rate));
            html += td(fmtNum(s.upload_inserts));
            html += td(fmtNum(s.upload_updates));
            html += td(fmtNum(s.upload_deletes));
            html += td(fmtNum(s.download_inserts));
            html += td(fmtNum(s.download_updates));
            html += td(fmtNum(s.download_deletes));
            html += '<td class="' + errClass + '"' + errTrigger + '>' + fmtNum(s.error_count) + '</td>';
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
            const trigger = typeText.includes('Update') ? 'conflict_update' : typeText.includes('Delete') ? 'conflict_delete' : '';
            const cls = trigger ? 'cell-warn' : '';
            const tsAttr = trigger ? ' data-ts-trigger="' + trigger + '"' : '';
            html += '<tr>';
            html += td(c.conflict_table);
            html += td(c.origin_datasource);
            html += '<td class="' + cls + '"' + tsAttr + '>' + esc(typeText) + '</td>';
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
            const tsAttr = replRelated ? ' data-ts-trigger="blocking_repl_agent"' : '';
            html += '<tr>';
            html += td(b.blocked_spid);
            html += td(b.blocking_spid);
            html += td(b.wait_type);
            html += '<td class="' + waitClass + '"' + tsAttr + '>' + fmtNum(b.wait_time_ms) + '</td>';
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
        tbody.querySelectorAll('td').forEach(cell => {
            cell.addEventListener('click', () => {
                const trigger = cell.dataset.tsTrigger;
                if (trigger) { openTroubleshooting(trigger); return; }
                const text = cell.textContent;
                navigator.clipboard.writeText(text).then(() => {
                    cell.classList.add('copied');
                    showToast('Kopiert: ' + text.substring(0, 80));
                    setTimeout(() => cell.classList.remove('copied'), 400);
                });
            });
        });
    }

    // ── Sorting ────────────────────────────────────────────────
    function applySorting(tableId, data) {
        const state = sortState[tableId];
        if (!state) return data;
        const arr = [...data];
        arr.sort((a, b) => {
            let va = a[state.col], vb = b[state.col];
            if (va == null) va = '';
            if (vb == null) vb = '';
            if (state.isNum) {
                va = parseFloat(va) || 0;
                vb = parseFloat(vb) || 0;
                return state.dir === 'asc' ? va - vb : vb - va;
            }
            va = String(va).toLowerCase();
            vb = String(vb).toLowerCase();
            if (va < vb) return state.dir === 'asc' ? -1 : 1;
            if (va > vb) return state.dir === 'asc' ? 1 : -1;
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
                e.preventDefault(); e.stopPropagation();
                startX = e.pageX; startWidth = th.offsetWidth;
                const onMove = (e2) => { th.style.width = Math.max(40, startWidth + e2.pageX - startX) + 'px'; th.style.minWidth = th.style.width; };
                const onUp = () => { document.removeEventListener('mousemove', onMove); document.removeEventListener('mouseup', onUp); };
                document.addEventListener('mousemove', onMove);
                document.addEventListener('mouseup', onUp);
            });
        });
    }

    // ── Modal ──────────────────────────────────────────────────
    function setupModal() {
        const overlay = $('#modal-overlay');
        $('#btn-add-server').addEventListener('click', () => { clearModal(); overlay.classList.remove('hidden'); });
        $('#modal-close').addEventListener('click', () => overlay.classList.add('hidden'));
        overlay.addEventListener('click', (e) => { if (e.target === overlay) overlay.classList.add('hidden'); });

        $('#srv-auth').addEventListener('change', function () {
            $('#sql-auth-fields').classList.toggle('hidden', this.value !== 'sql');
        });

        $('#btn-save-server').addEventListener('click', async () => {
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
            const btn = $('#btn-save-server');
            btn.disabled = true; btn.textContent = 'Verbinde...';
            try {
                const r = await fetch('/api/server/add', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(sc) });
                const d = await r.json();
                if (d.error) throw new Error(d.error);
                overlay.classList.add('hidden');
                showToast('Server "' + sc.name + '" verbunden!');
                fetchData();
            } catch (e) { showModalError(e.message); }
            finally { btn.disabled = false; btn.textContent = 'Verbinden'; }
        });

        $('#btn-remove-server').addEventListener('click', async () => {
            const name = $('#srv-name').value.trim() || $('#server-select').value;
            if (!name || !confirm('Server "' + name + '" wirklich entfernen?')) return;
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

    function clearModal() {
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

    // ── Card Detail Panel ─────────────────────────────────────────
    function setupCardDetailPanel() {
        $('#cd-close').addEventListener('click', closeCardDetail);
    }

    function openCardDetail(pub, sub) {
        if (!currentData) return;

        const card = (currentData.cards || []).find(c => c.publication === pub && c.subscriber === sub);
        const sessions = (currentData.sessions || []).filter(s => s.publication_name === pub && s.subscriber === sub);
        const conflicts = (currentData.conflicts || []).filter(c => c.origin_datasource === sub);
        const blocking = (currentData.blocking || []).filter(b => {
            const prog = (b.blocked_program || '').toLowerCase();
            return prog.includes('replmerg') || prog.includes('repl-merge');
        });

        // Title + badge
        $('#cd-title').textContent = pub + ' → ' + sub;
        const badge = $('#cd-badge');
        if (card) {
            badge.textContent = card.status_text;
            badge.className = 'card-status-badge ' + card.status_class;
        }

        // Recommendation
        const recEl = $('#cd-recommendation');
        if (card && card.level !== 'green') {
            recEl.classList.remove('hidden', 'rc-ok', 'rc-warn', 'rc-crit');
            const rcLevel = card.level === 'red' ? 'rc-crit' : 'rc-warn';
            recEl.classList.add(rcLevel);
            recEl.innerHTML = buildRecommendation(card, sessions, conflicts, blocking);
        } else if (card && card.level === 'green') {
            recEl.classList.remove('hidden', 'rc-warn', 'rc-crit');
            recEl.classList.add('rc-ok');
            recEl.innerHTML = '<div class="cd-rec-title rc-ok">✓ Alles in Ordnung</div>' +
                '<div class="cd-rec-text">Keine Fehler, keine kritischen Konflikte, keine Blockaden für diese Verbindung.</div>';
        } else {
            recEl.classList.add('hidden');
        }

        // Sessions table
        const sessCount = $('#cd-session-count');
        sessCount.textContent = sessions.length;
        sessCount.className = 'badge' + (sessions.some(s => s.run_status === 6) ? ' crit' : '');
        renderCardSessions(sessions);

        // Conflicts table
        const confCount = $('#cd-conflict-count');
        confCount.textContent = conflicts.length;
        confCount.className = 'badge' + (conflicts.length >= 6 ? ' warn' : '');
        renderCardConflicts(conflicts);

        // Blocking table
        const blockCount = $('#cd-blocking-count');
        blockCount.textContent = blocking.length;
        blockCount.className = 'badge' + (blocking.length > 0 ? ' crit' : '');
        renderCardBlocking(blocking);

        // Show panel
        const panel = $('#card-detail-panel');
        panel.classList.remove('hidden');
        panel.offsetHeight;
        panel.classList.add('visible');
    }

    function closeCardDetail() {
        const panel = $('#card-detail-panel');
        panel.classList.remove('visible');
        setTimeout(() => panel.classList.add('hidden'), 250);
    }

    function buildRecommendation(card, sessions, conflicts, blocking) {
        let title = '';
        let text = '';
        const rcClass = card.level === 'red' ? 'rc-crit' : 'rc-warn';

        // Failed sessions
        const failedSessions = sessions.filter(s => s.run_status === 6);
        if (failedSessions.length > 0) {
            title = '⚠ ' + failedSessions.length + ' fehlgeschlagene Session(s)';
            const lastMsg = failedSessions[0].last_message || '';
            if (lastMsg.toLowerCase().includes('deadlock')) {
                text = 'Deadlock während der Synchronisation. Konkurrierende Zugriffe zwischen Applikation und Merge-Agent. Indizes und Zugriffsmuster prüfen.';
            } else if (lastMsg.toLowerCase().includes('timeout')) {
                text = 'Timeout – Server möglicherweise überlastet oder Netzwerk-Latenz zu hoch. Agent-Profil und QueryTimeout prüfen.';
            } else if (lastMsg.toLowerCase().includes('connect')) {
                text = 'Verbindungsfehler zum Subscriber. Netzwerk, DNS und SQL Server-Dienst auf dem Subscriber prüfen.';
            } else {
                text = 'Fehlermeldung: "' + lastMsg.substring(0, 200) + '". SQL Server Agent Job-Verlauf prüfen.';
            }
            return '<div class="cd-rec-title ' + rcClass + '">' + esc(title) + '</div><div class="cd-rec-text">' + esc(text) + '</div>';
        }

        // Retry
        const retrySessions = sessions.filter(s => s.run_status === 5);
        if (retrySessions.length > 0) {
            title = '⟳ Session im Retry-Modus';
            text = 'Der Merge-Agent versucht erneut zu synchronisieren. Wenn Retries häufen, Verbose-Logging aktivieren: replmerg.exe -OutputVerboseLevel 2';
            return '<div class="cd-rec-title ' + rcClass + '">' + esc(title) + '</div><div class="cd-rec-text">' + esc(text) + '</div>';
        }

        // Blocking
        if (blocking.length > 0) {
            const b = blocking[0];
            const prog = b.blocker_program || 'Unbekannt';
            const host = b.blocker_host || '?';
            const login = b.blocker_login || '?';
            title = '🔒 Merge-Agent wird blockiert';
            if (prog.toLowerCase().includes('management studio') || prog.toLowerCase().includes('azdata')) {
                text = 'Blockiert durch ' + prog + ' auf ' + host + ' (' + login + '). Vermutlich offene Transaktion ohne COMMIT. Kollegen kontaktieren.';
            } else {
                text = 'Blockiert durch ' + prog + ' auf ' + host + ' (' + login + '). Wartzeit: ' + Math.round((b.wait_time_ms || 0) / 1000) + 's. SQL-Text der blockierenden Session prüfen.';
            }
            return '<div class="cd-rec-title ' + rcClass + '">' + esc(title) + '</div><div class="cd-rec-text">' + esc(text) + '</div>';
        }

        // High conflicts
        if (conflicts.length >= 6) {
            title = '⚡ Erhöhte Konfliktrate: ' + conflicts.length + ' Konflikte';
            text = 'Mehrere Standorte ändern dieselben Datensätze. Datenhoheit prüfen – wer darf welche Zeilen ändern? Column-Level Tracking erwägen.';
            return '<div class="cd-rec-title ' + rcClass + '">' + esc(title) + '</div><div class="cd-rec-text">' + esc(text) + '</div>';
        }

        // High latency
        if (card.max_duration > 300) {
            title = '🐌 Hohe Latenz: ' + formatDuration(card.max_duration);
            const totalRows = card.total_uploads + card.total_downloads;
            if (totalRows > 10000) {
                text = 'Großes Datenvolumen (' + totalRows.toLocaleString('de-DE') + ' Zeilen). Massen-Operationen in Wartungsfenster verlegen oder Agent-Profil optimieren.';
            } else {
                text = 'Wenige Daten aber hohe Latenz – deutet auf I/O-Engpass, Netzwerkprobleme oder Server-Überlastung hin.';
            }
            return '<div class="cd-rec-title ' + rcClass + '">' + esc(title) + '</div><div class="cd-rec-text">' + esc(text) + '</div>';
        }

        return '';
    }

    function renderCardSessions(sessions) {
        const wrap = $('#cd-sessions');
        if (sessions.length === 0) {
            wrap.innerHTML = '<div class="cd-empty">Keine Sessions für diese Verbindung</div>';
            return;
        }
        let html = '<table class="cd-table"><thead><tr>';
        html += '<th>Status</th><th>Dauer</th><th>Up ↑</th><th>Down ↓</th><th>Errors</th><th>Start</th><th>Message</th>';
        html += '</tr></thead><tbody>';
        sessions.forEach(s => {
            const cls = getStatusClass(s.run_status_text);
            const ups = (s.upload_inserts || 0) + (s.upload_updates || 0) + (s.upload_deletes || 0);
            const downs = (s.download_inserts || 0) + (s.download_updates || 0) + (s.download_deletes || 0);
            html += '<tr>';
            html += '<td class="' + cls + '">' + esc(s.run_status_text) + '</td>';
            html += '<td>' + formatDuration(s.duration_seconds) + '</td>';
            html += '<td>' + fmtNum(ups) + '</td>';
            html += '<td>' + fmtNum(downs) + '</td>';
            html += '<td' + (s.error_count > 0 ? ' style="color:var(--accent-red);font-weight:600"' : '') + '>' + (s.error_count || 0) + '</td>';
            html += '<td>' + esc(s.start_time) + '</td>';
            html += '<td>' + esc(s.last_message) + '</td>';
            html += '</tr>';
        });
        html += '</tbody></table>';
        wrap.innerHTML = html;
        attachCellHandlers(wrap);
    }

    function renderCardConflicts(conflicts) {
        const wrap = $('#cd-conflicts');
        if (conflicts.length === 0) {
            wrap.innerHTML = '<div class="cd-empty">Keine Konflikte für diesen Subscriber</div>';
            return;
        }
        let html = '<table class="cd-table"><thead><tr>';
        html += '<th>Tabelle</th><th>Typ</th><th>Reason</th><th>Zeitpunkt</th>';
        html += '</tr></thead><tbody>';
        conflicts.forEach(c => {
            html += '<tr>';
            html += '<td>' + esc(c.conflict_table) + '</td>';
            html += '<td>' + esc(c.conflict_type_text) + '</td>';
            html += '<td>' + esc(c.reason_text) + '</td>';
            html += '<td>' + esc(c.create_time) + '</td>';
            html += '</tr>';
        });
        html += '</tbody></table>';
        wrap.innerHTML = html;
        attachCellHandlers(wrap);
    }

    function renderCardBlocking(blocking) {
        const wrap = $('#cd-blocking');
        if (blocking.length === 0) {
            wrap.innerHTML = '<div class="cd-empty">Keine Blockaden für Replikations-Agenten</div>';
            return;
        }
        let html = '<table class="cd-table"><thead><tr>';
        html += '<th>Blocked</th><th>Blocker</th><th>Wait</th><th>Blocker Prog</th><th>Host</th><th>Login</th><th>SQL</th>';
        html += '</tr></thead><tbody>';
        blocking.forEach(b => {
            html += '<tr>';
            html += '<td>SPID ' + (b.blocked_spid || '') + '</td>';
            html += '<td>SPID ' + (b.blocking_spid || '') + '</td>';
            html += '<td>' + fmtNum(b.wait_time_ms) + ' ms</td>';
            html += '<td>' + esc(b.blocker_program) + '</td>';
            html += '<td>' + esc(b.blocker_host) + '</td>';
            html += '<td>' + esc(b.blocker_login) + '</td>';
            html += '<td>' + esc(b.blocker_sql_text) + '</td>';
            html += '</tr>';
        });
        html += '</tbody></table>';
        wrap.innerHTML = html;
        attachCellHandlers(wrap);
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
        const ul = $('#ts-causes'); ul.innerHTML = '';
        (entry.causes || []).forEach(c => { const li = document.createElement('li'); li.textContent = c; ul.appendChild(li); });
        const ol = $('#ts-solutions'); ol.innerHTML = '';
        (entry.solutions || []).forEach(s => { const li = document.createElement('li'); li.textContent = s; ol.appendChild(li); });
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
        setTimeout(() => { toast.classList.remove('visible'); setTimeout(() => toast.classList.add('hidden'), 200); }, 2000);
    }

    // ── Helpers ────────────────────────────────────────────────
    function td(val) { return '<td>' + esc(val) + '</td>'; }
    function esc(v) { if (v == null) return ''; return String(v).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'); }
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
