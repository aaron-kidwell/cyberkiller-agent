package main

import (
	_ "embed"
	"flag"
	"fmt"
	"image/color"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// ghostPNG is the transparent ghost mascot, baked into the binary so the photo
// renders for anyone who runs the agent — no external file needed.
//
//go:embed ghost.png
var ghostPNG []byte

// termColor is the detected terminal color depth, set once at TUI startup.
var termColor colorMode

// lastW/lastH track the previous render dimensions to detect terminal resizes.
var lastW, lastH int

// termBG is the dark backdrop transparent pixels composite onto (matches the
// web theme #08060d) so the ghost's glow fades into the terminal.
var termBG = color.RGBA{R: 8, G: 6, B: 13, A: 255}

// detectColorMode picks the best color depth with NO flags required. Modern
// terminals (Kali qterminal, Parrot mate-terminal, gnome-terminal, kitty,
// alacritty, xterm, …) all render 24-bit truecolor escapes, and the rare one
// that can't will approximate them to its nearest palette automatically. So we
// default to truecolor for everything that has a real terminal, and only fall
// back to mono for a genuinely color-less TERM ("dumb" or unset). 256-color is
// only ever selected explicitly via --color 256.
func detectColorMode() colorMode {
	term := os.Getenv("TERM")
	if term == "" || term == "dumb" {
		// No interactive terminal info — but if stdout is a tty we still try
		// truecolor (most pipelines that reach here are real terminals).
		if fi, err := os.Stdout.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) != 0 {
			return colorTrue
		}
		return colorNone
	}
	return colorTrue
}

// ── ANSI helpers (pure stdlib — no TUI framework, keeps the agent auditable) ──

const (
	esc       = "\033["
	altOn     = "\033[?1049h" // enter alternate screen buffer
	altOff    = "\033[?1049l" // leave it (restores prior terminal contents)
	hideCur   = "\033[?25l"
	showCur   = "\033[?25h"
	clearAll  = "\033[2J"
	homeCur   = "\033[H"
	resetSGR  = "\033[0m"
	boldSGR   = "\033[1m"
	dimSGR    = "\033[2m"
)

// Truecolor foreground. Falls through fine on 24-bit terminals (the norm now).
func fg(r, g, b int) string { return fmt.Sprintf("\033[38;2;%d;%d;%dm", r, g, b) }

// Brand palette (matches the web theme).
var (
	cMag   = fg(232, 52, 198)  // #e834c6
	cPink  = fg(255, 106, 213) // #ff6ad5
	cCyan  = fg(34, 211, 238)  // #22d3ee
	cPurp  = fg(139, 92, 246)  // #8b5cf6
	cGreen = fg(0, 255, 136)   // #00ff88
	cDim   = fg(120, 116, 140)
	cTxt   = fg(220, 216, 240)
	cRed   = fg(244, 63, 94)
)

func moveTo(row, col int) string { return fmt.Sprintf("%s%d;%dH", esc, row, col) }

type winsize struct{ Row, Col, X, Y uint16 }

// termSize returns the real terminal dimensions by querying the stdout fd with
// ioctl(TIOCGWINSZ) — the most reliable method. Falls back to `stty size` and
// then 80x24.
func termSize() (rows, cols int) {
	ws := &winsize{}
	r, _, _ := syscall.Syscall(syscall.SYS_IOCTL, os.Stdout.Fd(),
		uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(ws)))
	if r == 0 && ws.Col > 0 && ws.Row > 0 {
		return int(ws.Row), int(ws.Col)
	}
	// Fallback: stty.
	rows, cols = 24, 80
	if out, err := exec.Command("stty", "-F", "/dev/tty", "size").Output(); err == nil {
		parts := strings.Fields(strings.TrimSpace(string(out)))
		if len(parts) == 2 {
			if v, e := strconv.Atoi(parts[0]); e == nil && v > 0 {
				rows = v
			}
			if v, e := strconv.Atoi(parts[1]); e == nil && v > 0 {
				cols = v
			}
		}
	}
	return
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ── Ghost ANSI art — compact neon mascot rendered with block glyphs ──────────
// Drawn in pink/magenta with cyan lightning to echo the web logo.
func ghostArt() []string {
	p := cPink
	m := cMag
	c := cCyan
	z := resetSGR
	return []string{
		"        " + p + "▄████▄" + z,
		"      " + p + "▟██████▙" + z,
		"     " + m + "███" + cTxt + "●" + m + "██" + cTxt + "●" + m + "██" + z,
		"     " + m + "██▁▁▁▁▁██" + z,
		"     " + p + "██████████" + "   " + c + "⚡" + z,
		"     " + p + "█████████" + "  " + c + "⚡⚡" + z,
		"     " + m + "███████" + "   " + c + "⚡" + z,
		"     " + m + "██▀██▀██" + z,
		"      " + p + "▀▘ ▝▀" + z,
	}
}

// ── Bandwidth sampling ────────────────────────────────────────────────────────

type bwSample struct {
	rx, tx   uint64 // bytes
	rxp, txp uint64 // packets
	t        time.Time
}

// readWGCounters reads cumulative rx/tx bytes for the wg0 interface from sysfs.
// Returns ok=false if the interface isn't present (e.g. not connected yet).
func readU64(path string) (uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return v, err == nil
}

// readWGCounters reads cumulative rx/tx BYTES and PACKETS for the interface
// from sysfs. ok=false if the interface isn't up yet.
func readWGCounters(iface string) (rxB, txB, rxP, txP uint64, ok bool) {
	base := "/sys/class/net/" + iface + "/statistics/"
	rxB, ok1 := readU64(base + "rx_bytes")
	txB, ok2 := readU64(base + "tx_bytes")
	rxP, _ = readU64(base + "rx_packets")
	txP, _ = readU64(base + "tx_packets")
	return rxB, txB, rxP, txP, ok1 && ok2
}

// findCKInterface returns the WireGuard interface to monitor: the preferred
// name if it's up, else the first wg* interface present (the arena tunnel).
func findCKInterface(pref string) string {
	if _, err := os.Stat("/sys/class/net/" + pref); err == nil {
		return pref
	}
	entries, err := os.ReadDir("/sys/class/net")
	if err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "wg") || strings.HasPrefix(e.Name(), "ck") {
				return e.Name()
			}
		}
	}
	return pref
}

func humanCount(n uint64) string {
	f := float64(n)
	if f < 1000 {
		return fmt.Sprintf("%d", n)
	}
	units := []string{"", "K", "M", "G"}
	i := 0
	for f >= 1000 && i < len(units)-1 {
		f /= 1000
		i++
	}
	return fmt.Sprintf("%.1f%s", f, units[i])
}

func humanBytes(b uint64) string {
	f := float64(b)
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d %s", b, units[i])
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

func humanRate(bps float64) string {
	return humanBytes(uint64(bps)) + "/s"
}

// sparkline renders a ring buffer of rates as unicode block bars.
func sparkline(hist []float64, color string) string {
	blocks := []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}
	var max float64 = 1
	for _, v := range hist {
		if v > max {
			max = v
		}
	}
	var b strings.Builder
	b.WriteString(color)
	for _, v := range hist {
		idx := int((v / max) * float64(len(blocks)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(blocks) {
			idx = len(blocks) - 1
		}
		b.WriteRune(blocks[idx])
	}
	b.WriteString(resetSGR)
	return b.String()
}

// bar renders a fixed-width meter for a rate relative to a rolling peak.
func bar(rate, peak float64, width int, color string) string {
	if peak < 1 {
		peak = 1
	}
	filled := int((rate / peak) * float64(width))
	if filled > width {
		filled = width
	}
	if filled < 0 {
		filled = 0
	}
	return color + strings.Repeat("█", filled) + cDim + strings.Repeat("░", width-filled) + resetSGR
}

// ── Terminal raw mode (so single keypresses register without Enter) ──────────

func setRaw() func() {
	// Use stty (portable, no cgo). cbreak + no echo lets us read 'q' instantly.
	exec.Command("stty", "-F", "/dev/tty", "cbreak", "-echo").Run()
	return func() { exec.Command("stty", "-F", "/dev/tty", "sane").Run() }
}

// ── TUI ───────────────────────────────────────────────────────────────────────

func runTUI(args []string) {
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	demo := fs.Bool("demo", false, "local test: simulate bandwidth, no WireGuard/root needed")
	iface := fs.String("iface", "wg0", "network interface to monitor")
	noPhoto := fs.Bool("ascii", false, "force ASCII block art instead of the photo")
	colorFlag := fs.String("color", "auto", "color mode: auto|true|256|off (force for consistent look across terminals)")
	fs.Parse(args)

	switch *colorFlag {
	case "true", "truecolor", "24bit":
		termColor = colorTrue
	case "256":
		termColor = color256
	case "off", "none", "mono":
		termColor = colorNone
	default: // "auto"
		termColor = detectColorMode()
	}
	if *noPhoto {
		termColor = colorNone // forces the ANSI-art fallback
	}

	// Load state for handle/arena IP if present (best-effort).
	handle, arenaIP := "local-kali", "10.66.10.5"
	if st, err := loadState(); err == nil {
		if st.Handle != "" {
			handle = st.Handle
		}
		if st.ArenaIP != "" {
			arenaIP = st.ArenaIP
		}
	}
	// `tui` subcommand: show the fake boot only in --demo; never run heartbeats.
	tuiDashboard(handle, arenaIP, *iface, *demo, *demo, nil)
}

// tuiDashboard runs the live dashboard on the alternate screen until the user
// quits (q / Ctrl+C). demo simulates traffic; otherwise it reads the real wg
// interface. showBoot replays the connect animation first. onQuit (optional)
// runs after the screen is restored — used by the connect flow to tear down
// the tunnel. Heartbeats, if any, are driven by the caller's goroutine.
func tuiDashboard(handle, arenaIP, iface string, demo, showBoot bool, onQuit func()) {
	restore := setRaw()
	fmt.Print(altOn + hideCur + clearAll)
	cleanup := func() {
		fmt.Print(showCur + altOff + resetSGR)
		restore()
	}
	defer cleanup()
	defer func() {
		if onQuit != nil {
			onQuit()
		}
	}()

	if showBoot {
		bootSequence(handle, arenaIP)
	}

	// Quit on Ctrl+C as well as 'q'.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	keys := make(chan byte, 8)
	go func() {
		buf := make([]byte, 1)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil || n == 0 {
				return
			}
			keys <- buf[0]
		}
	}()

	start := time.Now()
	const histLen = 256 // enough samples to fill a wide btop-style graph
	rxHist := make([]float64, histLen)
	txHist := make([]float64, histLen)
	var rxPeak, txPeak float64

	var prev bwSample
	var totalRx, totalTx, totalRxP, totalTxP uint64
	var demoRx, demoTx, demoRxP, demoTxP uint64
	var demoDL, demoUL float64
	connected := false
	ckIface := findCKInterface(iface) // resolve the actual arena wg interface

	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()

	frame := 0
	for {
		select {
		case <-sig:
			return
		case k := <-keys:
			if k == 'q' || k == 'Q' || k == 3 /*Ctrl+C*/ {
				return
			}
		case now := <-tick.C:
			frame++
			var rx, tx, rxp, txp uint64
			if demo {
				// Organic bursty traffic: a wandering baseline plus occasional
				// spikes, so the graph has btop-like peaks and valleys.
				connected = true
				// Realistic arena traffic: mostly idle, with occasional bursts
				// (a scan, a file pull). Baseline decays toward ~idle so totals
				// accumulate slowly like a real session, not a constant stream.
				demoDL = demoDL*0.88 + 0.01      // decay toward a low idle floor
				if rndF() < 0.04 {               // ~1 burst every 6s
					demoDL = 0.4 + rndF()*0.6    // download burst
				}
				demoUL = demoUL*0.90 + 0.005
				if rndF() < 0.03 {
					demoUL = 0.2 + rndF()*0.4
				}
				// demoDL/UL are RATES (fraction of cap). Bytes added this tick =
				// rate * cap * tickInterval, so the displayed MB/s matches the cap.
				const tickSec = 0.25
				inBurst := uint64(demoDL * 1_400_000 * tickSec) // cap ~1.4 MB/s
				outBurst := uint64(demoUL * 400_000 * tickSec)  // cap ~400 KB/s
				demoRx += inBurst
				demoTx += outBurst
				demoRxP += inBurst/1400 + 1 // ~MTU-sized packets
				demoTxP += outBurst/1400 + 1
				rx, tx, rxp, txp = demoRx, demoTx, demoRxP, demoTxP
			} else {
				var ok bool
				rx, tx, rxp, txp, ok = readWGCounters(ckIface)
				connected = ok
			}
			totalRx, totalTx, totalRxP, totalTxP = rx, tx, rxp, txp

			var rxRate, txRate, rxPps, txPps float64
			if !prev.t.IsZero() {
				dt := now.Sub(prev.t).Seconds()
				if dt > 0 {
					rxRate = float64(rx-prev.rx) / dt
					txRate = float64(tx-prev.tx) / dt
					rxPps = float64(rxp-prev.rxp) / dt
					txPps = float64(txp-prev.txp) / dt
				}
			}
			prev = bwSample{rx: rx, tx: tx, rxp: rxp, txp: txp, t: now}

			rxHist = append(rxHist[1:], rxRate)
			txHist = append(txHist[1:], txRate)
			// True session peaks (no decay) so the "peak" stat is accurate. The
			// graph scales to its own visible-window max, so peak is purely a
			// reported statistic here.
			if rxRate > rxPeak {
				rxPeak = rxRate
			}
			if txRate > txPeak {
				txPeak = txRate
			}

			render(frame, start, handle, arenaIP, ckIface, connected, demo,
				totalRx, totalTx, totalRxP, totalTxP, rxRate, txRate, rxPps, txPps,
				rxPeak, txPeak, rxHist, txHist)
		}
	}
}

// bootSequence prints the same connect output as the real agent (verbatim
// strings from runFirstConnect/applyUpdate) with realistic pauses, so a demo
// recording is indistinguishable from a live connect. Renders on the alt
// screen, then clears for the dashboard.
func bootSequence(handle, arenaIP string) {
	type step struct {
		pre   string
		ok    string
		delay time.Duration
	}
	pause := func(d time.Duration) { time.Sleep(d) }
	fmt.Print(homeCur + clearAll)

	// Banner (matches printHeader()).
	fmt.Println()
	fmt.Println("  " + cMag + "┌─────────────────────────────────────────┐" + resetSGR)
	fmt.Println("  " + cMag + "│" + cPink + boldSGR + "        CYBERKILLER ARENA AGENT          " + resetSGR + cMag + "│" + resetSGR)
	fmt.Printf("  "+cMag+"│"+resetSGR+cDim+"              v%-26s"+resetSGR+cMag+"│"+resetSGR+"\n", "4.7.7")
	fmt.Println("  " + cMag + "└─────────────────────────────────────────┘" + resetSGR)
	fmt.Println()
	pause(400 * time.Millisecond)

	fmt.Print("  " + cDim + "[▸] Checking for updates...          " + resetSGR)
	pause(700 * time.Millisecond)
	fmt.Println(cGreen + "v4.7.7 (up to date)" + resetSGR)
	pause(250 * time.Millisecond)

	fmt.Print("  Verifying token...   ")
	pause(600 * time.Millisecond)
	fmt.Println(cGreen + "OK" + resetSGR)
	fmt.Printf("  "+cDim+"Operative handle:    "+resetSGR+cTxt+"%s"+resetSGR+"\n\n", handle)
	pause(400 * time.Millisecond)

	steps := []step{
		{"  [▸] Registering with arena...        ", cGreen + "✓" + resetSGR, 650 * time.Millisecond},
		{"  [▸] Configuring tunnel...            ", cGreen + "✓" + resetSGR, 800 * time.Millisecond},
		{"  [▸] Bringing up WireGuard...         ", cGreen + "✓" + resetSGR, 700 * time.Millisecond},
		{"  [▸] Installing egress kill-switch... ", cGreen + "✓" + resetSGR, 500 * time.Millisecond},
		{"  [▸] Verifying tunnel...              ", cGreen + "✓" + resetSGR, 900 * time.Millisecond},
	}
	for _, s := range steps {
		fmt.Print(s.pre)
		pause(s.delay)
		fmt.Println(s.ok)
	}
	fmt.Printf("\n  "+cDim+"Arena IP:  "+resetSGR+cCyan+"%s"+resetSGR+"\n", arenaIP)
	fmt.Println("  " + cDim + "Tunnel:    " + resetSGR + cGreen + "UP" + resetSGR)
	pause(700 * time.Millisecond)
	fmt.Println("\n  " + cGreen + "● Connected to the arena. Launching dashboard..." + resetSGR)
	pause(900 * time.Millisecond)
	fmt.Print(clearAll + homeCur)
}

// shutdownSequence performs the real teardown (wg down, kill-switch removal,
// disconnect heartbeat) while showing the same styled, paced animation as the
// boot sequence — so connect and disconnect feel consistent.
func shutdownSequence(st agentState) {
	pause := func(d time.Duration) { time.Sleep(d) }
	fmt.Println()
	fmt.Println("  " + cMag + "┌─────────────────────────────────────────┐" + resetSGR)
	fmt.Println("  " + cMag + "│" + cPink + boldSGR + "        DISCONNECTING FROM ARENA          " + resetSGR + cMag + "│" + resetSGR)
	fmt.Println("  " + cMag + "└─────────────────────────────────────────┘" + resetSGR)
	fmt.Println()
	pause(350 * time.Millisecond)

	fmt.Print("  [▸] Tearing down WireGuard tunnel... ")
	exec.Command("wg-quick", "down", "wg0").Run()
	pause(500 * time.Millisecond)
	fmt.Println(cGreen + "✓" + resetSGR)

	fmt.Print("  [▸] Removing egress kill-switch...   ")
	removeKillSwitch()
	pause(400 * time.Millisecond)
	fmt.Println(cGreen + "✓" + resetSGR)

	fmt.Print("  [▸] Signing off the arena...         ")
	sendHeartbeat(st, false)
	pause(450 * time.Millisecond)
	fmt.Println(cGreen + "✓" + resetSGR)

	st.BgPID = 0
	saveState(st)
	pause(300 * time.Millisecond)
	fmt.Printf("\n  "+cDim+"Disconnected"+resetSGR+", %s. See you back in the arena.\n\n", st.Handle)
}

// rndF returns a pseudo-random float in [0,1) from a cheap xorshift PRNG —
// avoids importing math/rand for demo traffic generation.
var rndState uint64 = 0x9e3779b97f4a7c15

func rndF() float64 {
	rndState ^= rndState << 13
	rndState ^= rndState >> 7
	rndState ^= rndState << 17
	return float64(rndState>>11) / float64(1<<53)
}

func render(frame int, start time.Time, handle, arenaIP, iface string, connected, demo bool,
	totalRx, totalTx, totalRxP, totalTxP uint64, rxRate, txRate, rxPps, txPps,
	rxPeak, txPeak float64, rxHist, txHist []float64) {

	// Fill the actual terminal (clamped to sane bounds). Recomputed each frame
	// so the layout adapts if the window is resized.
	tr, tc := termSize()
	W := clampInt(tc, 56, 120)
	H := clampInt(tr, 20, 50)
	innerW := W - 2

	var b strings.Builder
	// On a size change, wipe the whole screen so a resize leaves no stale cells
	// (shorter lines / old borders) from the previous dimensions.
	if W != lastW || H != lastH {
		b.WriteString(clearAll)
		lastW, lastH = W, H
	}
	b.WriteString(homeCur)

	row := func(content string) {
		b.WriteString(cMag + "║" + resetSGR + content + padTo(content, innerW) + cMag + "║" + resetSGR + "\n")
	}
	top := cMag + "╔" + strings.Repeat("═", innerW) + "╗" + resetSGR
	bot := cMag + "╚" + strings.Repeat("═", innerW) + "╝" + resetSGR
	sep := cMag + "╟" + strings.Repeat("─", innerW) + "╢" + resetSGR

	// Vertical budget. Fixed chrome: top(1)+sep(1)+inLabel(1)+outLabel(1)+
	// sep(1)+footer(1)+bottom(1) = 7. Header (ghost) gets the larger share.
	// IMPORTANT: headerH + 2*graphRows must EXACTLY equal avail, or the frame
	// over/underflows H and the terminal scrolls (cutting off the top border).
	avail := H - 7
	graphRows := clampInt(avail/5, 2, 6) // each of the two graphs
	headerH := avail - 2*graphRows
	if headerH < 6 { // tiny terminal: shrink graphs to keep some header
		graphRows = clampInt((avail-6)/2, 1, graphRows)
		headerH = avail - 2*graphRows
	}
	if headerH < 1 {
		headerH = 1
	}

	// Ghost sized to fill the header height. Source aspect ~0.87 (w/h), so to
	// fill `headerH` character rows we need cols ≈ headerH * 2 * 0.87 ≈ 1.74×.
	// Capped to ~58% of the inner width so the title column still fits.
	ghostCols := clampInt(int(float64(headerH)*1.74), 18, innerW*52/100)
	// Color terminals get high-quality half-block rendering (2 true-color
	// pixels per cell); no-color falls back to ASCII density; decode failure
	// to the tiny ANSI block art.
	var ghost []string
	if termColor == colorNone {
		ghost = renderImageASCII(ghostPNG, ghostCols, termBG, termColor)
	} else {
		ghost = renderImageCells(ghostPNG, ghostCols, termBG, termColor)
	}
	if ghost == nil {
		ghost = ghostArt()
	}

	conn := cRed + "○ OFFLINE" + resetSGR
	if connected {
		conn = cGreen + "● CONNECTED" + resetSGR
	}
	if demo {
		conn += "   " + cDim + "[DEMO]" + resetSGR
	}
	up := time.Since(start).Round(time.Second)
	title := []string{
		cPink + boldSGR + "C Y B E R   K I L L E R" + resetSGR,
		cDim + "competitive hacking arena" + resetSGR,
		"",
		conn,
		"",
		cDim + "HANDLE   " + resetSGR + cTxt + handle + resetSGR,
		cDim + "ARENA IP " + resetSGR + cCyan + arenaIP + resetSGR,
		cDim + "UPTIME   " + resetSGR + cTxt + fmtDur(up) + resetSGR,
	}

	b.WriteString(top + "\n")

	// Header: ghost (left) + title block, both vertically centered in headerH.
	gPad := (headerH - len(ghost)) / 2
	tPad := (headerH - len(title)) / 2
	const titleGap = "        " // 8 spaces so the text isn't cramped against the ghost
	for i := 0; i < headerH; i++ {
		left := strings.Repeat(" ", ghostCols)
		if gi := i - gPad; gi >= 0 && gi < len(ghost) {
			left = ghost[gi]
		}
		right := ""
		if ti := i - tPad; ti >= 0 && ti < len(title) {
			right = title[ti]
		}
		content := "  " + left + titleGap + right
		row(content)
	}

	b.WriteString(sep + "\n")

	// Bandwidth — btop-style braille area graphs that fill the width and scale
	// with height. IN graph is a cyan gradient, OUT is magenta.
	graphCols := innerW - 4
	cyanLow := color.RGBA{8, 90, 110, 255}
	cyanHigh := color.RGBA{34, 211, 238, 255}
	magLow := color.RGBA{90, 20, 80, 255}
	magHigh := color.RGBA{232, 52, 198, 255}

	// DOWNLOAD label — real wg counters: rate, packets/sec, total bytes/packets, peak
	row("  " + cCyan + boldSGR + "▼ DOWNLOAD" + resetSGR +
		cDim + " (" + iface + ")  " + resetSGR + cCyan + humanRate(rxRate) + resetSGR +
		cDim + "  " + resetSGR + cTxt + humanCount(uint64(rxPps)) + " pps" + resetSGR +
		cDim + "   total " + resetSGR + cTxt + humanBytes(totalRx) + resetSGR +
		cDim + " / " + humanCount(totalRxP) + " pkts" + resetSGR +
		cDim + "   peak " + resetSGR + cTxt + humanRate(rxPeak) + resetSGR)
	for _, gl := range brailleGraph(rxHist, graphCols, graphRows, rxPeak, cyanLow, cyanHigh, termColor) {
		row("  " + gl)
	}
	// UPLOAD label
	row("  " + cMag + boldSGR + "▲ UPLOAD" + resetSGR +
		cDim + "   (" + iface + ")  " + resetSGR + cMag + humanRate(txRate) + resetSGR +
		cDim + "  " + resetSGR + cTxt + humanCount(uint64(txPps)) + " pps" + resetSGR +
		cDim + "   total " + resetSGR + cTxt + humanBytes(totalTx) + resetSGR +
		cDim + " / " + humanCount(totalTxP) + " pkts" + resetSGR +
		cDim + "   peak " + resetSGR + cTxt + humanRate(txPeak) + resetSGR)
	for _, gl := range brailleGraph(txHist, graphCols, graphRows, txPeak, magLow, magHigh, termColor) {
		row("  " + gl)
	}

	b.WriteString(sep + "\n")
	row("  " + cDim + "q quit · range machines at 10.66.20.x · cyberkiller.net/hub" + resetSGR)
	// Bottom border: NO trailing newline. A newline on the final line of a
	// full-height frame makes the terminal scroll up one row, cutting off the
	// top border. Clear to end-of-screen first to wipe any leftover rows.
	b.WriteString("\033[0J" + bot)
	fmt.Print(b.String())
}

// padTo returns spaces to pad a styled string to `width` visible columns.
func padTo(styled string, width int) string {
	v := visibleLen(styled)
	if v >= width {
		return ""
	}
	return strings.Repeat(" ", width-v)
}

// visibleLen counts printable columns, skipping ANSI escape sequences and
// accounting for wide/zero-width glyphs approximately.
func visibleLen(s string) int {
	n := 0
	inEsc := false
	for _, r := range s {
		if inEsc {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
			continue
		}
		if r == '\033' {
			inEsc = true
			continue
		}
		switch {
		case r >= 0x1F300 && r <= 0x1FAFF: // emoji ~ 2 cols
			n += 2
		case r == '⚡' || r == '●' || r == '○':
			n++
		case r >= 0x2500 && r <= 0x259F: // box/block drawing = 1 col
			n++
		default:
			n++
		}
	}
	return n
}

func fmtDur(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}
