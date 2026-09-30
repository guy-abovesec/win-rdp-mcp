//go:build linux

package main

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"image"
	_ "image/png" // register the PNG decoder for encodeSnapshot
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/pflag"
)

// The controller is the operator-side half of the system. It runs on Linux, is
// the MCP server the AI connects to over stdio, and drives the Windows target
// entirely over RDP:
//
//   - The desktop tier (screenshots, mouse, keyboard) is served locally by
//     holding a live xfreerdp session inside a headless Xvfb and wrapping it
//     with import (capture) and xdotool (input). No footprint on the target.
//   - The system tier (window tree, shell, registry, files, services, ...) is
//     served by the agent, which the controller pushes into the RDP session
//     over drive redirection and talks to through the dir transport — files on
//     that same redirected drive. Still nothing but RDP on the wire.
//
// The operator provides only RDP credentials; everything else is bootstrapped.

// desktopTools are answered locally via xdotool/import — they need no agent, so
// they work the moment the RDP session is up. Everything else is proxied to the
// in-session agent.
var desktopTools = map[string]bool{
	"Click": true, "Type": true, "Move": true, "Scroll": true,
	"Shortcut": true, "Wait": true, "Snapshot": true,
}

func controlMain(argv []string) error {
	fs := pflag.NewFlagSet("win-rdp-mcp control", pflag.ContinueOnError)
	fs.SortFlags = false
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "win-rdp-mcp control %s — drive a Windows target over RDP, expose it as MCP\n\n"+
			"Usage:\n  win-rdp-mcp control --target HOST --user USER [flags]\n\n"+
			"The password comes from --pass-file or $%s. Flags:\n", Version, envTargetPass)
		fmt.Fprint(fs.Output(), fs.FlagUsages())
	}

	var (
		target      = fs.StringP("target", "t", "", "target host or host:port (RDP)")
		user        = fs.StringP("user", "u", "", "RDP username")
		domain      = fs.StringP("domain", "D", "", "RDP domain (blank for a local account)")
		passFile    = fs.StringP("pass-file", "p", "", "file holding the RDP password (else $"+envTargetPass+")")
		width       = fs.IntP("width", "W", 1280, "session width in pixels")
		height      = fs.IntP("height", "H", 800, "session height in pixels")
		agentExe    = fs.String("agent-exe", "", "path to the Windows agent binary to push (default: ./win-rdp-mcp.exe)")
		noAgent     = fs.Bool("no-agent", false, "desktop tools only; do not push the in-session agent")
		debug       = fs.BoolP("debug", "d", false, "log RDP and bootstrap detail to stderr")
		enableAll   = fs.BoolP("enable-all", "a", false, "enable every tool, including destructive tier 3")
		enableTier3 = fs.BoolP("enable-tier3", "3", false, "enable the destructive tier 3 tools")
		disableT2   = fs.BoolP("disable-tier2", "2", false, "disable the interactive tier 2 tools")
	)
	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return err
	}

	pass, err := resolvePassword(*passFile)
	if err != nil {
		return err
	}
	if *target == "" || *user == "" {
		fs.Usage()
		return fmt.Errorf("both --target and --user are required")
	}

	enabled, err := resolveEnabledTools(toolSelection{
		enableTier3:  *enableTier3,
		disableTier2: *disableT2,
		enableAll:    *enableAll,
	})
	if err != nil {
		return err
	}

	rdpBin, err := findControllerDeps()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The RDP session is not opened here but on the first tool call. Windows
	// gives a user one session, so every connect takes it from whoever held it;
	// a controller that connected at startup would take it for clients that
	// only list tools and exit (health checks, version probes, a second MCP
	// client launching the same config), and would make the MCP handshake wait
	// on the RDP logon.
	sess := &rdpSession{
		host: *target, user: *user, domain: *domain, pass: pass,
		width: *width, height: *height, debug: *debug,
		rdpBin: rdpBin, life: ctx,
	}
	defer sess.stop()

	var proxy *agentProxy
	if !*noAgent {
		exe := *agentExe
		if exe == "" {
			exe = defaultAgentExe()
		}
		proxy = &agentProxy{session: sess, agentExe: exe, enabled: enabled}
		// Bootstrap runs in the background once the session first comes up, so
		// the desktop tier is usable immediately; system tools wait for it
		// rather than blocking the connect on it.
		sess.onFirstConnect = func(ctx context.Context) {
			if err := proxy.bootstrap(ctx); err != nil {
				logf("agent bootstrap failed (desktop tools still work): %v", err)
			}
		}
	}

	srv := mcp.NewServer(
		&mcp.Implementation{Name: "win-rdp-mcp-control", Version: Version},
		&mcp.ServerOptions{Instructions: controllerInstructions(*noAgent)},
	)
	if err := registerControllerTools(srv, enabled, sess, proxy); err != nil {
		return err
	}

	logf("controller ready — %d tools (%d desktop-local, rest via agent); RDP connects to %s on the first tool call",
		len(enabled), countLocal(enabled), sess.host)
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

const envTargetPass = "WIN_RDP_TARGET_PASS"

func resolvePassword(passFile string) (string, error) {
	if passFile != "" {
		data, err := os.ReadFile(passFile)
		if err != nil {
			return "", fmt.Errorf("reading --pass-file: %w", err)
		}
		return strings.TrimRight(string(data), "\r\n"), nil
	}
	if p := os.Getenv(envTargetPass); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("no password: set $%s or pass --pass-file", envTargetPass)
}

func defaultAgentExe() string {
	// Next to the controller binary, then the working directory.
	if self, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(self), "win-rdp-mcp.exe")
		if fileExists(cand) {
			return cand
		}
	}
	return "win-rdp-mcp.exe"
}

// findControllerDeps checks the host programs the controller shells out to and
// returns the FreeRDP client to run. It runs at startup because the RDP session
// itself only comes up on the first tool call.
//
// Debian and Ubuntu install the FreeRDP 3 client as xfreerdp3 (next to a
// FreeRDP 2 xfreerdp, on older releases); Nix and upstream builds call it
// xfreerdp. The versioned name wins when both are present.
func findControllerDeps() (string, error) {
	var missing []string
	rdpBin := ""
	for _, name := range []string{"xfreerdp3", "xfreerdp"} {
		if path, err := exec.LookPath(name); err == nil {
			rdpBin = path
			break
		}
	}
	if rdpBin == "" {
		missing = append(missing, "xfreerdp3 (FreeRDP 3)")
	}
	for _, name := range []string{"Xvfb", "xdpyinfo", "xdotool", "import"} {
		if _, err := exec.LookPath(name); err != nil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("the controller needs %s on PATH (Debian/Ubuntu: apt install freerdp3-x11 xvfb x11-utils xdotool imagemagick)",
			strings.Join(missing, ", "))
	}
	return rdpBin, nil
}

func countLocal(enabled map[string]bool) int {
	n := 0
	for name := range enabled {
		if desktopTools[name] {
			n++
		}
	}
	return n
}

// ── RDP session ──────────────────────────────────────────────────────────────

// rdpSession holds one persistent headless RDP connection: an Xvfb display with
// xfreerdp drawing the remote desktop into it. Screenshots read that display;
// input is injected into it and FreeRDP relays it to the target. Coordinates
// are 1:1 because the Xvfb, the RDP session and the remote desktop are all the
// same size.
//
// The connection is opened on first use and reopened whenever the client has
// exited; see ensureConnected.
type rdpSession struct {
	host, user, domain, pass string
	width, height            int
	debug                    bool
	rdpBin                   string // the FreeRDP client, from findControllerDeps

	// life bounds the process: the RDP client and background work started on
	// connect outlive the tool call that triggered them.
	life context.Context
	// onFirstConnect runs once, in the background, after the first successful
	// connect. The agent bootstrap hangs off it.
	onFirstConnect func(context.Context)

	display   string // e.g. ":140"
	workdir   string // exposed to the session as \\tsclient\mcp
	xvfb      *exec.Cmd
	client    *rdpClient // the current xfreerdp; nil before the first connect
	connected bool       // a connect has succeeded at least once

	mu sync.Mutex // desktop input/capture is one physical device: serialise it
}

// reconnectSettle is how long a reconnected session gets to repaint before the
// tool that triggered the reconnect goes ahead.
const reconnectSettle = 2 * time.Second

// ensureConnected brings the RDP session up if it is not: on first use, and
// again whenever xfreerdp has exited. The usual cause of an exit is another
// client logging in as the same user — Windows gives a user one session and
// hands it to the newest connection — and without a reconnect the controller
// would keep capturing an empty Xvfb (a black screenshot) and typing into
// nothing. Callers hold s.mu.
func (s *rdpSession) ensureConnected(ctx context.Context) error {
	if s.client != nil && s.client.running() {
		return nil
	}
	if s.xvfb == nil {
		if err := s.startDisplay(ctx); err != nil {
			return err
		}
	}
	reconnect := s.connected
	if reconnect {
		logf("RDP client exited (%s); reconnecting", s.client.exitReason())
	}
	if err := s.connect(ctx); err != nil {
		return err
	}
	if reconnect {
		// Reattaching to the existing session lands on its desktop, so there is
		// no lock screen to wake — and wake's click and Enter would act on
		// whatever that desktop is showing.
		sleepCtx(ctx, reconnectSettle)
		logf("RDP session reconnected: %s", s.host)
		return nil
	}
	// A fresh connection often lands on the lock screen; a click plus Enter
	// dismisses it and NLA completes the logon to a live desktop.
	s.wake(ctx)
	s.connected = true
	logf("RDP session up: %s (%dx%d)", s.host, s.width, s.height)
	if s.debug {
		logf("exchange dir: %s", s.workdir)
	}
	if s.onFirstConnect != nil {
		go s.onFirstConnect(s.life)
	}
	return nil
}

// startDisplay creates the exchange dir and the headless X server the session
// is drawn into. Both outlive any one RDP client, so the redirected drive keeps
// its path across reconnects.
func (s *rdpSession) startDisplay(ctx context.Context) error {
	if s.workdir == "" {
		dir, err := os.MkdirTemp("", "win-rdp-mcp-ctl-")
		if err != nil {
			return err
		}
		s.workdir = dir
	}

	display, err := allocDisplay()
	if err != nil {
		return err
	}
	xvfb := exec.Command("Xvfb", display, "-screen", "0",
		fmt.Sprintf("%dx%dx24", s.width, s.height), "-nolisten", "tcp")
	if err := xvfb.Start(); err != nil {
		return fmt.Errorf("starting Xvfb: %w", err)
	}
	s.display = display
	if err := s.waitForX(ctx); err != nil {
		xvfb.Process.Kill()
		xvfb.Wait()
		return err
	}
	s.xvfb = xvfb
	return nil
}

// connect starts xfreerdp and waits for its window, which it maps once the
// logon has gone through.
func (s *rdpSession) connect(ctx context.Context) error {
	var extra io.Writer
	if s.debug {
		extra = os.Stderr
	}
	client, err := startRDPClient(s.rdpBin, s.clientArgs(),
		append(os.Environ(), "DISPLAY="+s.display, "KRB5_CONFIG=/dev/null"), extra)
	if err != nil {
		return err
	}
	s.client = client

	err = poll(ctx, 30*time.Second, func() bool {
		return !client.running() || s.windowID() != ""
	}, "xfreerdp window never appeared (RDP connection failed — check host/credentials)")
	if err == nil && !client.running() {
		err = fmt.Errorf("RDP connection to %s failed: %s", s.host, client.exitReason())
	}
	if err != nil {
		client.kill()
		return err
	}
	return nil
}

// clientArgs is the xfreerdp command line, password included, which is why it
// is handed over on stdin rather than as argv (see startRDPClient).
func (s *rdpSession) clientArgs() []string {
	args := []string{
		"/v:" + s.host,
		"/u:" + s.user,
		"/p:" + s.pass,
		"/cert:ignore", "/sec:nla",
		fmt.Sprintf("/size:%dx%d", s.width, s.height),
		"/drive:mcp," + s.workdir, // the file channel + agent delivery
		"+clipboard",
		"/log-level:" + logLevel(s.debug),
	}
	if s.domain != "" {
		args = append(args, "/d:"+s.domain)
	}
	return args
}

func (s *rdpSession) stop() {
	if s.client != nil {
		s.client.kill()
	}
	if s.xvfb != nil && s.xvfb.Process != nil {
		s.xvfb.Process.Kill()
		s.xvfb.Wait()
	}
	if s.workdir != "" {
		os.RemoveAll(s.workdir)
	}
}

func logLevel(debug bool) string {
	if debug {
		return "INFO"
	}
	return "WARN"
}

// waitForX blocks until the Xvfb display accepts connections.
func (s *rdpSession) waitForX(ctx context.Context) error {
	return poll(ctx, 10*time.Second, func() bool {
		return exec.Command("xdpyinfo", "-display", s.display).Run() == nil
	}, "Xvfb display "+s.display+" did not come up")
}

// rdpClient is one run of xfreerdp. It is watched from the moment it starts, so
// an exit is noticed (and the process reaped) rather than left as a zombie the
// controller keeps sending input to.
type rdpClient struct {
	cmd  *exec.Cmd
	log  *logTail
	done chan struct{} // closed once the process has exited
	err  error         // its exit status; read only after done is closed
}

// startRDPClient runs name with its arguments supplied on stdin through
// FreeRDP's /args-from:stdin, so the password never appears in argv, which any
// local user can read from /proc. /args-from is in every FreeRDP 3 release:
// https://github.com/FreeRDP/FreeRDP/commit/9b67ef1a87 (in 3.0.0-beta1).
// stderr is kept in a small tail for error reports and also copied to extra
// when that is non-nil.
func startRDPClient(name string, args, env []string, extra io.Writer) (*rdpClient, error) {
	c := &rdpClient{log: newLogTail(rdpLogLines), done: make(chan struct{})}
	c.cmd = exec.Command(name, "/args-from:stdin")
	c.cmd.Env = env
	c.cmd.Stdin = strings.NewReader(strings.Join(args, "\n") + "\n")
	c.cmd.Stderr = c.log
	if extra != nil {
		c.cmd.Stderr = io.MultiWriter(c.log, extra)
	}
	if err := c.cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", name, err)
	}
	go func() {
		c.err = c.cmd.Wait()
		close(c.done)
	}()
	return c, nil
}

func (c *rdpClient) running() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// kill stops the client and waits for it to be reaped.
func (c *rdpClient) kill() {
	if c.running() {
		c.cmd.Process.Kill()
	}
	<-c.done
}

// exitReason says why the client exited: its exit status, what xfreerdp means
// by it, and FreeRDP's error line, e.g. "exit status 134 (logon failed: wrong
// user name or password): [nla_recv_pdu]: ERRCONNECT_LOGON_FAILURE
// [0x00020014]". Only meaningful once running() is false.
func (c *rdpClient) exitReason() string {
	reason := "exited"
	if c.err != nil {
		reason = c.err.Error()
		var exit *exec.ExitError
		if errors.As(c.err, &exit) {
			if meaning, ok := xfreerdpExitCodes[exit.ExitCode()]; ok {
				reason += " (" + meaning + ")"
			}
		}
	}
	if line := c.log.lastError(); line != "" {
		reason += ": " + line
	}
	return reason
}

// xfreerdpExitCodes explains the xfreerdp exit statuses an operator is likely
// to meet. The values are the XF_EXIT_* enum, unchanged across FreeRDP 3:
// https://github.com/FreeRDP/FreeRDP/blob/3.15.0/client/X11/xfreerdp.h
var xfreerdpExitCodes = map[int]string{
	1:   "disconnected by the server",
	2:   "logged off",
	3:   "idle timeout",
	4:   "logon timeout",
	5:   "another connection took the session",
	7:   "connection denied",
	9:   "the account may not log in over RDP",
	11:  "disconnected by the user",
	131: "connection failed",
	132: "authentication failed",
	133: "security negotiation failed",
	134: "logon failed: wrong user name or password",
	135: "account locked out",
	139: "DNS error",
	140: "host name not found",
	141: "could not connect",
	143: "TLS connect failed",
}

// rdpLogLines bounds how much xfreerdp stderr is kept for error reports.
const rdpLogLines = 32

// logTail is an io.Writer that keeps the last n complete lines written to it.
type logTail struct {
	mu      sync.Mutex
	n       int
	lines   []string
	partial []byte
}

func newLogTail(n int) *logTail { return &logTail{n: n} }

func (t *logTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.partial = append(t.partial, p...)
	for {
		i := bytes.IndexByte(t.partial, '\n')
		if i < 0 {
			break
		}
		t.lines = append(t.lines, strings.TrimRight(string(t.partial[:i]), "\r"))
		t.partial = t.partial[i+1:]
	}
	if over := len(t.lines) - t.n; over > 0 {
		t.lines = append(t.lines[:0], t.lines[over:]...)
	}
	return len(p), nil
}

// lastError returns FreeRDP's most telling recent ERROR message, without the
// timestamp and thread prefix: "[func]: message". A line naming an
// ERRCONNECT_/ERRINFO_ code wins over later ones: FreeRDP logs the cause first
// and then generic failures as the connection unwinds.
func (t *logTail) lastError() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	fallback := ""
	for i := len(t.lines) - 1; i >= 0; i-- {
		line := t.lines[i]
		if !strings.Contains(line, "[ERROR]") {
			continue
		}
		msg := line
		if _, rest, ok := strings.Cut(line, "] - "); ok {
			msg = rest
		}
		if strings.Contains(msg, "ERRCONNECT_") || strings.Contains(msg, "ERRINFO_") {
			return msg
		}
		if fallback == "" {
			fallback = msg
		}
	}
	return fallback
}

func (s *rdpSession) windowID() string {
	for _, sel := range [][]string{{"--name", "FreeRDP"}, {"--class", "xfreerdp"}} {
		out, err := s.xdo(append([]string{"search"}, sel...)...)
		if err == nil {
			if id := strings.TrimSpace(firstLine(out)); id != "" {
				return id
			}
		}
	}
	return ""
}

// wake nudges a possible lock screen: activate the window, click, press Enter,
// and give the desktop a moment to paint.
func (s *rdpSession) wake(ctx context.Context) {
	if id := s.windowID(); id != "" {
		s.xdo("windowactivate", id)
	}
	s.xdo("mousemove", strconv.Itoa(s.width/2), strconv.Itoa(s.height/2), "click", "1")
	sleepCtx(ctx, 700*time.Millisecond)
	s.xdo("key", "Return")
	sleepCtx(ctx, 3*time.Second)
}

// xdo runs an xdotool command against this session's display.
func (s *rdpSession) xdo(args ...string) (string, error) {
	cmd := exec.Command("xdotool", args...)
	cmd.Env = append(os.Environ(), "DISPLAY="+s.display)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("xdotool %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// capture grabs the whole display as PNG bytes — the remote desktop at 1:1.
func (s *rdpSession) capture() ([]byte, error) {
	cmd := exec.Command("import", "-window", "root", "png:-")
	cmd.Env = append(os.Environ(), "DISPLAY="+s.display)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("screen capture failed: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// ── Desktop tools (local) ────────────────────────────────────────────────────

func (s *rdpSession) snapshot(ctx context.Context, args arguments, proxy *agentProxy) (toolResult, error) {
	s.mu.Lock()
	err := s.ensureConnected(ctx)
	var png []byte
	if err == nil {
		png, err = s.capture()
	}
	s.mu.Unlock()
	if err != nil {
		return toolResult{}, err
	}

	var blocks []contentBlock
	if args.boolOr("use_vision", true) {
		data, mime := encodeSnapshot(png, args.intOr("max_width", 0), args.intOr("quality", 75))
		blocks = append(blocks, contentBlock{Type: "image", Data: data, MIME: mime})
	}

	// If the agent is up, borrow its window/element enumeration (screenshot
	// suppressed) so the snapshot carries clickable targets, not just pixels.
	text := fmt.Sprintf("Remote desktop %dx%d (via RDP).", s.width, s.height)
	if proxy != nil && proxy.ready() {
		sub := map[string]any{"use_vision": false}
		if r, err := proxy.call(ctx, "Snapshot", sub); err == nil {
			if t := firstText(r.Content); t != "" {
				text = t
			}
		}
	} else if proxy != nil {
		text += " (window/element list appears once the in-session agent finishes starting.)"
	}
	blocks = append(blocks, contentBlock{Type: "text", Text: text})
	return toolResult{Content: blocks}, nil
}

func (s *rdpSession) click(ctx context.Context, args arguments) (toolResult, error) {
	x, y := args.intOr("x", 0), args.intOr("y", 0)
	button := map[string]string{"left": "1", "middle": "2", "right": "3"}[args.stringOr("button", "left")]
	if button == "" {
		button = "1"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureConnected(ctx); err != nil {
		return toolResult{}, err
	}
	switch action := args.stringOr("action", "click"); action {
	case "hover":
		if _, err := s.xdo("mousemove", itoa(x), itoa(y)); err != nil {
			return toolResult{}, err
		}
		return textResult("Hovered at (%d,%d)", x, y), nil
	case "double":
		if _, err := s.xdo("mousemove", itoa(x), itoa(y), "click", "--repeat", "2", button); err != nil {
			return toolResult{}, err
		}
		return textResult("Double-clicked at (%d,%d)", x, y), nil
	default:
		if _, err := s.xdo("mousemove", itoa(x), itoa(y), "click", button); err != nil {
			return toolResult{}, err
		}
		return textResult("Clicked %s at (%d,%d)", args.stringOr("button", "left"), x, y), nil
	}
}

func (s *rdpSession) typeText(ctx context.Context, args arguments) (toolResult, error) {
	text := args.stringOr("text", "")
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureConnected(ctx); err != nil {
		return toolResult{}, err
	}
	if x, y := args.intOr("x", 0), args.intOr("y", 0); x != 0 || y != 0 {
		if _, err := s.xdo("mousemove", itoa(x), itoa(y), "click", "1"); err != nil {
			return toolResult{}, err
		}
	}
	if args.boolOr("clear", false) {
		s.xdo("key", "ctrl+a")
		s.xdo("key", "Delete")
	}
	if text != "" {
		if _, err := s.xdo("type", "--", text); err != nil {
			return toolResult{}, err
		}
	}
	if args.boolOr("press_enter", false) {
		s.xdo("key", "Return")
	}
	return textResult("Typed %d characters", len([]rune(text))), nil
}

func (s *rdpSession) move(ctx context.Context, args arguments) (toolResult, error) {
	x, y := args.intOr("x", 0), args.intOr("y", 0)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureConnected(ctx); err != nil {
		return toolResult{}, err
	}
	if args.boolOr("drag", false) {
		sx, sy := args.intOr("start_x", 0), args.intOr("start_y", 0)
		if sx != 0 || sy != 0 {
			s.xdo("mousemove", itoa(sx), itoa(sy))
		}
		s.xdo("mousedown", "1")
		s.xdo("mousemove", itoa(x), itoa(y))
		s.xdo("mouseup", "1")
		return textResult("Dragged to (%d,%d)", x, y), nil
	}
	if _, err := s.xdo("mousemove", itoa(x), itoa(y)); err != nil {
		return toolResult{}, err
	}
	return textResult("Moved to (%d,%d)", x, y), nil
}

func (s *rdpSession) scroll(ctx context.Context, args arguments) (toolResult, error) {
	amount := args.intOr("amount", 0)
	horizontal := args.boolOr("horizontal", false)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureConnected(ctx); err != nil {
		return toolResult{}, err
	}
	if x, y := args.intOr("x", 0), args.intOr("y", 0); x != 0 || y != 0 {
		s.xdo("mousemove", itoa(x), itoa(y))
	}
	// xdotool scrolls one "click" per button press: 4/5 vertical, 6/7 horizontal.
	button := "4" // up
	if amount < 0 {
		button = "5" // down
	}
	if horizontal {
		if amount < 0 {
			button = "6"
		} else {
			button = "7"
		}
	}
	n := amount
	if n < 0 {
		n = -n
	}
	for range n {
		s.xdo("click", button)
	}
	dir := "vertically"
	if horizontal {
		dir = "horizontally"
	}
	return textResult("Scrolled %d %s", amount, dir), nil
}

func (s *rdpSession) shortcut(ctx context.Context, args arguments) (toolResult, error) {
	raw := args.stringOr("keys", "")
	combo := xdotoolCombo(raw)
	if combo == "" {
		return toolResult{}, fmt.Errorf("no keys given")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureConnected(ctx); err != nil {
		return toolResult{}, err
	}
	if _, err := s.xdo("key", combo); err != nil {
		return toolResult{}, err
	}
	return textResult("Executed shortcut: %s", raw), nil
}

func waitTool2(ctx context.Context, args arguments) (toolResult, error) {
	d := min(max(time.Duration(args.floatOr("seconds", 1.0)*float64(time.Second)), 0), maxWait)
	if !sleepCtx(ctx, d) {
		return toolResult{}, ctx.Err()
	}
	return textResult("Waited %s", d), nil
}

// xdotoolCombo maps a "ctrl+shift+esc" style string onto xdotool keysyms.
func xdotoolCombo(raw string) string {
	repl := map[string]string{
		"win": "super", "cmd": "super", "meta": "super",
		"esc": "Escape", "escape": "Escape", "enter": "Return", "return": "Return",
		"del": "Delete", "delete": "Delete", "ins": "Insert", "insert": "Insert",
		"pageup": "Prior", "pagedown": "Next", "space": "space", "tab": "Tab",
		"up": "Up", "down": "Down", "left": "Left", "right": "Right",
		"home": "Home", "end": "End", "backspace": "BackSpace", "printscreen": "Print",
	}
	var parts []string
	for p := range strings.SplitSeq(raw, "+") {
		k := strings.TrimSpace(strings.ToLower(p))
		if k == "" {
			continue
		}
		if mapped, ok := repl[k]; ok {
			parts = append(parts, mapped)
		} else {
			parts = append(parts, k)
		}
	}
	return strings.Join(parts, "+")
}

// ── MCP registration + routing ───────────────────────────────────────────────

func registerControllerTools(srv *mcp.Server, enabled map[string]bool, sess *rdpSession, proxy *agentProxy) error {
	specs, err := loadToolSpecs()
	if err != nil {
		return err
	}
	for _, spec := range specs {
		if !enabled[spec.Name] {
			continue
		}
		var schema jsonschema.Schema
		if err := json.Unmarshal(spec.InputSchema, &schema); err != nil {
			return fmt.Errorf("tool %s: %w", spec.Name, err)
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			return fmt.Errorf("tool %s: %w", spec.Name, err)
		}
		tool := &mcp.Tool{
			Name: spec.Name, Description: spec.Description,
			InputSchema: &schema, Annotations: annotationsFor(spec.Name),
		}
		srv.AddTool(tool, routeTool(spec.Name, resolved, sess, proxy))
	}
	return nil
}

// routeTool sends desktop tools to the local xdotool/import handlers and every
// other tool to the in-session agent.
func routeTool(name string, schema *jsonschema.Resolved, sess *rdpSession, proxy *agentProxy) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := decodeArguments(req.Params.Arguments)
		if err != nil {
			return errorResult("Invalid arguments: " + err.Error()), nil
		}
		if err := schema.Validate(map[string]any(args)); err != nil {
			return errorResult("Invalid arguments: " + err.Error()), nil
		}

		if desktopTools[name] {
			result, err := localDesktop(ctx, name, sess, proxy, args)
			if err != nil {
				return errorResult(name + " error: " + err.Error()), nil
			}
			return toCallResult(result), nil
		}

		if proxy == nil {
			return errorResult(name + " needs the in-session agent, which is disabled (-no-agent)."), nil
		}
		result, err := proxy.call(ctx, name, map[string]any(args))
		if err != nil {
			return errorResult(name + " error: " + err.Error()), nil
		}
		return toCallResult(result), nil
	}
}

func localDesktop(ctx context.Context, name string, sess *rdpSession, proxy *agentProxy, args arguments) (toolResult, error) {
	switch name {
	case "Snapshot":
		return sess.snapshot(ctx, args, proxy)
	case "Click":
		return sess.click(ctx, args)
	case "Type":
		return sess.typeText(ctx, args)
	case "Move":
		return sess.move(ctx, args)
	case "Scroll":
		return sess.scroll(ctx, args)
	case "Shortcut":
		return sess.shortcut(ctx, args)
	case "Wait":
		return waitTool2(ctx, args)
	}
	return toolResult{}, fmt.Errorf("no local handler for %s", name)
}

func controllerInstructions(noAgent bool) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteByte('\n') }
	w("# win-rdp-mcp (controller)")
	w("")
	w("Drives a Windows target over RDP. You are given only RDP credentials; the desktop is driven directly and the system agent is pushed into the session automatically.")
	w("")
	w("- Start with `Snapshot` to see the screen. Coordinates are screen pixels at the session resolution.")
	w("- `Click`, `Type`, `Move`, `Scroll`, `Shortcut` drive the desktop with no footprint on the target.")
	if !noAgent {
		w("- The window/element list, `Shell`, files, registry, services and the rest come from an agent pushed into the session over RDP. It may take a few seconds after startup before those answer.")
	} else {
		w("- Running with -no-agent: only the desktop tools above are available.")
	}
	return strings.TrimRight(b.String(), "\n")
}

// ── small helpers ────────────────────────────────────────────────────────────

func itoa(n int) string { return strconv.Itoa(n) }

// encodeSnapshot re-encodes the captured PNG as JPEG (smaller, fewer tokens),
// downscaling to maxWidth. If decoding or encoding fails it hands back the
// original PNG so a snapshot is never lost to a compression hiccup.
func encodeSnapshot(png []byte, maxWidth, quality int) ([]byte, string) {
	img, _, err := image.Decode(bytes.NewReader(png))
	if err != nil {
		return png, "image/png"
	}
	jpg, err := jpegBytes(img, quality, maxWidth)
	if err != nil {
		return png, "image/png"
	}
	return jpg, "image/jpeg"
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func firstText(blocks []contentBlock) string {
	for _, b := range blocks {
		if b.Type == "text" {
			return b.Text
		}
	}
	return ""
}

// poll runs check every 250ms until it returns true or the deadline passes.
func poll(ctx context.Context, timeout time.Duration, check func() bool, failMsg string) error {
	deadline := time.Now().Add(timeout)
	for {
		if check() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s", failMsg)
		}
		if !sleepCtx(ctx, 250*time.Millisecond) {
			return ctx.Err()
		}
	}
}

// sleepCtx sleeps for d, returning false if ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// allocDisplay finds an unused X display number.
func allocDisplay() (string, error) {
	for n := 140; n < 400; n++ {
		if !fileExists(fmt.Sprintf("/tmp/.X11-unix/X%d", n)) && !fileExists(fmt.Sprintf("/tmp/.X%d-lock", n)) {
			return ":" + strconv.Itoa(n), nil
		}
	}
	return "", fmt.Errorf("no free X display found")
}
