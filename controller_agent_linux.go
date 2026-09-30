//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	json "encoding/json/v2"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// agentProxy pushes the Windows agent into the RDP session and talks to it over
// the dir transport — files on the redirected drive (\\tsclient\mcp on the
// target, session.workdir here). It bootstraps once, then relays every
// non-desktop tool call as a request/response file pair.
type agentProxy struct {
	session  *rdpSession
	agentExe string
	enabled  map[string]bool

	up   atomic.Bool
	once sync.Once
}

func (p *agentProxy) rpcDir() string { return filepath.Join(p.session.workdir, "rpc") }
func (p *agentProxy) ready() bool    { return p.up.Load() }

// bootstrap copies the agent onto the redirected drive, launches it inside the
// session via the Run dialog, and waits for its readiness marker.
func (p *agentProxy) bootstrap(ctx context.Context) error {
	var bootErr error
	p.once.Do(func() { bootErr = p.doBootstrap(ctx) })
	return bootErr
}

func (p *agentProxy) doBootstrap(ctx context.Context) error {
	if !fileExists(p.agentExe) {
		return fmt.Errorf("agent binary not found at %s (build it with `just build-windows`, or pass --agent-exe)", p.agentExe)
	}
	// The agent lands on the redirected drive, which the session sees as
	// \\tsclient\mcp. Copy it in and pre-create the rpc dir so the request we
	// send right after readiness has somewhere to go.
	dst := filepath.Join(p.session.workdir, "win-rdp-mcp.exe")
	if err := copyFile(p.agentExe, dst); err != nil {
		return fmt.Errorf("staging agent onto the redirected drive: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(p.rpcDir(), "req"), 0o755); err != nil {
		return err
	}

	launch := `\\tsclient\mcp\win-rdp-mcp.exe --transport dir --dir \\tsclient\mcp\rpc` + p.tierFlags()
	logf("bootstrapping agent in session: %s", launch)

	p.session.mu.Lock()
	// Win+R opens the Run dialog; type the command and press Enter. Keystroke
	// injection over RDP is how the agent gets launched with no SMB or WinRM.
	p.session.xdo("key", "super+r")
	p.session.mu.Unlock()
	if !sleepCtx(ctx, 1200*time.Millisecond) {
		return ctx.Err()
	}
	p.session.mu.Lock()
	p.session.xdo("type", "--", launch)
	p.session.mu.Unlock()
	if !sleepCtx(ctx, 400*time.Millisecond) {
		return ctx.Err()
	}
	p.session.mu.Lock()
	p.session.xdo("key", "Return")
	p.session.mu.Unlock()

	// Wait for the agent to write its readiness marker.
	readyFile := filepath.Join(p.rpcDir(), "ready")
	if err := poll(ctx, 45*time.Second, func() bool { return fileExists(readyFile) },
		"agent did not report ready (Run-dialog launch or Defender may have blocked it)"); err != nil {
		return err
	}
	p.up.Store(true)
	logf("agent ready")
	return nil
}

// tierFlags reproduces the controller's tool selection on the agent, so the
// agent enables the same surface. It stays within the Run dialog length limit
// by using the tier toggles rather than an explicit tool list.
func (p *agentProxy) tierFlags() string {
	hasTier3 := slices.ContainsFunc(tier3, func(n string) bool { return p.enabled[n] })
	hasTier2 := slices.ContainsFunc(tier2, func(n string) bool { return p.enabled[n] })
	flags := ""
	if hasTier3 {
		flags += " --enable-tier3"
	}
	if !hasTier2 {
		flags += " --disable-tier2"
	}
	return flags
}

// bootstrapWait bounds how long a tool call blocks waiting for the agent to
// finish coming up. Bootstrap (Run-dialog launch + Go start + drive latency)
// runs ~40s, so the first system call after connect waits rather than failing.
const bootstrapWait = 90 * time.Second

// call relays one tool invocation to the agent and waits for its reply.
func (p *agentProxy) call(ctx context.Context, name string, args map[string]any) (toolResult, error) {
	// The agent is reached through the redirected drive, which exists only
	// while the RDP client runs; this also opens the session (and starts the
	// bootstrap) when a system tool is the first call.
	p.session.mu.Lock()
	err := p.session.ensureConnected(ctx)
	p.session.mu.Unlock()
	if err != nil {
		return toolResult{}, err
	}

	if !p.ready() {
		// Block on bootstrap instead of erroring: the AI's first system-tool
		// call right after connect should just work, not have to be retried.
		waitCtx, cancel := context.WithTimeout(ctx, bootstrapWait)
		defer cancel()
		if err := poll(waitCtx, bootstrapWait, p.ready,
			"the in-session agent did not finish starting"); err != nil {
			return toolResult{}, err
		}
	}

	id, err := newRPCID()
	if err != nil {
		return toolResult{}, err
	}
	req := dirRequest{ID: id, Name: name, Arguments: args}
	data, err := json.Marshal(req)
	if err != nil {
		return toolResult{}, err
	}
	if err := writeAtomic(filepath.Join(p.rpcDir(), "req", id+".json"), data); err != nil {
		return toolResult{}, fmt.Errorf("writing request to the redirected drive: %w", err)
	}

	respPath := filepath.Join(p.rpcDir(), "resp", id+".json")
	callCtx, cancel := context.WithTimeout(ctx, toolCallTimeout)
	defer cancel()
	if err := poll(callCtx, toolCallTimeout, func() bool { return fileExists(respPath) },
		"no response from the agent (it may have exited)"); err != nil {
		return toolResult{}, err
	}

	raw, err := os.ReadFile(respPath)
	os.Remove(respPath)
	if err != nil {
		return toolResult{}, err
	}
	var resp dirResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return toolResult{}, fmt.Errorf("decoding agent response: %w", err)
	}
	return toolResult{Content: resp.Content, IsError: resp.IsError}, nil
}

func newRPCID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	// A time prefix keeps the request directory sortable oldest-first.
	return fmt.Sprintf("%013d-%s", time.Now().UnixMilli(), hex.EncodeToString(b[:])), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// writeAtomic writes to a .tmp sibling then renames, so a reader on the other
// end of the redirected drive never sees a half-written file.
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
