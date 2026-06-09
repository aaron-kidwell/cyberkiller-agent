package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const agentVersion = "4.7.7"
const stateFile = "/var/run/cyberkiller-agent.json"

// defaultAPI is the URL the agent uses to reach the CyberKiller control plane
// for register/heartbeat/flag-submit/etc. It is intentionally empty in source —
// production releases set it at build time via:
//
//	go build -ldflags "-X main.defaultAPI=https://your-range.example/api"
//
// If left blank and no --api flag is passed, the agent will refuse to start
// and print a usage hint instead of guessing a URL.
var defaultAPI = ""

var httpClient = &http.Client{Timeout: 8 * time.Second}

type agentState struct {
	PlayerID    string `json:"player_id"`
	ArenaIP     string `json:"arena_ip"`
	Handle      string `json:"handle"`
	APIBase     string `json:"api_base"`
	InviteToken string `json:"invite_token"`
	BgPID       int    `json:"bg_pid,omitempty"` // non-zero = background agent process
}

func main() {
	sub := ""
	if len(os.Args) > 1 {
		sub = os.Args[1]
	}
	switch sub {
	case "connect":
		runReconnect()
	case "disconnect":
		runDisconnect()
	case "status":
		runStatus()
	case "submit":
		runSubmit(os.Args[2:])
	default:
		runFirstConnect()
	}
}

// ─── First-time connection ────────────────────────────────────────────────────

func runFirstConnect() {
	apiEnv := os.Getenv("CK_API")
	if apiEnv == "" {
		apiEnv = defaultAPI
	}
	api := flag.String("api", apiEnv, "arena API base URL")
	endpointOverride := flag.String("endpoint", "", "override WireGuard endpoint (host:port)")
	detach := flag.Bool("detach", false, "run heartbeat loop in background")
	flag.Parse()

	token := flag.Arg(0)

	printHeader()

	if *api == "" {
		fmt.Fprintln(os.Stderr, "  Error: no arena API URL configured.")
		fmt.Fprintln(os.Stderr, "  Pass --api https://your-range.example/api  OR set CK_API in your environment.")
		fmt.Fprintln(os.Stderr, "  (Official releases ship with the URL baked in at build time.)")
		os.Exit(1)
	}

	checkForUpdate(*api)

	if token == "" {
		fmt.Fprintln(os.Stderr, "  Error: provide your invite token")
		fmt.Fprintln(os.Stderr, "  Usage: sudo ./cyberkiller-agent INVITE_TOKEN [--detach]")
		os.Exit(1)
	}
	requireRoot()
	ensureDependencies()

	fmt.Print("  Verifying token...   ")
	handle, workingAPI, err := fetchHandle(*api, token)
	if err != nil {
		fmt.Println("FAIL")
		fmt.Fprintf(os.Stderr, "\n  Could not verify token: %v\n", err)
		os.Exit(1)
	}
	*api = workingAPI // use whichever URL variant succeeded
	fmt.Println("OK")
	fmt.Printf("  Operative handle:    %s\n\n", handle)

	if !showRules(handle) {
		fmt.Println("\n  Connection aborted.")
		os.Exit(1)
	}

	fmt.Print("\n  [▸] Generating keys...               ")
	priv, pub, err := wgKeypair()
	if err != nil {
		fmt.Println("FAIL")
		fmt.Fprintf(os.Stderr, "  wg genkey: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("✓")

	fmt.Print("  [▸] Registering with arena...        ")
	reg, workingReg, err := register(*api, token, handle, pub)
	if err != nil {
		fmt.Println("FAIL")
		fmt.Fprintf(os.Stderr, "  register: %v\n", err)
		os.Exit(1)
	}
	if workingReg != "" {
		*api = workingReg
	}
	fmt.Println("✓")

	fmt.Print("  [▸] Configuring tunnel...            ")
	endpoint := reg.HubEndpoint
	if *endpointOverride != "" {
		endpoint = *endpointOverride
	}
	if err := setupWG(priv, reg.ArenaIP, reg.HubPubkey, endpoint, reg.ExtraRoutes); err != nil {
		fmt.Println("FAIL")
		fmt.Fprintf(os.Stderr, "  wireguard: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("✓")

	applyKillSwitch()

	fmt.Print("  [▸] Verifying tunnel...              ")
	if verifyTunnel(3 * time.Second) {
		fmt.Println("✓")
	} else {
		fmt.Println("⚠  (handshake pending — give it a few seconds)")
	}

	st := agentState{
		PlayerID:    reg.PlayerID,
		ArenaIP:     reg.ArenaIP,
		Handle:      handle,
		APIBase:     *api,
		InviteToken: token,
	}
	saveState(st)
	sendHeartbeat(st, true)

	hills := fetchHills(*api)
	pts := fetchArenaStats(*api)
	printWelcome(handle, reg.ArenaIP, hills, pts)

	runLoop(st, *detach)
}

// ─── Reconnect ────────────────────────────────────────────────────────────────

func runReconnect() {
	st, err := loadState()
	if err != nil {
		fmt.Fprintln(os.Stderr, "  No saved session — register first: sudo ./cyberkiller-agent INVITE_TOKEN")
		os.Exit(1)
	}
	requireRoot()
	ensureDependencies()

	api := st.APIBase
	if api == "" {
		api = defaultAPI
	}
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	apiFlag := fs.String("api", api, "arena API base URL")
	detach := fs.Bool("detach", false, "run heartbeat loop in background")
	fs.Parse(os.Args[2:])
	st.APIBase = *apiFlag

	printHeader()
	checkForUpdate(st.APIBase)
	fmt.Printf("  Resuming session for: %s\n", st.Handle)
	fmt.Printf("  Arena IP:             %s\n\n", st.ArenaIP)

	fmt.Print("  [▸] Bringing tunnel up...            ")
	exec.Command("wg-quick", "down", "wg0").Run()
	if out, err2 := exec.Command("wg-quick", "up", "wg0").CombinedOutput(); err2 != nil {
		fmt.Println("FAIL")
		fmt.Fprintf(os.Stderr, "  %s\n", strings.TrimSpace(string(out)))
		fmt.Fprintln(os.Stderr, "  Tip: re-register if WG config is gone: sudo ./cyberkiller-agent INVITE_TOKEN")
		os.Exit(1)
	}
	fmt.Println("✓")

	applyKillSwitch()

	fmt.Print("  [▸] Verifying tunnel...              ")
	if verifyTunnel(5 * time.Second) {
		fmt.Println("✓")
	} else {
		fmt.Println("⚠  (handshake pending — give it a few seconds)")
	}

	saveState(*st)
	sendHeartbeat(*st, true)

	hills := fetchHills(st.APIBase)
	pts := fetchArenaStats(st.APIBase)
	printWelcome(st.Handle, st.ArenaIP, hills, pts)

	runLoop(*st, *detach)
}

// ─── Main loop ────────────────────────────────────────────────────────────────

func runLoop(st agentState, detach bool) {
	if detach {
		spawnBackground(st)
		return
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	tick := time.NewTicker(10 * time.Second)
	updateTick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()
	defer updateTick.Stop()

	for {
		select {
		case <-sig:
			teardown(st)
			return
		case <-tick.C:
			sendHeartbeat(st, true)
		case <-updateTick.C:
			if info, available, _ := fetchUpdateInfo(st.APIBase); available {
				fmt.Printf("\n  [!] Update available (v%s) — auto-updating...\n", info.Version)
				teardown(st)
				applyUpdate(st.APIBase, info, true) // reconnect=true: exec new binary as "connect"
			}
		}
	}
}

// spawnBackground re-execs this binary in a new session without --detach,
// saves the child PID to the state file, then returns the terminal.
func spawnBackground(st agentState) {
	exe, _ := os.Executable()
	logPath := "/var/log/cyberkiller-agent.log"
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		logFile, _ = os.CreateTemp("", "ck-agent-*.log")
		logPath = logFile.Name()
	}

	// Re-exec as "connect" (reconnect) — wg0 is already up, state is saved
	cmd := exec.Command(exe, "connect")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "  Failed to detach: %v\n", err)
		os.Exit(1)
	}

	st.BgPID = cmd.Process.Pid
	saveState(st)

	fmt.Printf("  Agent running in background (PID %d)\n\n", st.BgPID)
	printNextSteps(true, logPath)
}

// teardown brings wg0 down, removes the kill switch, sends a disconnect
// heartbeat, and clears the background PID from the state file.
func teardown(st agentState) {
	fmt.Printf("\n  [▸] Disconnecting %s...\n", st.Handle)
	exec.Command("wg-quick", "down", "wg0").Run()
	removeKillSwitch()
	sendHeartbeat(st, false)
	st.BgPID = 0
	saveState(st)
	fmt.Println("  Tunnel down. See you next time.")
}

// ─── Disconnect subcommand ────────────────────────────────────────────────────

func runDisconnect() {
	printHeader()

	st, err := loadState()
	if err != nil {
		// No saved session — still clean up wg0 in case it's lingering
		fmt.Println("  No active session found. Cleaning up tunnel...")
		exec.Command("wg-quick", "down", "wg0").Run()
		removeKillSwitch()
		fmt.Println("  Done.")
		return
	}

	// Kill background process if one is running
	if st.BgPID > 0 {
		if proc, perr := os.FindProcess(st.BgPID); perr == nil {
			if proc.Signal(syscall.Signal(0)) == nil {
				fmt.Printf("  Stopping background agent (PID %d)...\n", st.BgPID)
				proc.Signal(syscall.SIGTERM)
				time.Sleep(500 * time.Millisecond)
			}
		}
		st.BgPID = 0
	}

	exec.Command("wg-quick", "down", "wg0").Run()
	removeKillSwitch()
	sendHeartbeat(*st, false)
	os.Remove(stateFile)
	fmt.Println("  Disconnected.")
}

// ─── WireGuard ───────────────────────────────────────────────────────────────

func wgKeypair() (priv, pub string, err error) {
	out, err := exec.Command("wg", "genkey").Output()
	if err != nil {
		return "", "", err
	}
	priv = strings.TrimSpace(string(out))
	pubCmd := exec.Command("wg", "pubkey")
	pubCmd.Stdin = strings.NewReader(priv + "\n")
	pubOut, err := pubCmd.Output()
	return priv, strings.TrimSpace(string(pubOut)), err
}

func setupWG(priv, arenaIP, hubPub, endpoint, extraSubnets string) error {
	allowedIPs := "10.66.0.0/16"
	if extraSubnets != "" {
		allowedIPs += ", " + extraSubnets
	}
	conf := fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = %s/32
MTU = 1420

[Peer]
PublicKey = %s
Endpoint = %s
AllowedIPs = %s
PersistentKeepalive = 25
`, priv, arenaIP, hubPub, endpoint, allowedIPs)
	os.MkdirAll("/etc/wireguard", 0700)
	os.WriteFile("/etc/wireguard/wg0.conf", []byte(conf), 0600)
	exec.Command("wg-quick", "down", "wg0").Run()
	out, err := exec.Command("wg-quick", "up", "wg0").CombinedOutput()
	if err != nil {
		return fmt.Errorf("wg-quick up: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func applyKillSwitch() {
	exec.Command("iptables", "-C", "OUTPUT", "-o", "wg0", "!", "-d", "10.66.0.0/16", "-j", "DROP").Run()
	exec.Command("iptables", "-A", "OUTPUT", "-o", "wg0", "!", "-d", "10.66.0.0/16", "-j", "DROP").Run()
}

// verifyTunnel waits up to `timeout` for the WireGuard peer to complete a
// handshake. It doesn't rely on ICMP (10.66.0.1 may not respond to ping),
// it reads `wg show wg0 latest-handshakes` and considers anything within
// the last 180s a live handshake. Pokes a UDP packet first to trigger the
// initial handshake if the kernel hasn't sent one yet.
func verifyTunnel(timeout time.Duration) bool {
	// Trigger a handshake by addressing the hub IP. The packet itself doesn't
	// need to be answered; sending it causes wg to start the handshake.
	exec.Command("ping", "-c", "1", "-W", "1", "10.66.0.1").Run()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.Command("wg", "show", "wg0", "latest-handshakes").Output()
		if err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				parts := strings.Fields(line)
				if len(parts) < 2 {
					continue
				}
				ts, perr := strconv.ParseInt(parts[1], 10, 64)
				if perr == nil && ts > 0 && time.Now().Unix()-ts < 180 {
					return true
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

func removeKillSwitch() {
	exec.Command("iptables", "-D", "OUTPUT", "-o", "wg0", "!", "-d", "10.66.0.0/16", "-j", "DROP").Run()
}

// ─── API helpers ─────────────────────────────────────────────────────────────

type tokenResp struct {
	Handle string `json:"handle"`
}

func fetchHandle(api, token string) (string, string, error) {
	for _, base := range apiURLVariants(api) {
		resp, err := httpClient.Get(base + "/token/" + token)
		if err != nil {
			continue
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			return "", base, fmt.Errorf("%s", raw)
		}
		var r tokenResp
		if err := json.Unmarshal(raw, &r); err != nil || r.Handle == "" {
			return "", base, fmt.Errorf("invalid token or token not registered on the site")
		}
		return r.Handle, base, nil
	}
	return "", "", fmt.Errorf("server unreachable at %s\n\n  Tip: pass --api http://SERVER_IP:PORT to specify the correct address", api)
}

type regResp struct {
	PlayerID    string `json:"player_id"`
	ArenaIP     string `json:"arena_ip"`
	HubPubkey   string `json:"hub_pubkey"`
	HubEndpoint string `json:"hub_endpoint"`
	ExtraRoutes string `json:"extra_routes"`
}

func register(api, token, handle, pub string) (*regResp, string, error) {
	payload, _ := json.Marshal(map[string]any{
		"invite_token": token, "handle": handle, "wg_pubkey": pub,
		"agent_version":       agentVersion,
		"consent_isolated_vm": true, "consent_range_only": true, "consent_scope": true,
	})
	for _, base := range apiURLVariants(api) {
		resp, err := httpClient.Post(base+"/register", "application/json", bytes.NewReader(payload))
		if err != nil {
			continue
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			return nil, base, fmt.Errorf("%s", raw)
		}
		var r regResp
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, base, fmt.Errorf("decode: %w", err)
		}
		if r.ArenaIP == "" || r.HubPubkey == "" {
			return nil, base, fmt.Errorf("incomplete response: %s", raw)
		}
		r.ArenaIP = strings.TrimSuffix(strings.Split(r.ArenaIP, "/")[0], "/32")
		return &r, base, nil
	}
	return nil, "", fmt.Errorf("server unreachable at %s", api)
}

// sendHeartbeat reports tunnel state to the API.
// tunnelUp=false sends a disconnect heartbeat — the server marks the player offline immediately.
func sendHeartbeat(st agentState, tunnelUp bool) {
	if !tunnelUp {
		// Use the stored state value rather than checking wg show (wg0 may already be down)
	} else {
		// Confirm wg0 is actually up
		tunnelUp = exec.Command("wg", "show", "wg0").Run() == nil
	}
	body, _ := json.Marshal(map[string]any{
		"player_id":       st.PlayerID,
		"player_arena_ip": st.ArenaIP,
		"handle":          st.Handle,
		"tunnel_up":       tunnelUp,
		"invite_token":    st.InviteToken,
		"agent_version":   agentVersion,
	})
	httpClient.Post(st.APIBase+"/heartbeat", "application/json", bytes.NewReader(body))
}

type hillInfo struct {
	ArenaIP    string `json:"arena_ip"`
	Tier       string `json:"tier"`
	ImageName  string `json:"image_name"`
	KingHandle string `json:"king_handle"`
	TTLSecs    int    `json:"ttl_secs"`
}

func fetchHills(api string) []hillInfo {
	resp, err := httpClient.Get(api + "/koth/hills")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var out struct {
		Hills []hillInfo `json:"hills"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	return out.Hills
}

type arenaStats struct {
	UserFlagPoints int `json:"user_flag_points"`
	RootFlagPoints int `json:"root_flag_points"`
}

func fetchArenaStats(api string) arenaStats {
	resp, err := httpClient.Get(api + "/arena/stats")
	if err != nil {
		return arenaStats{UserFlagPoints: 150, RootFlagPoints: 400}
	}
	defer resp.Body.Close()
	var s arenaStats
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil || s.UserFlagPoints == 0 {
		return arenaStats{UserFlagPoints: 150, RootFlagPoints: 400}
	}
	return s
}

// ─── Update ───────────────────────────────────────────────────────────────────

type updateInfo struct {
	Version     string `json:"version"`
	DownloadURL string `json:"download_url"`
}

// lanFallback is an optional same-LAN URL set at build time via:
//
//	go build -ldflags "-X main.lanFallback=http://10.0.0.42:8080"
//
// When set, the agent tries it after the primary defaultAPI fails — useful
// for LAN testers whose home router doesn't hairpin NAT to the public IP.
// Empty in source so the public repo doesn't leak deployment-specific addresses.
var lanFallback = ""

// publicFallback is the always-reachable public URL. Hardcoded so an agent
// built with a broken / arena-internal defaultAPI can still self-heal via
// auto-update. If you fork this for a private deployment, change it.
const publicFallback = "https://cyberkiller.net/api"

// apiURLVariants returns the given URL plus fallbacks:
// - port-8082 variant for old agents saved with :8080
// - LAN IP fallback (build-time) for same-LAN testers where router doesn't hairpin
// - WireGuard hub IP variant (10.66.0.1:8082) for after the tunnel is up
// - public-DNS fallback so an old broken binary can still reach the update server
func apiURLVariants(api string) []string {
	urls := []string{api}
	// port fallback: :8080/:8081 → :8082
	alt := strings.NewReplacer(":8080", ":8082", ":8081", ":8082").Replace(api)
	if alt != api {
		urls = append(urls, alt)
	}
	// LAN fallback (build-time)
	if lanFallback != "" && !strings.Contains(api, lanFallback) {
		urls = append(urls, lanFallback)
	}
	// WireGuard hub fallback: if not already targeting 10.66.0.1, add it
	if !strings.Contains(api, "10.66.0.1") {
		port := "8082"
		if strings.Contains(api, ":8080") {
			port = "8080"
		} else if strings.Contains(api, ":8081") {
			port = "8081"
		}
		urls = append(urls, "http://10.66.0.1:"+port)
	}
	// Public-DNS fallback — always reachable, always last so primary wins normally.
	if !strings.Contains(api, "cyberkiller.net") {
		urls = append(urls, publicFallback)
	}
	return urls
}

func fetchUpdateInfo(api string) (info updateInfo, available bool, reachable bool) {
	for _, base := range apiURLVariants(api) {
		resp, err := httpClient.Get(base + "/agent/version")
		if err != nil {
			continue
		}
		defer resp.Body.Close()
		var v updateInfo
		if err := json.NewDecoder(resp.Body).Decode(&v); err != nil || v.Version == "" {
			continue
		}
		return v, v.Version != agentVersion, true
	}
	return updateInfo{}, false, false
}

func applyUpdate(api string, v updateInfo, reconnect bool) {
	fmt.Printf("\n  ┌─────────────────────────────────────────┐\n")
	fmt.Printf("  │  UPDATE: v%-9s → v%-19s│\n", agentVersion, v.Version)
	fmt.Printf("  └─────────────────────────────────────────┘\n\n")
	fmt.Print("  [▸] Downloading new binary...        ")

	var dlResp *http.Response
	for _, base := range apiURLVariants(api) {
		r, err := httpClient.Get(base + v.DownloadURL)
		if err == nil && r.StatusCode == 200 {
			dlResp = r
			break
		}
	}
	if dlResp == nil {
		fmt.Println("FAIL — keeping current version")
		return
	}
	defer dlResp.Body.Close()

	exe, err := os.Executable()
	if err != nil {
		fmt.Println("FAIL — cannot locate binary")
		return
	}
	tmp := exe + ".update"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		fmt.Println("FAIL — cannot write update file")
		return
	}
	if _, err := io.Copy(f, dlResp.Body); err != nil {
		f.Close(); os.Remove(tmp)
		fmt.Println("FAIL — download interrupted")
		return
	}
	f.Close()
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		fmt.Println("FAIL — cannot replace binary")
		return
	}
	fmt.Println("✓")
	fmt.Printf("\n  ✓ Updated to v%s\n", v.Version)
	if reconnect {
		fmt.Printf("  ▸ Auto-reconnecting...\n\n")
		syscall.Exec(exe, []string{exe, "connect"}, os.Environ()) //nolint
	}
	fmt.Printf("  ▸ Reconnect: sudo %s connect\n\n", exe)
	os.Exit(0)
}

func checkForUpdate(api string) {
	fmt.Print("  [▸] Checking for updates...          ")
	info, available, reachable := fetchUpdateInfo(api)
	if !reachable {
		fmt.Printf("v%s (server unreachable — skipping)\n", agentVersion)
		return
	}
	if !available {
		fmt.Printf("v%s (up to date)\n", agentVersion)
		return
	}
	fmt.Printf("UPDATE AVAILABLE (v%s)\n", info.Version)
	applyUpdate(api, info, false) // startup: no reconnect (about to connect for first time)
}

// ─── State file ───────────────────────────────────────────────────────────────

func saveState(st agentState) {
	b, _ := json.Marshal(st)
	os.WriteFile(stateFile, b, 0600)
}

func loadState() (*agentState, error) {
	b, err := os.ReadFile(stateFile)
	if err != nil {
		return nil, err
	}
	var st agentState
	json.Unmarshal(b, &st)
	return &st, nil
}

// ─── UI ───────────────────────────────────────────────────────────────────────

func requireRoot() {
	if os.Getuid() != 0 {
		fmt.Fprintln(os.Stderr, "  Error: must run as root (WireGuard requires root)")
		fmt.Fprintln(os.Stderr, "  Usage: sudo ./cyberkiller-agent <TOKEN>")
		os.Exit(1)
	}
	// Make sure /usr/sbin and /sbin are searched — some sudo configs strip them.
	addPath := func(p string) {
		cur := os.Getenv("PATH")
		if !strings.Contains(cur, p) {
			os.Setenv("PATH", cur+":"+p)
		}
	}
	addPath("/usr/sbin")
	addPath("/sbin")
	addPath("/usr/local/sbin")
}

// ensureDependencies checks for wg, wg-quick, and iptables. If any are
// missing, it detects the system package manager and offers to install
// them with the player's permission. Bails out if they decline or no
// known package manager is found.
func ensureDependencies() {
	required := []string{"wg", "wg-quick", "iptables"}
	missing := []string{}
	for _, bin := range required {
		if _, err := exec.LookPath(bin); err != nil {
			missing = append(missing, bin)
		}
	}
	if len(missing) == 0 {
		return
	}

	pmName, pmCmds := detectPackageManager()
	fmt.Printf("\n  Missing required tools: %s\n", strings.Join(missing, ", "))
	if pmName == "" {
		fmt.Fprintln(os.Stderr, "  Could not detect a supported package manager.")
		fmt.Fprintln(os.Stderr, "  Install these packages manually and re-run:")
		fmt.Fprintln(os.Stderr, "    wireguard-tools, iptables")
		os.Exit(1)
	}

	fmt.Printf("  Detected package manager: %s\n", pmName)
	fmt.Println("  This will run:")
	for _, c := range pmCmds {
		fmt.Printf("    %s\n", strings.Join(c, " "))
	}
	fmt.Print("  Proceed? [Y/n]: ")
	sc := bufio.NewScanner(os.Stdin)
	sc.Scan()
	resp := strings.ToLower(strings.TrimSpace(sc.Text()))
	if resp == "n" || resp == "no" {
		fmt.Fprintln(os.Stderr, "  Cannot continue without required tools.")
		os.Exit(1)
	}

	for _, c := range pmCmds {
		fmt.Printf("\n  $ %s\n", strings.Join(c, " "))
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "\n  Install command failed: %v\n", err)
			fmt.Fprintln(os.Stderr, "  Please install wireguard-tools and iptables manually, then re-run.")
			os.Exit(1)
		}
	}

	// Re-check to confirm install actually placed the binaries on PATH.
	for _, bin := range required {
		if _, err := exec.LookPath(bin); err != nil {
			fmt.Fprintf(os.Stderr, "\n  Install completed but %s still not found in PATH.\n", bin)
			fmt.Fprintln(os.Stderr, "  Check your package repos and try again.")
			os.Exit(1)
		}
	}
	fmt.Println("\n  ✓ Dependencies installed.")
}

// detectPackageManager returns (name, commands-to-run). Commands are
// returned as a slice of argv slices so multi-step installs (e.g.
// apt-get update then apt-get install) can be expressed.
func detectPackageManager() (string, [][]string) {
	pkgs := []string{"wireguard-tools", "iptables"}
	candidates := []struct {
		name string
		bin  string
		cmds [][]string
	}{
		{"apt", "apt-get", [][]string{
			{"apt-get", "update", "-qq"},
			append([]string{"apt-get", "install", "-y"}, pkgs...),
		}},
		{"dnf", "dnf", [][]string{append([]string{"dnf", "install", "-y"}, pkgs...)}},
		{"yum", "yum", [][]string{append([]string{"yum", "install", "-y"}, pkgs...)}},
		{"pacman", "pacman", [][]string{append([]string{"pacman", "-S", "--noconfirm", "--needed"}, pkgs...)}},
		{"zypper", "zypper", [][]string{append([]string{"zypper", "install", "-y"}, pkgs...)}},
		{"apk", "apk", [][]string{append([]string{"apk", "add"}, pkgs...)}},
	}
	for _, c := range candidates {
		if _, err := exec.LookPath(c.bin); err == nil {
			return c.name, c.cmds
		}
	}
	return "", nil
}

func printHeader() {
	fmt.Println()
	fmt.Println("  ┌─────────────────────────────────────────┐")
	fmt.Println("  │        CYBERKILLER ARENA AGENT          │")
	fmt.Printf("  │              v%-26s│\n", agentVersion)
	fmt.Println("  └─────────────────────────────────────────┘")
	fmt.Println()
}

func showRules(handle string) bool {
	fmt.Println("  ── RULES OF ENGAGEMENT ──────────────────────────")
	fmt.Println()
	fmt.Println("  1. Attack ONLY 10.66.20.x range machines.")
	fmt.Println("  2. Never touch the control plane (10.66.0.1).")
	fmt.Println("  3. Do not attack other players.")
	fmt.Println("  4. Do not tunnel real-world traffic through the arena.")
	fmt.Println("  5. All activity is logged. No exceptions.")
	fmt.Println()
	fmt.Printf("  You are connecting as: %s\n\n", handle)
	fmt.Print("  Type ACCEPT to agree and connect: ")
	sc := bufio.NewScanner(os.Stdin)
	sc.Scan()
	resp := strings.ToUpper(strings.TrimSpace(sc.Text()))
	return resp == "ACCEPT" || resp == "AGREE"
}

func printWelcome(handle, arenaIP string, hills []hillInfo, pts arenaStats) {
	fmt.Println()
	fmt.Println("  ─────────────────────────────────────────────────")
	fmt.Printf("  WELCOME, %s\n", strings.ToUpper(handle))
	fmt.Println("  ─────────────────────────────────────────────────")
	fmt.Printf("  Arena IP:  %s\n", arenaIP)
	fmt.Printf("  Tunnel:    UP\n")
	if len(hills) > 0 {
		fmt.Printf("  Targets:   %d active — check the hub at the arena URL\n", len(hills))
	}
	fmt.Println()
	printNextSteps(false, "")
}

// printNextSteps renders a high-visibility "WHAT TO DO NEXT" panel so first-time
// users always know how to disconnect, check status, and where to go for help.
// Same content shown in both foreground and background modes — minor wording
// difference for the heartbeat line.
func printNextSteps(background bool, logPath string) {
	const bar = "  ────────────────────────────────────────────────────────────"
	fmt.Println(bar)
	fmt.Println("  WHAT NOW")
	fmt.Println(bar)
	fmt.Println("  ✓  You are connected to the arena over WireGuard.")
	fmt.Println("     Range machines live at 10.66.20.x — start with nmap.")
	fmt.Println()
	fmt.Println("  ↗  Open the hub:        https://cyberkiller.net/hub")
	fmt.Println("     Scoreboard, chat, machine list, intel drops.")
	fmt.Println()
	if background {
		fmt.Printf("  📜 Tail logs:           tail -f %s\n", logPath)
	} else {
		fmt.Println("  ⌨  Heartbeating every 10s. This window must stay open.")
		fmt.Println("     To run in background instead: Ctrl+C, then")
		fmt.Println("       sudo ./cyberkiller-agent connect --detach")
	}
	fmt.Println()
	fmt.Println("  ⛔ DISCONNECT (cleanly tears down VPN + kill-switch):")
	if background {
		fmt.Println("       sudo ./cyberkiller-agent disconnect")
	} else {
		fmt.Println("       Ctrl+C in this window")
		fmt.Println("       — or from another shell —")
		fmt.Println("       sudo ./cyberkiller-agent disconnect")
	}
	fmt.Println()
	fmt.Println("  🔧 Status:               sudo ./cyberkiller-agent status")
	fmt.Println("  🐛 Bug / issue:          https://cyberkiller.net/report")
	fmt.Println("  📖 Rules + scoring:      https://cyberkiller.net/hub?tab=rules")
	fmt.Println(bar)
	fmt.Println()
}

// ─── Status ───────────────────────────────────────────────────────────────────

func runStatus() {
	st, err := loadState()
	if err != nil {
		fmt.Println("  Status: not connected (no state file)")
		return
	}
	tunnelUp := exec.Command("wg", "show", "wg0").Run() == nil
	tunnelStr := "DOWN"
	if tunnelUp {
		tunnelStr = "UP"
	}
	fmt.Printf("  Operative: %s\n", st.Handle)
	fmt.Printf("  Arena IP:  %s\n", st.ArenaIP)
	fmt.Printf("  Tunnel:    %s\n", tunnelStr)
	fmt.Printf("  API:       %s\n", st.APIBase)
	if st.BgPID > 0 {
		alive := ""
		if proc, err := os.FindProcess(st.BgPID); err == nil && proc.Signal(syscall.Signal(0)) == nil {
			alive = " (running)"
		} else {
			alive = " (dead — run disconnect to clean up)"
		}
		fmt.Printf("  BgPID:     %d%s\n", st.BgPID, alive)
	}
}

// ─── Submit (flag submission via API) ─────────────────────────────────────────

func runSubmit(args []string) {
	st, err := loadState()
	if err != nil {
		fmt.Fprintln(os.Stderr, "  Not connected — run agent with your invite token first.")
		os.Exit(1)
	}
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "  Usage: cyberkiller-agent submit <flag|user|root|target|koth> [--ip IP] [--value VALUE]")
		os.Exit(1)
	}
	fs := flag.NewFlagSet("submit", flag.ExitOnError)
	ip := fs.String("ip", "", "target IP")
	value := fs.String("value", "", "flag value / token")
	fs.Parse(args[1:])

	kind := args[0]
	var endpoint string
	var body []byte
	switch kind {
	case "flag", "user", "root":
		endpoint = "/flag/submit"
		body, _ = json.Marshal(map[string]string{
			"attacker_id": st.PlayerID, "arena_ip": *ip,
			"flag": *value, "invite_token": st.InviteToken,
		})
	case "target":
		endpoint = "/kill/target"
		body, _ = json.Marshal(map[string]string{
			"attacker_id": st.PlayerID, "arena_ip": *ip,
			"value": *value, "invite_token": st.InviteToken,
		})
	case "koth":
		endpoint = "/koth/claim"
		body, _ = json.Marshal(map[string]string{
			"attacker_id": st.PlayerID, "arena_ip": *ip,
			"token": *value, "invite_token": st.InviteToken,
		})
	default:
		fmt.Fprintf(os.Stderr, "  Unknown kind: %s\n", kind)
		os.Exit(1)
	}
	resp, err := httpClient.Post(st.APIBase+endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	fmt.Println(string(raw))
}
