document.addEventListener('DOMContentLoaded', () => {
    // ─── State ───
    let ws = null;
    let wsReconnectTimer = null;
    const knownOutputs = new Set();
    const knownOutputData = new Map();
    let editingOutputId = null;
    let currentLogsId = null;

    // ─── DOM Refs ───
    const wsStatus = document.getElementById('ws-status');
    const outputsContainer = document.getElementById('outputs-container');
    const emptyOutputs = document.getElementById('empty-outputs');
    const template = document.getElementById('output-card-template');

    // ─── Toast System ───
    function toast(msg, type = 'info') {
        const container = document.getElementById('toast-container');
        const el = document.createElement('div');
        el.className = `toast ${type}`;
        el.textContent = msg;
        container.appendChild(el);
        setTimeout(() => {
            el.classList.add('fadeout');
            setTimeout(() => el.remove(), 300);
        }, 4000);
    }

    // ─── WebSocket ───
    function connectWS() {
        const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
        ws = new WebSocket(`${proto}//${location.host}/ws`);

        ws.onopen = () => {
            setWSStatus(true);
            fetchInitialState();
            fetchConfig();
        };
        ws.onmessage = (e) => {
            try { updateUI(JSON.parse(e.data)); } catch (err) { console.error('WS parse error', err); }
        };
        ws.onclose = () => {
            setWSStatus(false);
            wsReconnectTimer = setTimeout(connectWS, 3000);
        };
        ws.onerror = () => ws.close();
    }

    function setWSStatus(connected) {
        const indicator = wsStatus.querySelector('.indicator');
        const text = wsStatus.querySelector('.text');
        if (connected) {
            indicator.className = 'indicator connected pulsing';
            text.textContent = 'Connected';
        } else {
            indicator.className = 'indicator pulsing';
            text.textContent = 'Disconnected';
        }
    }

    async function fetchInitialState() {
        try {
            const res = await fetch('/api/status');
            if (res.ok) updateUI(await res.json());
        } catch (e) { /* silent */ }
    }

    // ─── UI Updaters ───
    function updateUI(data) {
        if (data.system) {
            document.getElementById('sys-cpu').textContent = data.system.cpu_load.toFixed(1) + '%';
            document.getElementById('sys-ram').textContent =
                data.system.ram_used_mb.toFixed(0) + ' / ' + data.system.ram_total_mb.toFixed(0) + ' MB';
        }
        if (data.switcher) updateSwitcherStats(data.switcher);
        if (data.outputs) updateOutputs(data.outputs);
    }

    function updateSwitcherStats(s) {
        const badge = document.getElementById('main-status-badge');
        badge.dataset.state = s.state;
        badge.textContent = s.state.toUpperCase().replace('_', ' ');

        const bitrateKbps = s.input_bitrate_kbps || 0;
        document.getElementById('stat-bitrate').innerHTML =
            (bitrateKbps / 1000).toFixed(2) + ' <small>Mbps</small>';
        document.getElementById('stat-uptime').textContent = s.uptime || '—';
        document.getElementById('stat-packets-rx').textContent = formatNum(s.packets_received);
        document.getElementById('stat-packets-fwd').textContent = formatNum(s.packets_forwarded);
        document.getElementById('stat-pids').textContent =
            (s.video_pid || '—') + ' / ' + (s.audio_pid || '—');

        document.getElementById('stat-switch-count').textContent = s.switch_count || 0;
        if (s.last_switch_time) {
            const d = new Date(s.last_switch_time);
            document.getElementById('stat-last-switch').textContent = d.toLocaleTimeString();
        }
    }

    function formatNum(n) {
        if (!n) return '0';
        if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M';
        if (n >= 1e3) return (n / 1e3).toFixed(1) + 'K';
        return n.toString();
    }

    // ─── Outputs ───
    function updateOutputs(outputs) {
        const currentIds = new Set(outputs.map(o => o.id));

        // Remove old
        for (const id of knownOutputs) {
            if (!currentIds.has(id)) {
                const card = outputsContainer.querySelector(`[data-id="${id}"]`);
                if (card) card.remove();
                knownOutputs.delete(id);
                knownOutputData.delete(id);
            }
        }

        // Add/Update
        for (const o of outputs) {
            knownOutputData.set(o.id, o);
            if (!knownOutputs.has(o.id)) {
                knownOutputs.add(o.id);
                const card = createOutputCard(o);
                outputsContainer.appendChild(card);
            } else {
                const card = outputsContainer.querySelector(`[data-id="${o.id}"]`);
                if (card) updateOutputCard(card, o);
            }
        }

        // Empty state
        emptyOutputs.style.display = knownOutputs.size === 0 ? 'block' : 'none';
    }

    function createOutputCard(o) {
        const frag = template.content.cloneNode(true);
        const card = frag.querySelector('.output-card');
        card.dataset.id = o.id;

        card.querySelector('.btn-edit').addEventListener('click', () => editOutput(o.id));
        card.querySelector('.btn-logs').addEventListener('click', () => openLogsModal(o.id, o.name));
        card.querySelector('.btn-toggle').addEventListener('click', () => toggleOutput(o.id));
        card.querySelector('.btn-delete').addEventListener('click', () => deleteOutput(o.id));

        updateOutputCard(card, o);
        return card;
    }

    function updateOutputCard(card, o) {
        card.querySelector('.output-name').textContent = o.name;
        card.classList.toggle('running', o.running);

        const statusEl = card.querySelector('.output-status');
        statusEl.textContent = o.running ? 'Running' : 'Stopped';
        statusEl.className = `detail-value output-status ${o.running ? 'running' : 'stopped'}`;

        card.querySelector('.output-codec').textContent = o.codec === 'h264' ? 'H.264' : 'H.265';

        const bkbps = o.output_bitrate_kbps || 0;
        card.querySelector('.output-bitrate').textContent =
            bkbps > 0 ? (bkbps / 1000).toFixed(1) + ' Mbps' : '—';

        card.querySelector('.output-uptime').textContent = o.uptime || '—';

        // Toggle button icon
        const toggleBtn = card.querySelector('.btn-toggle');
        if (o.running) {
            toggleBtn.innerHTML = '<svg viewBox="0 0 24 24" width="14" height="14" stroke="currentColor" stroke-width="2" fill="none"><rect x="6" y="4" width="4" height="16"/><rect x="14" y="4" width="4" height="16"/></svg>';
            toggleBtn.title = 'Stop';
        } else {
            toggleBtn.innerHTML = '<svg viewBox="0 0 24 24" width="14" height="14" stroke="currentColor" stroke-width="2" fill="currentColor"><polygon points="5 3 19 12 5 21 5 3"/></svg>';
            toggleBtn.title = 'Start';
        }

        // Error
        const errEl = card.querySelector('.output-error');
        if (o.error && o.running) {
            errEl.textContent = o.error;
            errEl.style.display = 'block';
        } else {
            errEl.style.display = 'none';
        }
    }

    async function toggleOutput(id) {
        const o = knownOutputData.get(id);
        if (!o) return;
        const action = o.running ? 'stop' : 'start';
        try {
            const res = await fetch(`/api/outputs/${id}/${action}`, { method: 'POST' });
            if (res.ok) toast(`Output ${action}ed`, 'success');
            else toast(`Failed to ${action} output`, 'error');
        } catch (e) { toast(`Error: ${e.message}`, 'error'); }
    }

    async function deleteOutput(id) {
        if (!confirm('Delete this output?')) return;
        try {
            const res = await fetch(`/api/outputs/${id}`, { method: 'DELETE' });
            if (res.ok) toast('Output deleted', 'success');
            else toast('Failed to delete output', 'error');
        } catch (e) { toast(`Error: ${e.message}`, 'error'); }
    }

    // ─── Config Section ───
    const configToggle = document.getElementById('config-toggle');
    const configBody = document.getElementById('config-body');
    const collapseIcon = configToggle.querySelector('.collapse-icon');

    configToggle.addEventListener('click', () => {
        configBody.classList.toggle('collapsed');
        collapseIcon.classList.toggle('open');
    });

    async function fetchConfig() {
        try {
            const res = await fetch('/api/config');
            if (res.ok) {
                const cfg = await res.json();
                document.getElementById('cfg-srt-addr').value = cfg.srt_addr || '';
                document.getElementById('cfg-srt-mode').value = cfg.srt_mode || 'caller';
                document.getElementById('cfg-srt-timeout').value = cfg.srt_timeout || 2000;
                document.getElementById('cfg-min-bitrate').value = cfg.min_bitrate_kbps || 0;
                document.getElementById('cfg-hysteresis').value = cfg.bitrate_hysteresis_seconds || 5;
                document.getElementById('cfg-stats-url').value = cfg.stats_url || '';
                document.getElementById('cfg-fallback-path').value = cfg.fallback_path || '';
            }
        } catch (e) { /* silent */ }
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
            const res = await fetch('/api/config', {
                method: 'PUT',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(cfg),
            });
            if (res.ok) toast('Configuration saved! Restart may be needed for SRT changes.', 'success');
            else toast('Failed to save config', 'error');
        } catch (e) { toast(`Error: ${e.message}`, 'error'); }
    });

    // ─── Add/Edit Output Modal ───
    const addModal = document.getElementById('add-modal');
    const formAddOutput = document.getElementById('form-add-output');
    const codecSelect = document.getElementById('out-codec');
    const h264Options = document.getElementById('h264-options');

    document.getElementById('btn-add-output').addEventListener('click', () => {
        editingOutputId = null;
        addModal.querySelector('h3').textContent = 'Add New Output';
        formAddOutput.reset();
        h264Options.style.display = 'none';
        openModal(addModal);
    });

    function editOutput(id) {
        const o = knownOutputData.get(id);
        if (!o) return;
        editingOutputId = id;
        addModal.querySelector('h3').textContent = 'Edit Output';
        document.getElementById('out-name').value = o.name || '';
        document.getElementById('out-url').value = o.url || '';
        document.getElementById('out-key').value = o.stream_key || '';
        document.getElementById('out-codec').value = o.codec || 'h265';
        if (o.codec === 'h264') {
            h264Options.style.display = 'block';
            document.getElementById('out-bitrate').value = o.bitrate || 6000;
            document.getElementById('out-preset').value = o.preset || 'ultrafast';
            document.getElementById('out-overlay').checked = !!o.overlay_enabled;
        } else {
            h264Options.style.display = 'none';
        }
        openModal(addModal);
    }

    codecSelect.addEventListener('change', (e) => {
        h264Options.style.display = e.target.value === 'h264' ? 'block' : 'none';
    });

    formAddOutput.addEventListener('submit', async (e) => {
        e.preventDefault();
        const payload = {
            name: document.getElementById('out-name').value.trim(),
            url: document.getElementById('out-url').value.trim(),
            stream_key: document.getElementById('out-key').value.trim(),
            codec: document.getElementById('out-codec').value,
        };
        if (payload.codec === 'h264') {
            payload.bitrate = parseInt(document.getElementById('out-bitrate').value, 10);
            payload.preset = document.getElementById('out-preset').value;
            payload.overlay_enabled = document.getElementById('out-overlay').checked;
        }
        try {
            const isEdit = editingOutputId !== null;
            const endpoint = isEdit ? `/api/outputs/${editingOutputId}` : '/api/outputs';
            const method = isEdit ? 'PUT' : 'POST';
            const res = await fetch(endpoint, { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) });
            if (res.ok) {
                toast(isEdit ? 'Output updated' : 'Output created', 'success');
                closeModal(addModal);
            } else {
                const data = await res.json();
                toast(`Error: ${data.error || 'Unknown'}`, 'error');
            }
        } catch (e) { toast(`Error: ${e.message}`, 'error'); }
    });

    // ─── Logs Modal ───
    const logsModal = document.getElementById('logs-modal');

    async function openLogsModal(id, name) {
        currentLogsId = id;
        document.getElementById('logs-output-name').textContent = name;
        document.getElementById('logs-content').textContent = 'Loading logs...';
        openModal(logsModal);
        await fetchLogs(id);
    }

    async function fetchLogs(id) {
        try {
            const res = await fetch(`/api/outputs/${id}/logs`);
            if (res.ok) {
                const data = await res.json();
                const pre = document.getElementById('logs-content');
                pre.textContent = (data.logs && data.logs.length > 0) ? data.logs.join('\n') : 'No logs available yet.';
                pre.parentElement.scrollTop = pre.parentElement.scrollHeight;
            }
        } catch (e) { document.getElementById('logs-content').textContent = `Error: ${e.message}`; }
    }

    document.getElementById('btn-refresh-logs').addEventListener('click', () => {
        if (currentLogsId) fetchLogs(currentLogsId);
    });

    // ─── Assets Modal ───
    const assetsModal = document.getElementById('assets-modal');
    document.getElementById('btn-open-assets').addEventListener('click', () => openModal(assetsModal));

    async function handleUpload(endpoint, fileInputId, btnId) {
        const fileInput = document.getElementById(fileInputId);
        if (!fileInput.files || fileInput.files.length === 0) {
            toast('Please select a file first.', 'error');
            return;
        }
        const btn = document.getElementById(btnId);
        const originalText = btn.textContent;
        btn.textContent = 'Uploading...';
        btn.disabled = true;

        const formData = new FormData();
        formData.append('file', fileInput.files[0]);

        try {
            const res = await fetch(endpoint, { method: 'POST', body: formData });
            if (res.ok) {
                toast('Upload successful!', 'success');
                fileInput.value = '';
                fetchConfig(); // Refresh fallback path display
            } else {
                const text = await res.text();
                toast(`Upload failed: ${text}`, 'error');
            }
        } catch (e) { toast(`Upload error: ${e.message}`, 'error'); }
        btn.textContent = originalText;
        btn.disabled = false;
    }

    document.getElementById('btn-upload-fallback').addEventListener('click', () =>
        handleUpload('/api/upload/fallback', 'file-fallback', 'btn-upload-fallback'));
    document.getElementById('btn-upload-watermark').addEventListener('click', () =>
        handleUpload('/api/upload/watermark', 'file-watermark', 'btn-upload-watermark'));

    // ─── Preview (Snapshot) ───
    const previewImg = document.getElementById('preview-snapshot');
    let previewInterval = null;
    let previewFps = 2;

    function stopPreviewRefresh() {
        if (previewInterval) { clearInterval(previewInterval); previewInterval = null; }
        previewImg.removeAttribute('src');
    }

    function startPreviewRefresh() {
        stopPreviewRefresh();
        if (previewFps <= 0) return;
        const ms = Math.max(200, Math.floor(1000 / previewFps));
        previewImg.src = '/api/preview/frame?t=' + Date.now();
        previewInterval = setInterval(() => {
            previewImg.src = '/api/preview/frame?t=' + Date.now();
        }, ms);
    }

    document.getElementById('btn-apply-preview').addEventListener('click', async () => {
        const fps = parseInt(document.getElementById('preview-fps').value, 10);
        const [w, h] = document.getElementById('preview-res').value.split('x').map(Number);
        previewFps = fps;
        try {
            await fetch(`/api/preview/settings?fps=${fps}&w=${w}&h=${h}`, { method: 'PUT' });
            if (fps === 0) {
                stopPreviewRefresh();
                toast('Preview desligado', 'info');
            } else {
                startPreviewRefresh();
                toast(`Preview: ${fps}fps ${w}x${h}`, 'success');
            }
        } catch (e) { toast(`Error: ${e.message}`, 'error'); }
    });

    // Start preview auto-refresh
    startPreviewRefresh();

    // ─── Modal Helpers ───
    function openModal(modal) { modal.classList.add('open'); }
    function closeModal(modal) { modal.classList.remove('open'); editingOutputId = null; currentLogsId = null; }

    // Close buttons (all modals)
    document.querySelectorAll('.btn-close-modal, .btn-cancel-modal').forEach(btn => {
        btn.addEventListener('click', () => {
            const modal = btn.closest('.modal-backdrop');
            if (modal) closeModal(modal);
        });
    });

    // Close on backdrop click
    document.querySelectorAll('.modal-backdrop').forEach(backdrop => {
        backdrop.addEventListener('click', (e) => {
            if (e.target === backdrop) closeModal(backdrop);
        });
    });

    // Close on ESC
    document.addEventListener('keydown', (e) => {
        if (e.key === 'Escape') {
            document.querySelectorAll('.modal-backdrop.open').forEach(m => closeModal(m));
        }
    });

    // ─── Start ───
    connectWS();
});
