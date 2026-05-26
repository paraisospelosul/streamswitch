document.addEventListener('DOMContentLoaded', () => {
    // ─── Toast ───
    function toast(msg, type = 'info') {
        const t = document.createElement('div');
        t.className = `toast ${type}`;
        t.textContent = msg;
        document.getElementById('toast-container').appendChild(t);
        setTimeout(() => { t.classList.add('fadeout'); setTimeout(() => t.remove(), 300); }, 3000);
    }

    // ─── WebSocket ───
    let ws = null;
    const wsStatus = document.getElementById('ws-status');
    const wsIndicator = wsStatus.querySelector('.indicator');
    const wsText = wsStatus.querySelector('.text');

    function connectWS() {
        const proto = location.protocol === 'https:' ? 'wss' : 'ws';
        ws = new WebSocket(`${proto}://${location.host}/ws`);
        ws.onopen = () => {
            wsIndicator.classList.add('connected');
            wsIndicator.classList.remove('pulsing');
            wsText.textContent = 'Connected';
        };
        ws.onclose = () => {
            wsIndicator.classList.remove('connected');
            wsIndicator.classList.add('pulsing');
            wsText.textContent = 'Reconnecting...';
            setTimeout(connectWS, 2000);
        };
        ws.onmessage = (e) => {
            try { handleUpdate(JSON.parse(e.data)); } catch (err) { console.error(err); }
        };
    }

    // ─── Update Handler ───
    function handleUpdate(data) {
        if (data.system) {
            document.getElementById('sys-cpu').textContent = (data.system.cpu_load || 0).toFixed(1) + '%';
            const ramMB = Math.round(data.system.ram_used_mb || 0);
            document.getElementById('sys-ram').textContent = ramMB + ' MB';
        }

        if (data.switcher) {
            const sw = data.switcher;
            const badge = document.getElementById('main-status-badge');
            badge.dataset.state = sw.state;
            badge.textContent = sw.state.toUpperCase().replace('_', ' ');

            const card = document.getElementById('switcher-card');
            card.style.borderLeftColor = sw.state === 'live' ? 'var(--live)' : (sw.state === 'fallback' || sw.state === 'backup') ? 'var(--fallback)' : 'var(--starting)';
            card.style.borderLeftWidth = '3px';

            const mbps = ((sw.input_bitrate_kbps || 0) / 1000).toFixed(2);
            document.getElementById('stat-bitrate').innerHTML = `${mbps} <small>Mbps</small>`;
            document.getElementById('stat-uptime').textContent = sw.uptime || '—';
            document.getElementById('stat-packets-rx').textContent = formatNum(sw.packets_received);
            document.getElementById('stat-packets-fwd').textContent = formatNum(sw.packets_forwarded);
            document.getElementById('stat-pids').textContent = `${sw.video_pid || '—'} / ${sw.audio_pid || '—'}`;
            document.getElementById('stat-switch-count').textContent = sw.switch_count || 0;
            document.getElementById('stat-last-switch').textContent = sw.last_switch_time ? new Date(sw.last_switch_time).toLocaleTimeString() : '—';

            if (sw.bitrate_history) drawBitrateChart(sw.bitrate_history);
            if (sw.recent_events) renderHistory(sw.recent_events);
        }

        if (data.outputs) renderOutputs(data.outputs);
        if (data.audio) updateVUMeter(data.audio);
        if (data.recording !== undefined) updateRecordingUI(data.recording);
    }

    function formatNum(n) {
        if (!n) return '0';
        if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M';
        if (n >= 1e3) return (n / 1e3).toFixed(1) + 'K';
        return n.toString();
    }

    // ─── Bitrate Chart ───
    const chartCanvas = document.getElementById('bitrate-chart');
    const chartCtx = chartCanvas.getContext('2d');

    function drawBitrateChart(history) {
        const dpr = window.devicePixelRatio || 1;
        const rect = chartCanvas.getBoundingClientRect();
        chartCanvas.width = rect.width * dpr;
        chartCanvas.height = 120 * dpr;
        chartCtx.scale(dpr, dpr);

        const w = rect.width, h = 120;
        const pad = { top: 10, right: 10, bottom: 20, left: 45 };
        const plotW = w - pad.left - pad.right;
        const plotH = h - pad.top - pad.bottom;

        chartCtx.clearRect(0, 0, w, h);
        if (!history || history.length === 0) return;

        const maxKbps = Math.max(1000, ...history) * 1.1;

        // Grid
        chartCtx.strokeStyle = 'rgba(255,255,255,0.05)';
        chartCtx.lineWidth = 1;
        for (let i = 0; i <= 4; i++) {
            const y = pad.top + (plotH / 4) * i;
            chartCtx.beginPath(); chartCtx.moveTo(pad.left, y); chartCtx.lineTo(w - pad.right, y); chartCtx.stroke();
            chartCtx.fillStyle = 'rgba(255,255,255,0.3)';
            chartCtx.font = '9px Inter, sans-serif';
            chartCtx.textAlign = 'right';
            chartCtx.fillText(((maxKbps / 4) * (4 - i) / 1000).toFixed(1) + 'M', pad.left - 5, y + 3);
        }

        // Fill
        const gradient = chartCtx.createLinearGradient(0, pad.top, 0, pad.top + plotH);
        gradient.addColorStop(0, 'rgba(0,212,255,0.15)');
        gradient.addColorStop(1, 'rgba(0,212,255,0)');
        chartCtx.beginPath();
        chartCtx.moveTo(pad.left, pad.top + plotH);
        for (let i = 0; i < history.length; i++) {
            const x = pad.left + (i / Math.max(1, history.length - 1)) * plotW;
            const y = pad.top + plotH - (history[i] / maxKbps) * plotH;
            chartCtx.lineTo(x, y);
        }
        chartCtx.lineTo(pad.left + plotW, pad.top + plotH);
        chartCtx.closePath();
        chartCtx.fillStyle = gradient;
        chartCtx.fill();

        // Line
        chartCtx.beginPath();
        for (let i = 0; i < history.length; i++) {
            const x = pad.left + (i / Math.max(1, history.length - 1)) * plotW;
            const y = pad.top + plotH - (history[i] / maxKbps) * plotH;
            i === 0 ? chartCtx.moveTo(x, y) : chartCtx.lineTo(x, y);
        }
        chartCtx.strokeStyle = '#00d4ff';
        chartCtx.lineWidth = 1.5;
        chartCtx.stroke();

        // Current
        if (history.length > 0) {
            chartCtx.fillStyle = '#00d4ff';
            chartCtx.font = 'bold 10px Inter';
            chartCtx.textAlign = 'right';
            chartCtx.fillText((history[history.length - 1] / 1000).toFixed(2) + ' Mbps', w - pad.right, pad.top - 1);
        }
    }

    // ─── VU Meter ───
    const vuLeft = document.getElementById('vu-left');
    const vuRight = document.getElementById('vu-right');
    const vuDbValue = document.getElementById('vu-db-value');

    function updateVUMeter(audio) {
        const toP = (db) => Math.max(0, Math.min(100, ((db + 60) / 60) * 100));
        vuLeft.style.width = toP(audio.peak_left_db) + '%';
        vuRight.style.width = toP(audio.peak_right_db) + '%';
        const avg = (audio.peak_left_db + audio.peak_right_db) / 2;
        vuDbValue.textContent = avg <= -60 ? '-∞ dB' : avg.toFixed(1) + ' dB';
    }

    // ─── Recording ───
    const btnRec = document.getElementById('btn-rec');
    let isRecording = false;

    function updateRecordingUI(rec) {
        isRecording = rec.active;
        const recStatus = document.getElementById('rec-status');
        if (rec.active) {
            btnRec.className = 'btn btn-rec recording';
            btnRec.textContent = '⏹ STOP REC';
            recStatus.textContent = `🔴 REC ${rec.duration || ''} — ${rec.size_mb || 0} MB`;
            recStatus.style.display = 'inline';
        } else {
            btnRec.className = 'btn btn-rec';
            btnRec.textContent = '⏺ REC';
            recStatus.textContent = '';
            recStatus.style.display = 'none';
        }
    }

    btnRec.addEventListener('click', async () => {
        const action = isRecording ? 'stop' : 'start';
        try {
            const res = await fetch(`/api/recording?action=${action}`, { method: 'POST' });
            const d = await res.json();
            if (res.ok) toast(d.status, 'success'); else toast(d.error || 'Error', 'error');
        } catch (e) { toast(e.message, 'error'); }
    });

    // Recordings modal
    document.getElementById('btn-show-recordings').addEventListener('click', () => { openModal(document.getElementById('recordings-modal')); loadRecordings(); });
    document.getElementById('btn-refresh-recs').addEventListener('click', loadRecordings);

    async function loadRecordings() {
        const list = document.getElementById('recordings-list');
        list.innerHTML = '<p class="text-muted">Loading...</p>';
        try {
            const res = await fetch('/api/recordings');
            const data = await res.json();
            const recs = data.recordings;
            if (!recs || recs.length === 0) { list.innerHTML = '<p class="text-muted">No recordings yet.</p>'; return; }
            list.innerHTML = recs.map(r => `
                <div class="recording-item">
                    <span class="rec-name" title="${r.path}">🎬 ${r.name}</span>
                    <span class="rec-size">${r.size_mb} MB</span>
                    <span class="rec-date">${new Date(r.mod_time).toLocaleString()}</span>
                    <div class="rec-actions">
                        <button class="btn-icon" title="Download" onclick="location.href='/api/recordings/${encodeURIComponent(r.name)}'">⬇</button>
                        <button class="btn-icon" title="Rename" onclick="renameRec('${r.name}')">✏</button>
                        <button class="btn-icon btn-delete" title="Delete" onclick="deleteRec('${r.name}')">🗑</button>
                    </div>
                </div>`).join('');
        } catch (e) { list.innerHTML = `<p class="text-muted">Error: ${e.message}</p>`; }
    }

    // Global functions for inline handlers
    window.renameRec = async function(name) {
        const newName = prompt('New filename:', name);
        if (!newName || newName === name) return;
        try {
            const res = await fetch(`/api/recordings/${encodeURIComponent(name)}`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ new_name: newName }) });
            if (res.ok) { toast('Renamed!', 'success'); loadRecordings(); }
            else toast('Rename failed', 'error');
        } catch (e) { toast(e.message, 'error'); }
    };

    window.deleteRec = async function(name) {
        if (!confirm(`Delete ${name}?`)) return;
        try {
            const res = await fetch(`/api/recordings/${encodeURIComponent(name)}`, { method: 'DELETE' });
            if (res.ok) { toast('Deleted!', 'success'); loadRecordings(); }
            else toast('Delete failed', 'error');
        } catch (e) { toast(e.message, 'error'); }
    };

    // ─── Switch History ───
    function renderHistory(events) {
        const list = document.getElementById('history-list');
        if (!events || events.length === 0) { list.innerHTML = '<p class="text-muted">No events yet.</p>'; return; }
        list.innerHTML = events.slice().reverse().map(ev => {
            const time = ev.time ? new Date(ev.time).toLocaleTimeString() : '';
            return `<div class="history-event">
                <span class="history-dot ${ev.to}"></span>
                <span class="history-time">${time}</span>
                <span>${ev.from} → ${ev.to}</span>
                <span class="history-reason">${ev.reason}</span>
            </div>`;
        }).join('');
    }

    // ─── Outputs ───
    let editingOutputId = null;
    let currentLogsId = null;
    const template = document.getElementById('output-card-template');

    function renderOutputs(outputs) {
        const container = document.getElementById('outputs-container');
        if (!outputs || outputs.length === 0) { container.innerHTML = '<div class="empty-state"><p>No outputs configured.</p></div>'; return; }
        const existing = new Map();
        container.querySelectorAll('.output-card').forEach(c => existing.set(c.dataset.id, c));
        outputs.forEach(out => {
            let card = existing.get(out.id);
            if (!card) {
                const clone = template.content.cloneNode(true);
                card = clone.querySelector('.output-card');
                card.dataset.id = out.id;
                setupOutputCard(card, out.id);
                container.appendChild(card);
            }
            updateOutputCard(card, out);
            existing.delete(out.id);
        });
        existing.forEach(c => c.remove());
    }

    function setupOutputCard(card, id) {
        card.querySelector('.btn-toggle').addEventListener('click', () => toggleOutput(id));
        card.querySelector('.btn-delete').addEventListener('click', () => deleteOutput(id));
        card.querySelector('.btn-edit').addEventListener('click', () => editOutput(id));
        card.querySelector('.btn-logs').addEventListener('click', () => showLogs(id));
    }

    function updateOutputCard(card, out) {
        card.querySelector('.output-name').textContent = out.name || out.id;
        card.classList.toggle('running', out.running);
        const s = card.querySelector('.output-status');
        s.textContent = out.running ? 'Running' : 'Stopped';
        s.className = `detail-value output-status ${out.running ? 'running' : 'stopped'}`;
        card.querySelector('.output-codec').textContent = out.codec === 'h264' ? 'H.264' : 'H.265';
        card.querySelector('.output-bitrate').textContent = out.bitrate ? out.bitrate + 'k' : 'Copy';
        card.querySelector('.output-uptime').textContent = out.uptime || '—';
        const e = card.querySelector('.output-error');
        if (out.last_error) { e.style.display = 'block'; e.textContent = out.last_error; } else e.style.display = 'none';
        const t = card.querySelector('.btn-toggle');
        t.title = out.running ? 'Stop' : 'Start';
        t.textContent = out.running ? '⏸' : '▶';
    }

    async function toggleOutput(id) {
        const card = document.querySelector(`.output-card[data-id="${id}"]`);
        const running = card && card.classList.contains('running');
        try {
            const res = await fetch(`/api/outputs/${id}/${running ? 'stop' : 'start'}`, { method: 'POST' });
            if (res.ok) toast(`Output ${running ? 'stopped' : 'started'}`, 'success');
        } catch (e) { toast(e.message, 'error'); }
    }

    async function deleteOutput(id) {
        if (!confirm('Delete this output?')) return;
        try { await fetch(`/api/outputs/${id}`, { method: 'DELETE' }); toast('Deleted', 'success'); } catch (e) { toast(e.message, 'error'); }
    }

    function editOutput(id) {
        editingOutputId = id;
        fetch('/api/outputs').then(r => r.json()).then(outputs => {
            const out = outputs.find(o => o.id === id);
            if (!out) return;
            document.getElementById('out-name').value = out.name || '';
            document.getElementById('out-url').value = out.url || '';
            document.getElementById('out-key').value = out.stream_key || '';
            document.getElementById('out-codec').value = out.codec || 'h265';
            document.getElementById('out-bitrate').value = out.bitrate || 6000;
            document.getElementById('out-preset').value = out.preset || 'ultrafast';
            document.getElementById('out-overlay').checked = out.overlay_enabled || false;
            toggleH264Options();
            document.querySelector('#add-modal .modal-header h3').textContent = 'Edit Output';
            openModal(document.getElementById('add-modal'));
        });
    }

    async function showLogs(id) {
        currentLogsId = id;
        const card = document.querySelector(`.output-card[data-id="${id}"]`);
        document.getElementById('logs-output-name').textContent = card?.querySelector('.output-name')?.textContent || id;
        document.getElementById('logs-content').textContent = 'Loading...';
        openModal(document.getElementById('logs-modal'));
        refreshLogs();
    }

    async function refreshLogs() {
        if (!currentLogsId) return;
        try {
            const r = await fetch(`/api/outputs/${currentLogsId}/logs`);
            const d = await r.json();
            document.getElementById('logs-content').textContent = d.logs?.join('\n') || 'No logs.';
        } catch (e) { document.getElementById('logs-content').textContent = e.message; }
    }
    document.getElementById('btn-refresh-logs').addEventListener('click', refreshLogs);

    // ─── Config ───
    const configToggle = document.getElementById('config-toggle');
    const configBody = document.getElementById('config-body');
    configToggle.addEventListener('click', () => { configBody.classList.toggle('collapsed'); configToggle.querySelector('.collapse-icon').classList.toggle('open'); });

    const historyToggle = document.getElementById('history-toggle');
    const historyBody = document.getElementById('history-body');
    historyToggle.addEventListener('click', () => { historyBody.classList.toggle('collapsed'); historyToggle.querySelector('.collapse-icon').classList.toggle('open'); });

    async function fetchConfig() {
        try {
            const r = await fetch('/api/config');
            if (!r.ok) return;
            const c = await r.json();
            document.getElementById('cfg-srt-addr').value = c.srt_addr || '';
            document.getElementById('cfg-srt-mode').value = c.srt_mode || 'caller';
            document.getElementById('cfg-srt-timeout').value = c.srt_timeout || 2000;
            document.getElementById('cfg-min-bitrate').value = c.min_bitrate_kbps || 0;
            document.getElementById('cfg-hysteresis').value = c.bitrate_hysteresis_seconds || 5;
            document.getElementById('cfg-stats-url').value = c.stats_url || '';
            document.getElementById('cfg-fallback-path').value = c.fallback_path || '';
        } catch (e) { /* ok */ }
    }

    document.getElementById('btn-save-config').addEventListener('click', async () => {
        const cfg = {
            srt_addr: document.getElementById('cfg-srt-addr').value.trim(),
            srt_mode: document.getElementById('cfg-srt-mode').value,
            srt_timeout: parseInt(document.getElementById('cfg-srt-timeout').value, 10),
            stats_url: document.getElementById('cfg-stats-url').value.trim(),
            fallback_path: document.getElementById('cfg-fallback-path').value.trim(),
            min_bitrate_kbps: parseInt(document.getElementById('cfg-min-bitrate').value, 10) || 0,
            bitrate_hysteresis_seconds: parseInt(document.getElementById('cfg-hysteresis').value, 10) || 5,
        };
        try {
            const r = await fetch('/api/config', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(cfg) });
            if (r.ok) toast('Config saved!', 'success'); else toast('Failed', 'error');
        } catch (e) { toast(e.message, 'error'); }
    });

    // ─── Add/Edit Output ───
    const codecSelect = document.getElementById('out-codec');
    function toggleH264Options() { document.getElementById('h264-options').style.display = codecSelect.value === 'h264' ? 'block' : 'none'; }
    codecSelect.addEventListener('change', toggleH264Options);

    document.getElementById('btn-add-output').addEventListener('click', () => {
        editingOutputId = null;
        document.getElementById('form-add-output').reset();
        toggleH264Options();
        document.querySelector('#add-modal .modal-header h3').textContent = 'Add Output';
        openModal(document.getElementById('add-modal'));
    });

    document.getElementById('form-add-output').addEventListener('submit', async (e) => {
        e.preventDefault();
        const config = {
            name: document.getElementById('out-name').value.trim(),
            url: document.getElementById('out-url').value.trim(),
            stream_key: document.getElementById('out-key').value.trim(),
            codec: document.getElementById('out-codec').value,
            bitrate: parseInt(document.getElementById('out-bitrate').value, 10) || 0,
            preset: document.getElementById('out-preset').value,
            overlay_enabled: document.getElementById('out-overlay').checked,
        };
        const method = editingOutputId ? 'PUT' : 'POST';
        const url = editingOutputId ? `/api/outputs/${editingOutputId}` : '/api/outputs';
        try {
            const r = await fetch(url, { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(config) });
            if (r.ok) { toast(editingOutputId ? 'Updated!' : 'Added!', 'success'); closeModal(document.getElementById('add-modal')); }
            else toast('Failed', 'error');
        } catch (e) { toast(e.message, 'error'); }
    });

    // ─── Quick Actions ───
    async function quickAction(action) {
        try { const r = await fetch(`/api/actions/${action}`, { method: 'POST' }); const d = await r.json(); toast(d.status || 'Done', 'success'); }
        catch (e) { toast(e.message, 'error'); }
    }
    document.getElementById('act-restart-srt').addEventListener('click', () => quickAction('restart-srt'));
    document.getElementById('act-restart-fallback').addEventListener('click', () => quickAction('restart-fallback'));
    document.getElementById('act-restart-outputs').addEventListener('click', () => quickAction('restart-outputs'));
    document.getElementById('act-restart-all').addEventListener('click', () => { if (confirm('Restart ALL?')) quickAction('restart-all'); });

    // ─── Assets ───
    document.getElementById('btn-open-assets').addEventListener('click', () => openModal(document.getElementById('assets-modal')));

    async function handleUpload(url, inputId, btnId) {
        const input = document.getElementById(inputId);
        const btn = document.getElementById(btnId);
        if (!input.files.length) { toast('Select a file', 'error'); return; }
        const fd = new FormData(); fd.append('file', input.files[0]);
        btn.disabled = true; btn.textContent = 'Uploading...';
        try { const r = await fetch(url, { method: 'POST', body: fd }); r.ok ? toast('Upload OK! Fallback restarting.', 'success') : toast('Failed', 'error'); }
        catch (e) { toast(e.message, 'error'); }
        btn.disabled = false; btn.textContent = btnId.includes('fallback') ? 'Upload Fallback' : 'Upload Watermark';
    }
    document.getElementById('btn-upload-fallback').addEventListener('click', () => handleUpload('/api/upload/fallback', 'file-fallback', 'btn-upload-fallback'));
    document.getElementById('btn-upload-watermark').addEventListener('click', () => handleUpload('/api/upload/watermark', 'file-watermark', 'btn-upload-watermark'));

    // ─── Preview ───
    const previewImg = document.getElementById('preview-snapshot');
    let previewInterval = null;
    let previewFps = 2;

    function stopPreview() { if (previewInterval) { clearInterval(previewInterval); previewInterval = null; } }
    function startPreview() {
        stopPreview();
        if (previewFps <= 0) return;
        const ms = Math.max(200, Math.floor(1000 / previewFps));
        const load = () => { const img = new Image(); img.onload = () => { previewImg.src = img.src; }; img.src = '/api/preview/frame?t=' + Date.now(); };
        load();
        previewInterval = setInterval(load, ms);
    }

    document.getElementById('btn-apply-preview').addEventListener('click', async () => {
        const fps = parseInt(document.getElementById('preview-fps').value, 10);
        const [w, h] = document.getElementById('preview-res').value.split('x').map(Number);
        previewFps = fps;
        try {
            await fetch(`/api/preview/settings?fps=${fps}&w=${w}&h=${h}`, { method: 'PUT' });
            if (fps === 0) { stopPreview(); toast('Preview off', 'info'); }
            else { startPreview(); toast(`Preview: ${fps}fps ${w}x${h}`, 'success'); }
        } catch (e) { toast(e.message, 'error'); }
    });
    startPreview();

    // ─── Modal Helpers ───
    function openModal(m) { if (m) m.classList.add('open'); }
    function closeModal(m) { if (m) m.classList.remove('open'); editingOutputId = null; currentLogsId = null; }

    document.querySelectorAll('.btn-close-modal, .btn-cancel-modal').forEach(b => b.addEventListener('click', () => { const m = b.closest('.modal-backdrop'); if (m) closeModal(m); }));
    document.querySelectorAll('.modal-backdrop').forEach(b => b.addEventListener('click', e => { if (e.target === b) closeModal(b); }));
    document.addEventListener('keydown', e => { if (e.key === 'Escape') document.querySelectorAll('.modal-backdrop.open').forEach(closeModal); });

    // ─── Start ───
    fetchConfig();
    connectWS();
});
