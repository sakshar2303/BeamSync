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
  import {
    EventsOn,
    EventsOffAll,
    BrowserOpenURL,
  } from "../wailsjs/runtime/runtime.js";
  import QRCode from "qrcode";
  import { onMount, onDestroy } from "svelte";
  import { fly } from "svelte/transition";
  import Typewriter from "./Typewriter.svelte";

  import {
    TopNavBar,
    FileDropZone,
    TransferProgressBar,
    TransferComplete,
    ConnectedDevicesPanel,
  } from "./design-system/index.js";

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
  // ── Sound toggle ────────────────────────────────────────────────────────
  let soundEnabled = localStorage.getItem("beamsync_sound") !== "false";
  function toggleSound() {
    soundEnabled = !soundEnabled;
    localStorage.setItem("beamsync_sound", soundEnabled ? "true" : "false");
    if (soundEnabled) PlaySound("blip"); // confirm it's on
  }

  // ── Batch transfer tracking ──────────────────────────────────────────
  // Count files received in the current upload session so we can play
  // the success tone only once at the end instead of once per file.
  let batchCount = 0; // files received this session
  let batchTimer = null; // resets batchCount after idle
  let showTickAnim = false; // drives the "all done" tick overlay
  let lastBatchCount = 0;

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
  function handleMouseMove(e) {
    // legacy mouse glow removed
  }

  // ── Mount / Unmount ─────────────────────────────────────────────────────
  onMount(async () => {
    // 💡 Fix for Wails Dev Mode Hot-Reloads:
    // Clear any zombie listeners from previous hmr reloads that had soundEnabled=true
    EventsOffAll();

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

      // Batch tracking: accumulate count, reset the "all done" idle timer.
      // The timer is also cancelled inside upload_progress, so it only fires
      // when there has been no transfer activity at all for 2.5 s.
      batchCount += 1;
      clearTimeout(batchTimer);
      batchTimer = setTimeout(() => {
        if (batchCount > 0) {
          playSound("success");
          lastBatchCount = batchCount;
          showTickAnim = true;
          batchCount = 0;
        }
      }, 2500);

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
      const avg = speedHistory.reduce((a, b) => a + b, 0) / speedHistory.length;
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
        total > 0 ? Math.min(100, Math.round((written / total) * 100)) : -1;

      progress = {
        active: true,
        filename,
        percent: pct,
        speed: speedStr,
        received: `${(written / 1048576).toFixed(2)} MB`,
        total: total > 0 ? `${(total / 1048576).toFixed(2)} MB` : 'Unknown',
        timeRemaining,
        totalTime: totalTimeStr,
        speedColor,
      };

      if (connectionState !== "CONNECTED") connectionState = "CONNECTED";
      // If a new file is actively streaming, cancel the batch-complete timer —
      // we are NOT done yet.
      clearTimeout(batchTimer);
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

  onDestroy(() => {
    EventsOffAll();
    clearTimeout(batchTimer);
    clearTimeout(_progressTimeout);
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
        color: { dark: "#0A0A0A", light: "#00000000" },
      },
      (err, url) => {
        if (!err) qrImage = url;
      },
    );
  }

  function playSound(type) {
    if (soundEnabled) PlaySound(type);
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
      total: "0.00 MB",
      timeRemaining: "—",
      totalTime: "0s",
      speedColor: "#ffb000",
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

<!-- Drop zone layer (always active behind nav for drag-drop SEND initiation) -->
<div
  class="app-dropzone"
  on:dragover={handleDragOver}
  on:drop={handleDrop}
  on:dragleave={handleDragLeave}
>
  {#if isDragOver}
    <div class="drop-overlay">
      <div class="drop-message">[ DROP → INITIATE_SEND ]</div>
    </div>
  {/if}

  <!-- Toast rack -->
  <div class="toast-rack" aria-live="polite">
    {#each toasts as t (t.id)}
      <div class="toast toast--{t.type}">{t.msg}</div>
    {/each}
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
  <div id="app" class="nb-theme">
    <TopNavBar
      activeTab={mode.toLowerCase()}
      networkStatus={connectionState.toLowerCase()}
      serverUrl={displayUrl}
      appVersion="v2.2"
      on:tabChange={({ detail }) => switchMode(detail.tab.toUpperCase())}
      on:settings={toggleSound}
      on:reset={handleDisconnectReset}
    />

    <main class="main-content">
      {#if mode === "RECEIVE"}
        <div class="mode-wrapper" in:fly={{ y: 15, duration: 250 }}>
        {#if connectionState !== "CONNECTED"}
          <div class="receive-standby">
            <div class="nb-card home-card">
              <div class="home-card__header">
                <div
                  class="status-indicator"
                  class:pulse={connectionState === "WAITING"}
                ></div>
                <h1 class="standby-title">
                  {#if connectionState === "WAITING"}
                    Connect via {serverUrl
                      .replace(/^https?:\/\//, "")
                      .split(":")[0] || "Wi-Fi"}
                  {:else if connectionState === "DISCONNECTED"}
                    Connection Lost
                  {:else}
                    Ready to Connect
                  {/if}
                </h1>
              </div>

              <div class="home-card__body">
                {#if qrImage}
                  <div class="qr-wrapper">
                    <img
                      src={qrImage}
                      alt="QR Code"
                      class="qr-code"
                      draggable="false"
                    />
                  </div>
                {:else}
                  <div class="qr-wrapper qr-loading">GENERATING_LINK...</div>
                {/if}

                <div class="instructions-list">
                  <div class="instr-step">
                    <span class="step-num">1</span> Connect to same Wi-Fi
                  </div>
                  <div class="instr-step">
                    <span class="step-num">2</span> Scan QR code
                  </div>
                  <div class="instr-step">
                    <span class="step-num">3</span> Select files
                  </div>
                </div>
              </div>

              <div class="home-card__footer">
                {#if displayUrl}
                  <div class="url-group">
                    <span class="url-text">{displayUrl}</span>
                    <button
                      class="nb-btn nb-btn--primary"
                      on:click={() => {
                        navigator.clipboard.writeText(displayUrl);
                        toast("Copied!", "success");
                      }}>COPY</button
                    >
                  </div>
                {/if}

                <div class="save-path-row">
                  <span class="save-path-lbl nb-badge">Save to</span>
                  <span class="save-path-val">{savePath || "Default"}</span>
                  <button
                    class="nb-btn nb-btn--ghost nb-btn--sm"
                    style="padding: 4px 10px; font-size: 0.75rem;"
                    on:click={changeSavePath}>CHANGE</button
                  >
                </div>
              </div>
            </div>

            {#if connectionState === "DISCONNECTED"}
              <button
                class="nb-btn nb-btn--danger reconnect-btn"
                on:click={handleDisconnectReset}>RECONNECT</button
              >
            {/if}
          </div>
        {:else}
          <!-- Connected Receive Mode -->
          <div class="receive-active">
            <h2 class="active-title">Device Connected</h2>

            {#if progress.active}
              <TransferProgressBar
                filename={progress.filename}
                percent={progress.percent}
                speed={progress.speed}
                received={progress.received}
                total={progress.total}
                eta={progress.timeRemaining}
                elapsed={progress.totalTime}
                role="receiver"
                active={true}
                on:cancel={() => resetAll()}
              />
            {:else}
              <div class="ready-banner pulse-bg">
                <div class="radar-ping"></div>
                <div class="ready-content">
                  <span class="status-badge">READY</span>
                  <span class="status-text">WAITING FOR FILES...</span>
                </div>
              </div>
            {/if}

            <div class="files-panel">
              <div class="files-header">
                <h3>RECEIVED FILES ({receivedFiles.length})</h3>
              </div>
              <div class="files-list" class:empty={receivedFiles.length === 0}>
                {#if receivedFiles.length === 0}
                  <div class="empty-state">
                    <svg width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="square"><rect x="3" y="3" width="18" height="18" rx="0" ry="0"/><line x1="9" y1="3" x2="9" y2="21"/><path d="M13 8h4"/><path d="M13 12h4"/></svg>
                    <p>INBOX EMPTY<br><small>Incoming data will appear here</small></p>
                  </div>
                {/if}
                {#each sortedFiles as file}
                  <button
                    class="file-item"
                    on:click={() => openFile(file.name)}
                  >
                    <span class="file-icon">{fileIcon(file.name)}</span>
                    <span class="file-name">{file.name}</span>
                    <span class="file-size">{formatSize(file.sizeBytes)}</span>
                    <span class="file-time">{file.modTime}</span>
                  </button>
                {/each}
              </div>
            </div>
          </div>
        {/if}
        </div>
      {:else if mode === "SEND"}
        <div class="mode-wrapper send-layout" in:fly={{ y: 15, duration: 250 }}>
          <FileDropZone on:selectFiles={startSend} on:requestPicker={startSend} />

          {#if showSenderDialog}
            <div class="sender-dialog">
              <div class="sender-header">
                <span class="radar-ping-small"></span>
                <h3>READY TO SEND</h3>
              </div>
              <p class="sender-desc">Scan the QR code on the receiving device to download</p>
              
              {#if qrImage}
                <div class="qr-frame">
                  <img src={qrImage} alt="Sender QR" class="sender-qr" />
                </div>
              {/if}
              
              <div class="url-action-bar">
                <span class="url-label">Or share this link:</span>
                <div class="url-box">
                  <input class="url-input nb-mono" readonly value={senderUrl} />
                  <button
                    class="nb-btn nb-btn--primary"
                    on:click={() => {
                      navigator.clipboard.writeText(senderUrl);
                      toast("Link copied!", "success");
                    }}>COPY</button
                  >
                </div>
              </div>
              
              <button
                class="nb-btn nb-btn--danger close-btn"
                on:click={() => (showSenderDialog = false)}>CLOSE SESSION</button
              >
            </div>
          {/if}
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
      {:else if mode === "ABOUT"}
        <div class="mode-wrapper about-layout" in:fly={{ y: 15, duration: 250 }}>
          <div class="about-card">
            <div class="about-header">
              <div class="logo-box">
                <img src={logoImg} class="about-logo" alt="BeamSync Logo" />
              </div>
              <div class="about-title">
                <h1>BEAMSYNC</h1>
                <span class="version-badge">v2.2</span>
              </div>
            </div>
            
            <p class="about-desc">
              Fast, token-secured file transfers over your local network. No
              cloud. No accounts.
            </p>
            
            <div class="about-tags">
              <span class="nb-badge nb-badge--info">LAN ONLY</span>
              <span class="nb-badge nb-badge--success">ZERO CLOUD</span>
            </div>
          </div>

          <div class="developer-card">
            <div class="dev-header">
              <h3>SYSTEM ARCHITECT</h3>
            </div>
            <div class="dev-body">
              <span class="dev-name">Pranav Agarkar</span>
              <div class="about-links">
                <button
                  class="nb-btn nb-btn--primary"
                  on:click={() => openLink("https://github.com/PranavAgarkar07")}
                  >GITHUB</button
                >
                <button
                  class="nb-btn nb-btn--secondary"
                  on:click={() =>
                    openLink("https://pranavagarkar07.github.io/portfolio-svelte/")}
                  >PORTFOLIO</button
                >
              </div>
            </div>
          </div>
        </div>
      {/if}
    </main>
  </div>

  {#if showTickAnim}
    <TransferComplete
      show={true}
      fileCount={lastBatchCount}
      on:dismiss={() => (showTickAnim = false)}
    />
  {/if}
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
<style>
  /* 
   * Local App.svelte styles for new layout composition 
   * All design systems tokens are global and handled by app.css/tokens.css
   */
  .app-dropzone {
    width: 100vw;
    height: 100vh;
    position: relative;
  }

  .drop-overlay {
    position: fixed;
    inset: 0;
    z-index: 1000;
    background: rgba(0, 0, 0, 0.85);
    display: flex;
    align-items: center;
    justify-content: center;
    border: 4px dashed var(--nb-primary);
  }

  .drop-message {
    background: var(--nb-surface);
    border: var(--nb-border-lg);
    font-size: var(--nb-text-2xl);
    font-weight: var(--nb-fw-bold);
    padding: var(--nb-space-4) var(--nb-space-6);
    box-shadow: var(--nb-shadow-lg);
  }

  /* Main Nav/Content Setup */
  #app {
    display: flex;
    flex-direction: column;
    height: 100%;
  }

  .main-content {
    flex: 1;
    overflow-y: auto;
    padding: var(--nb-space-6) var(--nb-space-8);
    display: flex;
    flex-direction: column;
    align-items: center;
  }

  /* Receive Standby */
  .receive-standby,
  .receive-active,
  .about-layout,
  .send-layout {
    width: 100%;
    max-width: 800px;
    display: flex;
    flex-direction: column;
    gap: var(--nb-space-6);
  }

  .receive-standby {
    align-items: center;
    width: 100%;
    max-width: 500px;
    margin: 0 auto;
    margin-top: var(--nb-space-4);
  }

  .home-card {
    width: 100%;
    padding: 0;
    overflow: hidden;
    display: flex;
    flex-direction: column;
  }

  .home-card__header {
    background: var(--nb-primary);
    color: var(--nb-primary-text, #ffffff);
    padding: var(--nb-space-4) var(--nb-space-5);
    border-bottom: var(--nb-border-lg);
    display: flex;
    align-items: center;
    justify-content: center;
    gap: var(--nb-space-4);
  }

  .status-indicator {
    width: 16px;
    height: 16px;
    background: #00e676; /* Neon green ping */
    border: 2px solid var(--nb-border-color);
    border-radius: 50%;
  }

  .pulse {
    animation: pulse 1.5s infinite alternate;
  }

  @keyframes pulse {
    0% {
      transform: scale(0.85);
      box-shadow: 0 0 0 0 rgba(0, 230, 118, 0.4);
    }
    100% {
      transform: scale(1.15);
      box-shadow: 0 0 0 6px rgba(0, 230, 118, 0);
    }
  }

  .standby-title {
    font-size: var(--nb-text-xl);
    font-family: var(--nb-font-mono);
    font-weight: 800;
    color: var(--nb-primary-text, #ffffff);
    margin: 0;
    line-height: 1.1;
    letter-spacing: -0.04em;
    text-transform: uppercase;
  }

  .home-card__body {
    padding: var(--nb-space-6) var(--nb-space-4);
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--nb-space-6);
    background: var(--nb-surface);
  }

  .qr-wrapper {
    background: #ffffff;
    padding: var(--nb-space-4);
    border: var(--nb-border-lg);
    box-shadow: 8px 8px 0px var(--nb-border-color);
    transition: transform 0.2s, box-shadow 0.2s;
  }
  .qr-wrapper:hover {
    transform: translate(-4px, -4px);
    box-shadow: 12px 12px 0px var(--nb-border-color);
  }

  .qr-code {
    width: 220px;
    height: 220px;
    display: block;
  }

  .qr-loading {
    width: 220px;
    height: 220px;
    display: flex;
    align-items: center;
    justify-content: center;
    font-family: var(--nb-font-mono);
    font-weight: 800;
    color: #0a0a0a;
  }

  .instructions-list {
    display: flex;
    flex-direction: column;
    gap: var(--nb-space-3);
    width: 100%;
    max-width: 380px;
  }

  .instr-step {
    display: flex;
    align-items: center;
    gap: var(--nb-space-4);
    font-family: var(--nb-font-body);
    font-size: var(--nb-text-base);
    font-weight: var(--nb-fw-bold);
    letter-spacing: -0.01em;
    padding: var(--nb-space-3) var(--nb-space-4);
    background: var(--nb-bg);
    border: var(--nb-border-lg);
    box-shadow: 4px 4px 0px var(--nb-border-color);
    color: var(--nb-text);
  }

  .step-num {
    background: var(--nb-secondary);
    color: #0a0a0a;
    font-family: var(--nb-font-mono);
    font-weight: 800;
    font-size: var(--nb-text-lg);
    width: 32px;
    height: 32px;
    display: flex;
    align-items: center;
    justify-content: center;
    border: var(--nb-border-lg);
    flex-shrink: 0;
  }

  .home-card__footer {
    padding: var(--nb-space-4);
    background: var(--nb-bg);
    border-top: var(--nb-border-lg);
    display: flex;
    flex-direction: column;
    gap: var(--nb-space-4);
  }

  .url-group {
    display: flex;
    align-items: stretch;
    border: var(--nb-border-lg);
    background: var(--nb-surface);
    overflow: hidden;
    box-shadow: 4px 4px 0px var(--nb-border-color);
  }

  .url-text {
    flex: 1;
    font-family: var(--nb-font-mono);
    font-size: var(--nb-text-base);
    font-weight: 800;
    color: var(--nb-text);
    padding: 0 var(--nb-space-4);
    display: flex;
    align-items: center;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .url-group .nb-btn {
    border: none;
    border-left: var(--nb-border-lg);
    border-radius: 0;
    margin: 0;
    box-shadow: none;
    font-size: var(--nb-text-sm);
    padding: 0 var(--nb-space-6);
  }
  .url-group .nb-btn:hover {
    transform: none;
    background: var(--nb-primary);
    color: var(--nb-primary-text, #ffffff);
  }

  .save-path-row {
    font-size: var(--nb-text-sm);
    font-family: var(--nb-font-mono);
    font-weight: 700;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--nb-space-3);
    background: var(--nb-surface);
    border: var(--nb-border-lg);
    border-style: dashed;
    padding: var(--nb-space-3) var(--nb-space-4);
  }

  .save-path-val {
    flex: 1;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--nb-text-muted);
  }

  .save-path-val {
    flex: 1;
    min-width: 0;
    font-family: var(--nb-font-mono);
    font-size: var(--nb-text-xs);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .reconnect-btn {
    margin-top: var(--nb-space-4);
  }

  /* Receive Active Components */
  .active-title {
    font-size: var(--nb-text-xl);
    border-bottom: var(--nb-border-lg);
    padding-bottom: var(--nb-space-2);
  }

  .ready-banner {
    padding: var(--nb-space-4);
    background: var(--nb-surface);
    border: var(--nb-border-lg);
    box-shadow: var(--nb-shadow-md);
    margin-bottom: var(--nb-space-5);
    display: flex;
    align-items: center;
    gap: var(--nb-space-4);
    position: relative;
    overflow: hidden;
  }

  .pulse-bg {
    background: repeating-linear-gradient(45deg, var(--nb-primary) 0, var(--nb-primary) 2px, transparent 2px, transparent 10px);
    background-color: var(--nb-bg);
  }

  .ready-content {
    display: flex;
    align-items: center;
    gap: var(--nb-space-3);
    background: var(--nb-surface);
    padding: var(--nb-space-2) var(--nb-space-4);
    border: var(--nb-border-lg);
    box-shadow: var(--nb-shadow-sm);
    z-index: 1;
  }

  .status-badge {
    background: var(--nb-secondary);
    color: var(--nb-secondary-text);
    padding: 4px 8px;
    font-family: var(--nb-font-display);
    font-weight: 800;
    font-size: var(--nb-text-sm);
    border: var(--nb-border-md);
  }

  .status-text {
    font-family: var(--nb-font-mono);
    font-weight: var(--nb-fw-bold);
    color: var(--nb-text);
  }

  .radar-ping {
    position: absolute;
    right: 30px;
    width: 20px;
    height: 20px;
    background: var(--nb-secondary);
    border-radius: 50%;
    animation: ping 2s cubic-bezier(0, 0, 0.2, 1) infinite;
  }

  @keyframes ping {
    75%, 100% {
      transform: scale(3);
      opacity: 0;
    }
  }

  .files-panel {
    background: var(--nb-surface);
    border: var(--nb-border-lg);
    box-shadow: var(--nb-shadow-md);
    display: flex;
    flex-direction: column;
  }

  .files-header {
    background: var(--nb-bg);
    padding: var(--nb-space-3) var(--nb-space-4);
    border-bottom: var(--nb-border-lg);
  }

  .files-header h3 {
    font-size: var(--nb-text-sm);
    letter-spacing: 0.05em;
  }

  .files-list {
    max-height: 300px;
    overflow-y: auto;
  }

  .files-list.empty {
    padding: var(--nb-space-6);
  }

  .empty-state {
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    gap: var(--nb-space-3);
    color: var(--nb-text-muted);
    text-align: center;
    padding: var(--nb-space-4);
  }

  .empty-state svg {
    color: var(--nb-primary);
    margin-bottom: var(--nb-space-2);
  }

  .empty-state p {
    font-family: var(--nb-font-display);
    font-weight: 800;
    font-size: var(--nb-text-lg);
    line-height: 1.2;
    margin: 0;
    color: var(--nb-text);
  }

  .empty-state small {
    font-family: var(--nb-font-mono);
    font-weight: 400;
    font-size: var(--nb-text-sm);
    color: var(--nb-text-muted);
  }

  .file-item {
    display: flex;
    align-items: center;
    gap: var(--nb-space-4);
    width: 100%;
    padding: var(--nb-space-3) var(--nb-space-4);
    border-bottom: 1px solid var(--nb-border-color);
    text-align: left;
    color: var(--nb-text);
  }

  .file-item:hover {
    background: var(--nb-bg);
  }

  .file-name {
    flex: 1;
    font-weight: var(--nb-fw-bold);
  }
  .file-size,
  .file-time {
    font-family: var(--nb-font-mono);
    font-size: var(--nb-text-xs);
    color: var(--nb-text-muted);
  }

  /* Sender Dialog */
  .sender-dialog {
    padding: var(--nb-space-6);
    background: var(--nb-surface);
    border: var(--nb-border-lg);
    box-shadow: 6px 6px 0px var(--nb-shadow-color);
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--nb-space-4);
    margin-top: var(--nb-space-6);
  }

  .sender-header {
    display: flex;
    align-items: center;
    gap: var(--nb-space-3);
    background: var(--nb-bg);
    border: var(--nb-border-md);
    padding: var(--nb-space-2) var(--nb-space-4);
  }

  .sender-header h3 {
    margin: 0;
    font-family: var(--nb-font-display);
    font-size: var(--nb-text-lg);
    font-weight: 800;
  }

  .sender-desc {
    color: var(--nb-text);
    font-weight: var(--nb-fw-bold);
    margin-bottom: var(--nb-space-2);
    text-align: center;
  }

  .radar-ping-small {
    width: 12px;
    height: 12px;
    background: var(--nb-primary);
    border-radius: 50%;
    animation: ping 1.5s cubic-bezier(0, 0, 0.2, 1) infinite;
  }

  .qr-frame {
    background: var(--nb-bg);
    padding: var(--nb-space-4);
    border: var(--nb-border-lg);
    box-shadow: var(--nb-shadow-md);
    margin: var(--nb-space-2) 0;
  }

  .sender-qr {
    width: 200px;
    height: 200px;
    display: block;
    background: #ffffff;
  }

  .url-action-bar {
    width: 100%;
    max-width: 400px;
    display: flex;
    flex-direction: column;
    gap: var(--nb-space-2);
    margin-bottom: var(--nb-space-3);
  }

  .url-label {
    font-family: var(--nb-font-mono);
    font-size: var(--nb-text-sm);
    font-weight: var(--nb-fw-bold);
  }

  .url-box {
    display: flex;
    width: 100%;
    border: var(--nb-border-md);
    background: var(--nb-bg);
  }

  .url-input {
    flex: 1;
    background: transparent;
    border: none;
    padding: var(--nb-space-3);
    outline: none;
    min-width: 0;
    color: var(--nb-text);
  }

  .close-btn {
    width: 100%;
    max-width: 400px;
    margin-top: var(--nb-space-2);
  }

  /* About Layout */
  .about-layout {
    display: flex;
    flex-direction: column;
    gap: var(--nb-space-6);
    margin-top: var(--nb-space-5);
    max-width: 600px;
    width: 100%;
    margin-left: auto;
    margin-right: auto;
  }

  .about-card {
    background: var(--nb-surface);
    border: var(--nb-border-lg);
    box-shadow: 6px 6px 0px var(--nb-shadow-color);
    padding: var(--nb-space-6);
    display: flex;
    flex-direction: column;
    gap: var(--nb-space-4);
  }

  .about-header {
    display: flex;
    align-items: center;
    gap: var(--nb-space-4);
    border-bottom: var(--nb-border-md);
    padding-bottom: var(--nb-space-4);
  }

  .logo-box {
    background: #000000;
    border: var(--nb-border-lg);
    box-shadow: 4px 4px 0px var(--nb-shadow-color);
    padding: var(--nb-space-1);
    display: flex;
    align-items: center;
    justify-content: center;
  }

  .about-logo {
    width: 64px;
    height: 64px;
    display: block;
  }

  .about-title {
    display: flex;
    flex-direction: row;
    gap: var(--nb-space-3);
    align-items: center;
  }

  .about-title h1 {
    margin: 0;
    font-family: var(--nb-font-display);
    font-weight: 800;
    font-size: 2rem;
    line-height: 1;
  }

  .version-badge {
    background: var(--nb-primary);
    color: var(--nb-primary-text);
    padding: 4px 8px;
    font-family: var(--nb-font-mono);
    font-size: var(--nb-text-sm);
    font-weight: 800;
    border: var(--nb-border-md);
  }

  .about-desc {
    font-family: var(--nb-font-mono);
    font-size: var(--nb-text-sm);
    line-height: 1.6;
    margin: 0;
    color: var(--nb-text-muted);
  }

  .about-tags {
    display: flex;
    gap: var(--nb-space-2);
    margin-top: var(--nb-space-3);
  }

  .developer-card {
    background: var(--nb-bg);
    border: var(--nb-border-lg);
    box-shadow: 4px 4px 0px var(--nb-shadow-color);
    display: flex;
    flex-direction: column;
  }

  .dev-header {
    background: var(--nb-primary);
    border-bottom: var(--nb-border-lg);
    padding: var(--nb-space-2) var(--nb-space-4);
  }

  .dev-header h3 {
    margin: 0;
    font-family: var(--nb-font-display);
    font-weight: 800;
    font-size: var(--nb-text-sm);
    letter-spacing: 0.05em;
    color: var(--nb-primary-text);
  }

  .dev-body {
    padding: var(--nb-space-4);
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--nb-space-4);
  }

  .dev-name {
    font-weight: var(--nb-fw-bold);
    font-size: var(--nb-text-lg);
    font-family: var(--nb-font-display);
  }

  .about-links {
    display: flex;
    gap: var(--nb-space-3);
  }
</style>
