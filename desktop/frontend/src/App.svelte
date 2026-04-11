<script>
  import "./app.css";
  import {
    StartReceiverDefault,
    StartSender,
    PlaySound,
    OpenFile,
    ResetApp,
    GetReceivedFiles,
    GetSavePath,
    SetSavePath,
    ApproveTransfer,
    RejectTransfer,
    GetTransferSettings,
    SaveTransferSettings,
  } from "../wailsjs/go/main/App.js";
  import { EventsOn, BrowserOpenURL } from "../wailsjs/runtime/runtime.js";
  import QRCode from "qrcode";
  import { onMount } from "svelte";
  import Typewriter from "./Typewriter.svelte";

  // Logo asset
  import logoImg from "./assets/images/icon.png";

  // ── App State ──────────────────────────────────────────────────────────────
  let mode = "RECEIVE"; // "RECEIVE" | "SEND" | "ABOUT"
  let connectionState = "IDLE"; // "IDLE" | "WAITING" | "CONNECTED" | "DISCONNECTED"

  let qrImage = "";
  let serverUrl = "";
  let senderUrl = "";

  let receivedFiles = [];
  let progress = {
    active: false,
    filename: "",
    percent: 0,
    speed: "0 MB/s",
    received: "0.00 MB",
    total: "0.00 MB",
    timeRemaining: "—",
    totalTime: "0s",
    speedColor: "#ffb000",
  };
  let lastProgressTime = 0;
  let lastLoaded = 0;
  let progressStartTime = 0; // Track when transfer started
  let speedHistory = []; // Rolling average for smooth speed display

  let showSenderDialog = false;
  let isDragOver = false;
  let savePath = ""; // persisted save directory

  // ── Transfer Request state ─────────────────────────────────────────────────
  let transferRequest = null; // pending transfer approval popup
  let rememberDevice = false;

  // ── Settings state ─────────────────────────────────────────────────────────
  let settings = {
    mode: "ask_first",
    maxFileSizeMB: 0,
    blockedExtensions: [],
    trustedDevices: [],
    blockedDevices: [],
  };
  let settingsDirty = false;
  let newBlockedExt = "";
  let newTrustedIP = "";
  let newTrustedName = "";
  let newBlockedIP = "";
  let newBlockedName = "";

  // ── Toast system ──────────────────────────────────────────────────────────
  // Each toast: { id, msg, type }
  let toasts = [];
  let _tid = 0;
  let _progressTimeout; // watchdog: clears stale progress if phone drops mid-upload

  function toast(msg, type = "info") {
    const id = ++_tid;
    toasts = [...toasts, { id, msg, type }];
    setTimeout(() => {
      toasts = toasts.filter((t) => t.id !== id);
    }, 3200);
  }

  // ── Cursor glow ─────────────────────────────────────────────────────────
  let cursorEl;
  let _raf;
  function handleMouseMove(e) {
    if (_raf) return;
    _raf = requestAnimationFrame(() => {
      if (cursorEl)
        cursorEl.style.transform = `translate3d(${e.clientX - 150}px,${e.clientY - 150}px,0)`;
      _raf = null;
    });
  }

  // ── Mount ─────────────────────────────────────────────────────────────────
  onMount(async () => {
    EventsOn("device_connected", () => {
      connectionState = "CONNECTED";
      playSound("connect");
      toast("⚡ Device linked to network", "success");
    });
    EventsOn("device_disconnected", () => {
      connectionState = "DISCONNECTED";
      playSound("click");
      toast("💔 Signal lost — device disconnected", "warn");
    });
    EventsOn("file_received", (filename) => {
      refreshFileList();
      // Fully reset progress — don't spread stale filename/numbers
      clearTimeout(_progressTimeout);
      progress = {
        active: false,
        filename: "",
        percent: 0,
        speed: "0 MB/s",
        received: "0.00 MB",
        total: "0.00 MB",
        timeRemaining: "—",
        totalTime: "0s",
        speedColor: "#ffb000",
      };
      lastLoaded = 0;
      lastProgressTime = 0;
      progressStartTime = 0;
      speedHistory = [];
      playSound("success");
      toast(`✅ Received: ${filename}`, "success");
    });

    // Helper function to format time duration (seconds to "2m 45s" format)
    const formatTime = (seconds) => {
      if (isNaN(seconds) || !isFinite(seconds)) return "—";
      if (seconds < 0) return "—";
      if (seconds < 60) return `${Math.round(seconds)}s`;
      const mins = Math.floor(seconds / 60);
      const secs = Math.round(seconds % 60);
      if (mins < 60) return `${mins}m ${secs}s`;
      const hours = Math.floor(mins / 60);
      const remainMins = mins % 60;
      return `${hours}h ${remainMins}m`;
    };

    // Helper function to get speed color indicator
    const getSpeedColor = (speedMBps) => {
      if (speedMBps > 10) return "#00ff00"; // Green: fast
      if (speedMBps > 5) return "#ffb000"; // Orange: medium
      return "#ff6b6b"; // Red: slow
    };

    // Helper function to calculate smooth speed using rolling average
    const calculateSmoothedSpeed = (currentSpeed) => {
      speedHistory.push(currentSpeed);
      if (speedHistory.length > 10) speedHistory.shift(); // Keep last 10 samples
      const avg =
        speedHistory.reduce((a, b) => a + b, 0) / speedHistory.length;
      return avg;
    };

    // Handler for both upload and download progress
    const handleProgressUpdate = (data) => {
      const parts = data.split("|");
      if (parts.length < 3) return;
      const [filename, wStr, tStr] = parts;
      const written = parseInt(wStr);
      const total = parseInt(tStr);
      const now = Date.now();
      const dt = (now - lastProgressTime) / 1000;

      // Initialize progress start time on first event
      if (progressStartTime === 0) {
        progressStartTime = now;
      }

      // Calculate raw speed
      let instantSpeed = 0; // MB/s
      if (dt > 0 && lastProgressTime > 0) {
        instantSpeed = (written - lastLoaded) / dt / 1048576;
      }

      // Apply smoothing to speed
      const smoothedSpeed = calculateSmoothedSpeed(Math.max(0, instantSpeed));
      const speedStr = `${Math.max(0, smoothedSpeed).toFixed(2)} MB/s`;
      const speedColor = getSpeedColor(smoothedSpeed);

      // Calculate ETA
      let timeRemaining = "—";
      if (smoothedSpeed > 0) {
        const remainingBytes = total - written;
        const secondsRemaining = remainingBytes / (smoothedSpeed * 1048576);
        timeRemaining = formatTime(secondsRemaining);
      }

      // Calculate total elapsed time
      const elapsedSeconds = (now - progressStartTime) / 1000;
      const totalTimeStr = formatTime(elapsedSeconds);

      lastLoaded = written;
      lastProgressTime = now;
      const pct =
        total > 0 ? Math.min(100, Math.round((written / total) * 100)) : 0;

      progress = {
        active: true,
        filename,
        percent: pct,
        speed: speedStr,
        received: `${(written / 1048576).toFixed(2)} MB`,
        total: `${(total / 1048576).toFixed(2)} MB`,
        timeRemaining,
        totalTime: totalTimeStr,
        speedColor,
      };

      if (connectionState !== "CONNECTED") connectionState = "CONNECTED";
      // Reset stale-progress watchdog: clears if no progress event for 30s
      clearTimeout(_progressTimeout);
      _progressTimeout = setTimeout(() => {
        progress = {
          active: false,
          filename: "",
          percent: 0,
          speed: "0 MB/s",
          received: "0.00 MB",
          total: "0.00 MB",
          timeRemaining: "—",
          totalTime: "0s",
          speedColor: "#ffb000",
        };
        lastLoaded = 0;
        lastProgressTime = 0;
        progressStartTime = 0;
        speedHistory = [];
      }, 30000);
    };

    EventsOn("upload_progress", handleProgressUpdate);
    EventsOn("download_progress", handleProgressUpdate);
    EventsOn("url_changed", (newURL) => {
      serverUrl = newURL;
      generateQR(newURL);
      if (showSenderDialog) senderUrl = newURL;
      toast("🔄 Network changed — QR refreshed", "info");
    });
    EventsOn("sender_started", (url) => {
      senderUrl = url;
      showSenderDialog = true;
      generateQR(url);
    });

    EventsOn("transfer_request", (dataStr) => {
      try {
        transferRequest = JSON.parse(dataStr);
        rememberDevice = false;
        playSound("blip");
      } catch {}
    });

    EventsOn("transfer_request_timeout", () => {
      if (transferRequest) {
        toast("⏰ Transfer request timed out", "warn");
        transferRequest = null;
      }
    });

    await initReceiver();
    // Load persisted save path for sidebar display
    try {
      savePath = await GetSavePath();
    } catch {
      savePath = "";
    }
    // Load transfer settings
    try {
      const s = await GetTransferSettings();
      if (s) settings = s;
    } catch {}
  });

  async function initReceiver() {
    connectionState = "WAITING";
    playSound("startup");
    try {
      serverUrl = await StartReceiverDefault();
    } catch {
      serverUrl = "";
      toast("❌ Failed to start receiver", "error");
      connectionState = "IDLE";
      return;
    }
    generateQR(serverUrl);
    await refreshFileList();
  }

  async function refreshFileList() {
    try {
      const files = await GetReceivedFiles();
      if (files) receivedFiles = files;
    } catch {
      /* non-blocking */
    }
  }

  async function switchMode(newMode) {
    // Re-allow switching to RECEIVE even if already there but connection is lost
    const alreadySameMode = mode === newMode;
    if (alreadySameMode && connectionState === "CONNECTED") return;
    playSound("blip");
    mode = newMode;
    if (newMode === "RECEIVE" && connectionState !== "CONNECTED") {
      await resetAll();
      await initReceiver();
    }
  }

  function openLink(url) {
    BrowserOpenURL(url);
  }

  async function startSend() {
    playSound("click");
    const result = await StartSender();
    if (result === "Cancelled") {
      toast("Sender cancelled", "info");
      return;
    }
    senderUrl = result;
    showSenderDialog = true;
    generateQR(result);
  }

  function generateQR(text) {
    if (!text) return;
    QRCode.toDataURL(
      text,
      {
        width: 220,
        margin: 2,
        color: { dark: "#00FF41", light: "#00000000" },
      },
      (err, url) => {
        if (!err) qrImage = url;
      },
    );
  }

  function playSound(type) {
    PlaySound(type);
  }
  function openFile(name) {
    OpenFile(name);
  }

  function formatSize(bytes) {
    if (!bytes) return "—";
    if (bytes >= 1048576) return (bytes / 1048576).toFixed(1) + " MB";
    if (bytes >= 1024) return (bytes / 1024).toFixed(0) + " KB";
    return bytes + " B";
  }

  function fileIcon(name = "") {
    const ext = name.split(".").pop().toLowerCase();
    const m = {
      pdf: "📄",
      jpg: "🖼️",
      jpeg: "🖼️",
      png: "🖼️",
      gif: "🖼️",
      webp: "🖼️",
      svg: "🖼️",
      mp4: "🎬",
      mov: "🎬",
      mkv: "🎬",
      avi: "🎬",
      mp3: "🎵",
      wav: "🎵",
      flac: "🎵",
      zip: "📦",
      tar: "📦",
      gz: "📦",
      rar: "📦",
      txt: "📝",
      md: "📝",
      doc: "📝",
      docx: "📝",
      apk: "📱",
      exe: "⚙️",
    };
    return m[ext] || "📁";
  }

  async function resetAll() {
    playSound("click");
    await ResetApp();
    connectionState = "IDLE";
    qrImage = "";
    serverUrl = "";
    senderUrl = "";
    showSenderDialog = false;
    progress = {
      active: false,
      filename: "",
      percent: 0,
      speed: "0 MB/s",
      received: "0.00 MB",
    };
    lastLoaded = 0;
    lastProgressTime = 0;
  }

  async function changeSavePath() {
    playSound("click");
    const result = await SetSavePath();
    if (result === "Cancelled") {
      toast("Folder selection cancelled", "info");
      return;
    }
    if (result.startsWith("Error:")) {
      toast("❌ " + result, "error");
      return;
    }
    // SetSavePath restarts receiver and returns new URL
    serverUrl = result;
    generateQR(result);
    savePath = await GetSavePath();
    connectionState = "WAITING";
    toast(`📁 Save path updated`, "success");
  }

  async function handleDisconnectReset() {
    await resetAll();
    mode = "RECEIVE";
    await initReceiver();
  }

  // ── Transfer request handlers ─────────────────────────────────────────────
  async function approveTransfer() {
    if (!transferRequest) return;
    const id = transferRequest.id;
    if (rememberDevice) {
      // Add to trusted, remove from blocked (mutual exclusivity)
      settings.blockedDevices = settings.blockedDevices.filter(
        (d) => d.ip !== transferRequest.senderIP
      );
      if (!settings.trustedDevices.find((d) => d.ip === transferRequest.senderIP)) {
        settings.trustedDevices = [
          ...settings.trustedDevices,
          { ip: transferRequest.senderIP, friendlyName: transferRequest.senderName },
        ];
      }
      await SaveTransferSettings(settings);
    }
    transferRequest = null;
    await ApproveTransfer(id);
    toast("✅ Transfer approved", "success");
  }

  async function rejectTransfer() {
    if (!transferRequest) return;
    const id = transferRequest.id;
    if (rememberDevice) {
      // Add to blocked, remove from trusted (mutual exclusivity)
      settings.trustedDevices = settings.trustedDevices.filter(
        (d) => d.ip !== transferRequest.senderIP
      );
      if (!settings.blockedDevices.find((d) => d.ip === transferRequest.senderIP)) {
        settings.blockedDevices = [
          ...settings.blockedDevices,
          { ip: transferRequest.senderIP, friendlyName: transferRequest.senderName },
        ];
      }
      await SaveTransferSettings(settings);
    }
    transferRequest = null;
    await RejectTransfer(id);
    toast("🚫 Transfer rejected", "warn");
  }

  // ── Settings helpers ──────────────────────────────────────────────────────
  async function saveSettings() {
    const result = await SaveTransferSettings(settings);
    if (result === "ok") {
      settingsDirty = false;
      toast("✅ Settings saved", "success");
    } else {
      toast("❌ " + result, "error");
    }
  }

  function addBlockedExt() {
    const ext = newBlockedExt.trim();
    if (!ext) return;
    const formatted = ext.startsWith(".") ? ext : "." + ext;
    if (!settings.blockedExtensions.includes(formatted)) {
      settings.blockedExtensions = [...settings.blockedExtensions, formatted];
      settingsDirty = true;
    }
    newBlockedExt = "";
  }

  function removeBlockedExt(ext) {
    settings.blockedExtensions = settings.blockedExtensions.filter((e) => e !== ext);
    settingsDirty = true;
  }

  function addTrustedDevice() {
    const ip = newTrustedIP.trim();
    if (!ip) return;
    if (!settings.trustedDevices.find((d) => d.ip === ip)) {
      settings.trustedDevices = [
        ...settings.trustedDevices,
        { ip, friendlyName: newTrustedName.trim() || ip },
      ];
      settingsDirty = true;
    }
    newTrustedIP = "";
    newTrustedName = "";
  }

  function removeTrustedDevice(ip) {
    settings.trustedDevices = settings.trustedDevices.filter((d) => d.ip !== ip);
    settingsDirty = true;
  }

  function addBlockedDevice() {
    const ip = newBlockedIP.trim();
    if (!ip) return;
    if (!settings.blockedDevices.find((d) => d.ip === ip)) {
      settings.blockedDevices = [
        ...settings.blockedDevices,
        { ip, friendlyName: newBlockedName.trim() || ip },
      ];
      settingsDirty = true;
    }
    newBlockedIP = "";
    newBlockedName = "";
  }

  function removeBlockedDevice(ip) {
    settings.blockedDevices = settings.blockedDevices.filter((d) => d.ip !== ip);
    settingsDirty = true;
  }

  // Drag & drop
  function handleDragOver(e) {
    e.preventDefault();
    isDragOver = true;
  }
  function handleDragLeave(e) {
    e.preventDefault();
    if (e.clientX === 0 && e.clientY === 0) isDragOver = false;
  }
  function handleDrop(e) {
    e.preventDefault();
    isDragOver = false;
    mode = "SEND";
    startSend();
  }

  // ── Derived (computed once, not on every render tick) ─────────────────────
  $: connLabel = {
    IDLE: "OFFLINE",
    WAITING: "LISTENING",
    CONNECTED: "LINKED",
    DISCONNECTED: "LOST",
  }[connectionState];
  $: connClass = {
    IDLE: "st--idle",
    WAITING: "st--wait",
    CONNECTED: "st--ok",
    DISCONNECTED: "st--err",
  }[connectionState];
  $: displayUrl = serverUrl.replace(/\/\?token=.*$/, "");
  $: sortedFiles = [...receivedFiles]; // backend returns newest-first
</script>

<svelte:window on:mousemove={handleMouseMove} />

<!-- Cursor glow — isolated fixed layer, GPU-promoted via will-change -->
<div class="cursor-glow" bind:this={cursorEl} aria-hidden="true"></div>

<!-- Drop overlay -->
<div
  class="drop-overlay"
  class:visible={isDragOver}
  on:dragover={handleDragOver}
  on:dragleave={handleDragLeave}
  on:drop={handleDrop}
>
  <div class="drop-message blink">[ DROP → INITIATE_SEND ]</div>
  <div class="drop-border"></div>
</div>

<!-- Toast rack -->
<div class="toast-rack" aria-live="polite">
  {#each toasts as t (t.id)}
    <div class="toast toast--{t.type}">{t.msg}</div>
  {/each}
</div>

<!-- App Shell -->
<div class="shell" on:dragover={handleDragOver} on:drop={handleDrop}>
  <!-- ── SIDEBAR ─────────────────────────────────────────────────────────── -->
  <aside class="sidebar">
    <!-- Logo + name -->
    <div class="sidebar__logo">
      <img src={logoImg} alt="BeamSync" class="logo-img" />
      <div>
        <div class="logo-title">BEAMSYNC</div>
        <div class="logo-ver">v2.0</div>
      </div>
    </div>

    <!-- Connection status pill -->
    <div class="conn-pill {connClass}">
      <span class="conn-dot"></span>
      <span class="conn-label">{connLabel}</span>
    </div>

    <!-- Mode tabs -->
    <nav class="sidebar__nav">
      <button
        class="nav-btn"
        class:active={mode === "RECEIVE"}
        on:click={() => switchMode("RECEIVE")}
        on:mouseenter={() => playSound("blip")}
      >
        <span class="nav-icon">⬇</span><span class="nav-label">RECEIVE</span>
      </button>
      <button
        class="nav-btn"
        class:active={mode === "SEND"}
        on:click={() => {
          switchMode("SEND");
          if (mode === "SEND") startSend();
        }}
        on:mouseenter={() => playSound("blip")}
      >
        <span class="nav-icon">⬆</span><span class="nav-label">SEND</span>
      </button>
    </nav>

    <!-- Network info -->
    {#if displayUrl}
      <div class="net-info">
        <div class="net-label">SERVER</div>
        <div class="net-url">{displayUrl}</div>
      </div>
    {/if}

    {#if savePath}
      <div class="save-path-box">
        <div class="save-path-header">
          <span class="save-path-icon">📂</span>
          <span class="save-path-label">SAVE TO</span>
        </div>
        <div class="save-path-value" title={savePath}>
          {savePath.split('/').slice(-2).join('/')}
        </div>
        <button
          class="save-path-btn"
          on:click={changeSavePath}
          on:mouseenter={() => playSound("blip")}
        >
          ✎ CHANGE
        </button>
      </div>
    {/if}

    <div class="sidebar__spacer"></div>

    <!-- About nav at the bottom -->
    <button
      class="nav-btn nav-btn--about"
      class:active={mode === "SETTINGS"}
      on:click={() => {
        playSound("blip");
        mode = "SETTINGS";
      }}
      on:mouseenter={() => playSound("blip")}
    >
      <span class="nav-icon">⚙</span><span class="nav-label">SETTINGS</span>
    </button>

    <button
      class="nav-btn nav-btn--about"
      class:active={mode === "ABOUT"}
      on:click={() => {
        playSound("blip");
        mode = "ABOUT";
      }}
      on:mouseenter={() => playSound("blip")}
    >
      <span class="nav-icon">◈</span><span class="nav-label">ABOUT</span>
    </button>

    <button
      class="terminate-btn"
      on:click={resetAll}
      on:mouseenter={() => playSound("blip")}
      title="Terminate all connections"
    >
      ☠ TERMINATE
    </button>
  </aside>

  <!-- ── MAIN ───────────────────────────────────────────────────────────── -->
  <main class="main">
    <!-- RECEIVE mode -->
    {#if mode === "RECEIVE"}
      {#if connectionState !== "CONNECTED"}
        <section class="panel qr-panel">
          <div class="panel__header">
            <h2 class="panel__title">
              {#if connectionState === "WAITING"}
                <Typewriter text="// WAITING_FOR_UPLINK..." speed={30} />
              {:else if connectionState === "DISCONNECTED"}
                // SIGNAL_LOST
              {:else}
                // STANDBY
              {/if}
            </h2>
          </div>

          <div class="qr-stage">
            {#if qrImage}
              <div
                class="qr-wrap"
                class:qr-pulse={connectionState === "WAITING"}
              >
                <div class="qr-label-top">DATA_LINK</div>
                <img
                  src={qrImage}
                  alt="Connection QR Code"
                  class="qr-img"
                  draggable="false"
                />
                {#if connectionState === "WAITING"}
                  <div class="qr-scan-line" aria-hidden="true"></div>
                {/if}
              </div>
            {:else}
              <div class="qr-loading blink">GENERATING_LINK...</div>
            {/if}
          </div>

          <div class="qr-instructions">
            <div class="instr-row">
              <span class="instr-num">01</span><span
                >Connect both devices to the same Wi-Fi network</span
              >
            </div>
            <div class="instr-row">
              <span class="instr-num">02</span><span
                >Scan the QR code or open the URL on your phone</span
              >
            </div>
            <div class="instr-row">
              <span class="instr-num">03</span><span
                >Select files and tap Upload</span
              >
            </div>
          </div>

          {#if connectionState === "DISCONNECTED"}
            <button
              class="action-btn action-btn--cyan"
              on:click={handleDisconnectReset}
              on:mouseenter={() => playSound("blip")}
            >
              ↺ RECONNECT
            </button>
          {/if}

          {#if displayUrl}
            <div class="url-strip">
              <span class="url-strip__label">URL</span>
              <span class="url-strip__val">{displayUrl}</span>
              <button
                class="url-strip__copy"
                on:click={() => {
                  navigator.clipboard.writeText(displayUrl);
                  toast("Copied!", "info");
                }}>COPY</button
              >
            </div>
          {/if}
        </section>
      {:else}
        <!-- Connected dashboard -->
        <section class="panel connected-panel">
          <div class="panel__header">
            <h2 class="panel__title connected-title">// LINK_ESTABLISHED</h2>
            <div class="conn-badge">DEVICE ONLINE</div>
          </div>

          {#if progress.active}
            <div class="transfer-block">
              <div class="transfer-header">
                <span class="transfer-icon">📡</span>
                <div class="transfer-info">
                  <div class="transfer-name">{progress.filename}</div>
                  <div class="transfer-meta">
                    <span class="transfer-meta__item"
                      >{progress.received}/{progress.total}</span
                    >
                    <span class="transfer-meta__item" class:transfer-meta__speed={progress.speed}>
                      <span
                        class="speed-dot"
                        style="background-color: {progress.speedColor};"
                      ></span>
                      {progress.speed}
                    </span>
                  </div>
                  <div class="transfer-submeta">
                    <span>⏱️ {progress.totalTime}</span>
                    <span>⌛ {progress.timeRemaining} remaining</span>
                  </div>
                </div>
                <div class="transfer-pct">{progress.percent}%</div>
              </div>
              <div class="transfer-bar-track">
                <div
                  class="transfer-bar-fill"
                  style="width:{progress.percent}%; background-color: {progress.speedColor};"
                ></div>
              </div>
            </div>
          {:else}
            <div class="ready-msg">
              <span class="ready-icon blink">▶</span>
              <span>READY — Waiting for incoming files…</span>
            </div>
          {/if}

          <div class="file-log">
            <div class="file-log__header">
              <span>📥 RECEIVED FILES</span>
              <span class="file-log__count"
                >{receivedFiles.length} file{receivedFiles.length !== 1
                  ? "s"
                  : ""}</span
              >
            </div>
            {#if sortedFiles.length > 0}
              <div class="file-list">
                {#each sortedFiles as file (file.name + file.modTime)}
                  <button class="file-row" on:click={() => openFile(file.name)}>
                    <span class="file-row__icon">{fileIcon(file.name)}</span>
                    <span class="file-row__name">{file.name}</span>
                    <span class="file-row__size"
                      >{formatSize(file.sizeBytes)}</span
                    >
                    <span class="file-row__time">{file.modTime}</span>
                    <span class="file-row__open">↗</span>
                  </button>
                {/each}
              </div>
            {:else}
              <div class="file-empty">
                No files yet — waiting for transmission…
              </div>
            {/if}
          </div>
        </section>
      {/if}

      <!-- SEND mode -->
    {:else if mode === "SEND"}
      <section class="panel send-panel">
        <div class="panel__header">
          <h2 class="panel__title send-title">// UPLINK_READY</h2>
        </div>
        <div
          class="send-drop-zone"
          on:click={startSend}
          role="button"
          tabindex="0"
          on:keydown={(e) => e.key === "Enter" && startSend()}
        >
          <div class="send-drop__icon">⬆</div>
          <div class="send-drop__primary">Click to select files</div>
          <div class="send-drop__secondary">or drag &amp; drop anywhere</div>
        </div>
        <div class="send-info-cards">
          <div class="info-card">
            <div class="info-card__icon">🔒</div>
            <div>
              <div class="info-card__title">Secure</div>
              <div class="info-card__desc">
                Token-authenticated local transfer
              </div>
            </div>
          </div>
          <div class="info-card">
            <div class="info-card__icon">⚡</div>
            <div>
              <div class="info-card__title">Fast</div>
              <div class="info-card__desc">Direct LAN — no cloud required</div>
            </div>
          </div>
          <div class="info-card">
            <div class="info-card__icon">📡</div>
            <div>
              <div class="info-card__title">Multi-file</div>
              <div class="info-card__desc">Send multiple files at once</div>
            </div>
          </div>
        </div>
      </section>

      <!-- SETTINGS mode -->
    {:else if mode === "SETTINGS"}
      <section class="panel settings-panel">
        <div class="panel__header">
          <h2 class="panel__title">// TRANSFER_PERMISSIONS</h2>
        </div>

        <!-- Transfer Mode -->
        <div class="settings-group">
          <div class="settings-group__title">⚡ TRANSFER MODE</div>
          <div class="settings-radios">
            {#each [
              { val: "accept_all", label: "Accept All", desc: "Automatically accept everything" },
              { val: "ask_first", label: "Ask First", desc: "Show approval prompt for every transfer" },
              { val: "trusted_only", label: "Trusted Devices Only", desc: "Auto-accept from approved devices" },
              { val: "block_all", label: "Block All", desc: "Reject all incoming transfers" },
            ] as opt}
              <label class="settings-radio" class:active={settings.mode === opt.val}>
                <input type="radio" name="mode" value={opt.val}
                  bind:group={settings.mode}
                  on:change={() => (settingsDirty = true)} />
                <div class="settings-radio__content">
                  <div class="settings-radio__label">{opt.label}</div>
                  <div class="settings-radio__desc">{opt.desc}</div>
                </div>
              </label>
            {/each}
          </div>
        </div>

        <!-- File Restrictions -->
        <div class="settings-group">
          <div class="settings-group__title">📁 FILE RESTRICTIONS</div>
          <div class="settings-field">
            <label class="settings-label">Max file size (MB) — 0 = unlimited</label>
            <input class="settings-input" type="number" min="0"
              bind:value={settings.maxFileSizeMB}
              on:input={() => (settingsDirty = true)} />
          </div>
          <div class="settings-field">
            <label class="settings-label">Blocked extensions</label>
            <div class="tag-row">
              {#each settings.blockedExtensions as ext}
                <span class="tag tag--red">{ext}
                  <button class="tag__rm" on:click={() => removeBlockedExt(ext)}>✕</button>
                </span>
              {/each}
            </div>
            <div class="settings-add-row">
              <input class="settings-input" placeholder=".exe" bind:value={newBlockedExt}
                on:keydown={(e) => e.key === "Enter" && addBlockedExt()} />
              <button class="settings-add-btn" on:click={addBlockedExt}>+ ADD</button>
            </div>
          </div>
        </div>

        <!-- Trusted Devices -->
        <div class="settings-group">
          <div class="settings-group__title">✅ TRUSTED DEVICES</div>
          {#each settings.trustedDevices as dev}
            <div class="device-row">
              <span class="device-row__name">{dev.friendlyName || dev.ip}</span>
              <span class="device-row__ip">{dev.ip}</span>
              <button class="device-row__rm" on:click={() => removeTrustedDevice(dev.ip)}>✕</button>
            </div>
          {/each}
          <div class="settings-add-row">
            <input class="settings-input" placeholder="IP address" bind:value={newTrustedIP} />
            <input class="settings-input" placeholder="Name (optional)" bind:value={newTrustedName}
              on:keydown={(e) => e.key === "Enter" && addTrustedDevice()} />
            <button class="settings-add-btn" on:click={addTrustedDevice}>+ ADD</button>
          </div>
        </div>

        <!-- Blocked Devices -->
        <div class="settings-group">
          <div class="settings-group__title">🚫 BLOCKED DEVICES</div>
          {#each settings.blockedDevices as dev}
            <div class="device-row device-row--blocked">
              <span class="device-row__name">{dev.friendlyName || dev.ip}</span>
              <span class="device-row__ip">{dev.ip}</span>
              <button class="device-row__rm" on:click={() => removeBlockedDevice(dev.ip)}>✕</button>
            </div>
          {/each}
          <div class="settings-add-row">
            <input class="settings-input" placeholder="IP address" bind:value={newBlockedIP} />
            <input class="settings-input" placeholder="Name (optional)" bind:value={newBlockedName}
              on:keydown={(e) => e.key === "Enter" && addBlockedDevice()} />
            <button class="settings-add-btn" on:click={addBlockedDevice}>+ ADD</button>
          </div>
        </div>

        {#if settingsDirty}
          <button class="action-btn action-btn--cyan" on:click={saveSettings}>
            💾 SAVE SETTINGS
          </button>
        {:else}
          <div class="settings-saved-hint">All changes saved</div>
        {/if}
      </section>

      <!-- ABOUT mode -->
    {:else if mode === "ABOUT"}
      <section class="panel about-panel">
        <!-- App block -->
        <div class="about-app">
          <img
            src={logoImg}
            alt="BeamSync"
            class="about-app__logo"
            draggable="false"
          />
          <div class="about-app__info">
            <div class="about-app__name">BEAMSYNC</div>
            <div class="about-app__version">v2.0.0</div>
            <p class="about-app__desc">
              Fast, token-secured file transfers over your local network. No
              cloud. No accounts. Just beam it.
            </p>
            <div class="about-app__chips">
              <span class="chip">🔒 LAN Only</span>
              <span class="chip">⚡ Zero Cloud</span>
              <span class="chip">📡 Real-time Progress</span>
              <span class="chip">🖥 Desktop + Mobile</span>
            </div>
          </div>
        </div>

        <div class="about-divider">
          <span class="about-divider__line"></span>
          <span class="about-divider__label">// DEVELOPER</span>
          <span class="about-divider__line"></span>
        </div>

        <!-- Developer block -->
        <div class="about-dev">
          <div class="about-dev__avatar-wrap">
            <img
              src={logoImg}
              alt="Pranav Agarkar"
              class="about-dev__avatar"
              draggable="false"
            />
            <div class="about-dev__avatar-ring"></div>
          </div>
          <div class="about-dev__info">
            <div class="about-dev__name">Pranav Agarkar</div>
            <div class="about-dev__tagline">
              // Building tools that make life simpler — one commit at a time
            </div>
            <div class="about-dev__links">
              <button
                class="link-btn link-btn--gh"
                on:click={() => openLink("https://github.com/PranavAgarkar07")}
                on:mouseenter={() => playSound("blip")}
              >
                <span>⌥</span> GitHub
              </button>
              <button
                class="link-btn link-btn--web"
                on:click={() =>
                  openLink(
                    "https://pranavagarkar07.github.io/portfolio-svelte/",
                  )}
                on:mouseenter={() => playSound("blip")}
              >
                <span>⬡</span> Portfolio
              </button>
            </div>
          </div>
        </div>

        <div class="about-footer">
          Built with Wails · Go · Svelte &nbsp;·&nbsp; © 2025 Pranav Agarkar
        </div>
      </section>
    {/if}
  </main>
</div>

<!-- Transfer Request Approval Dialog -->
{#if transferRequest}
  <div class="dialog-backdrop">
    <div class="dialog transfer-dialog">
      <div class="dialog__corner tl"></div>
      <div class="dialog__corner tr"></div>
      <div class="dialog__corner bl"></div>
      <div class="dialog__corner br"></div>
      <h2 class="dialog__title">// INCOMING_TRANSFER</h2>
      <p class="dialog__sub">Someone wants to send you a file</p>

      <div class="transfer-req-info">
        <div class="transfer-req-row">
          <span class="transfer-req-label">FROM</span>
          <span class="transfer-req-val">{transferRequest.senderName || transferRequest.senderIP}</span>
        </div>
        <div class="transfer-req-row">
          <span class="transfer-req-label">FILE</span>
          <span class="transfer-req-val">{transferRequest.filename}</span>
        </div>
        <div class="transfer-req-row">
          <span class="transfer-req-label">SIZE</span>
          <span class="transfer-req-val">{transferRequest.sizeMB}</span>
        </div>
        <div class="transfer-req-row">
          <span class="transfer-req-label">TYPE</span>
          <span class="transfer-req-val">{transferRequest.mimeType || "unknown"}</span>
        </div>
      </div>

      <label class="transfer-req-remember">
        <input type="checkbox" bind:checked={rememberDevice} />
        <span>Remember my choice for this device</span>
      </label>

      <div class="transfer-req-btns">
        <button class="action-btn action-btn--red" on:click={rejectTransfer}
          on:mouseenter={() => playSound("blip")}>
          ✕ REJECT
        </button>
        <button class="action-btn action-btn--cyan" on:click={approveTransfer}
          on:mouseenter={() => playSound("blip")}>
          ✓ ACCEPT
        </button>
      </div>
    </div>
  </div>
{/if}

<!-- Sender URL dialog -->
{#if showSenderDialog}
  <div class="dialog-backdrop" on:click|self={() => (showSenderDialog = false)}>
    <div class="dialog">
      <div class="dialog__corner tl"></div>
      <div class="dialog__corner tr"></div>
      <div class="dialog__corner bl"></div>
      <div class="dialog__corner br"></div>
      <button class="dialog__close" on:click={() => (showSenderDialog = false)}
        >✕</button
      >
      <h2 class="dialog__title">// PAYLOAD_READY</h2>
      <p class="dialog__sub">Scan on receiving device to download</p>
      {#if qrImage}
        <div class="dialog__qr-wrap">
          <img
            src={qrImage}
            alt="Sender QR Code"
            class="dialog__qr"
            draggable="false"
          />
        </div>
      {/if}
      <div class="dialog__url-row">
        <input
          class="dialog__url-input"
          type="text"
          value={senderUrl}
          readonly
        />
        <button
          class="dialog__copy-btn"
          on:click={() => {
            navigator.clipboard.writeText(senderUrl);
            toast("URL copied!", "success");
          }}>COPY</button
        >
      </div>
      <button
        class="action-btn action-btn--amber"
        on:click={() => (showSenderDialog = false)}>CLOSE</button
      >
    </div>
  </div>
{/if}
