//go:build linux

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestStartRDPClientKeepsArgumentsOffArgv(t *testing.T) {
	// A stand-in client that exits 0 only if its arguments arrive on stdin.
	fake := filepath.Join(t.TempDir(), "fake-rdp")
	writeScript(t, fake, `read -r a; read -r b; [ "$a" = "/v:host" ] && [ "$b" = "/p:s3cret" ] || exit 9`)

	c, err := startRDPClient(fake, []string{"/v:host", "/p:s3cret"}, os.Environ(), nil)
	if err != nil {
		t.Fatal(err)
	}
	waitExit(t, c)
	if want := []string{fake, "/args-from:stdin"}; !slices.Equal(c.cmd.Args, want) {
		t.Fatalf("argv = %q, want %q (the password must not be on the command line)", c.cmd.Args, want)
	}
	if c.err != nil {
		t.Fatalf("client did not read its arguments from stdin: %v", c.err)
	}
}

func TestRDPClientExitReasonCarriesFreeRDPError(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-rdp")
	writeScript(t, fake, `cat >/dev/null
echo '[16:50:34:849] [24:1d] [ERROR][com.winpr.sspi.Kerberos] - [kerberos_AcquireCredentialsHandleA]: krb5_parse_name' >&2
echo '[16:50:34:947] [24:1d] [WARN][com.freerdp.core.connection] - [rdp_client_connect_auto_detect]: noise' >&2
echo '[16:50:43:957] [24:1d] [ERROR][com.freerdp.core] - [rdp_set_error_info]: ERRINFO_RPC_INITIATED_DISCONNECT [0x00010001]' >&2
exit 1`)

	c, err := startRDPClient(fake, []string{"/v:host"}, os.Environ(), nil)
	if err != nil {
		t.Fatal(err)
	}
	waitExit(t, c)
	if c.running() {
		t.Fatal("running() = true after exit")
	}
	got := c.exitReason()
	want := "exit status 1 (disconnected by the server): [rdp_set_error_info]: ERRINFO_RPC_INITIATED_DISCONNECT [0x00010001]"
	if got != want {
		t.Fatalf("exitReason() = %q, want %q", got, want)
	}
}

func TestRDPClientExitReasonForAWrongPassword(t *testing.T) {
	// What xfreerdp 3.15 prints for a bad password: the cause, then generic
	// failures as the connection unwinds.
	fake := filepath.Join(t.TempDir(), "fake-rdp")
	writeScript(t, fake, `cat >/dev/null
echo '[17:01:06:523] [13:12] [ERROR][com.freerdp.core] - [nla_recv_pdu]: ERRCONNECT_LOGON_FAILURE [0x00020014]' >&2
echo '[17:01:06:523] [13:12] [ERROR][com.freerdp.core.rdp] - [rdp_recv_callback_int][0xaaaad3429b20]: CONNECTION_STATE_NLA - nla_recv_pdu() fail' >&2
echo '[17:01:06:523] [13:12] [ERROR][com.freerdp.core.transport] - [transport_check_fds]: transport_check_fds: transport->ReceiveCallback() - STATE_RUN_FAILED [-1]' >&2
exit 134`)

	c, err := startRDPClient(fake, nil, os.Environ(), nil)
	if err != nil {
		t.Fatal(err)
	}
	waitExit(t, c)
	want := "exit status 134 (logon failed: wrong user name or password): [nla_recv_pdu]: ERRCONNECT_LOGON_FAILURE [0x00020014]"
	if got := c.exitReason(); got != want {
		t.Fatalf("exitReason() = %q, want %q", got, want)
	}
}

func TestRDPClientKillReapsARunningClient(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "fake-rdp")
	writeScript(t, fake, "exec sleep 30")
	c, err := startRDPClient(fake, nil, os.Environ(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !c.running() {
		t.Fatal("running() = false for a live client")
	}
	start := time.Now()
	c.kill()
	if c.running() || time.Since(start) > 5*time.Second {
		t.Fatalf("kill did not stop and reap the client (running=%v, took %s)", c.running(), time.Since(start))
	}
}

func TestLogTail(t *testing.T) {
	lt := newLogTail(3)
	// Writes need not align with lines.
	for _, chunk := range []string{"one\ntw", "o\r\nthree\n", "[x] [y] [ERROR][tag] - [fn]: boom\nfour\npartial"} {
		if n, err := lt.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
		}
	}
	if want := []string{"three", "[x] [y] [ERROR][tag] - [fn]: boom", "four"}; !slices.Equal(lt.lines, want) {
		t.Fatalf("lines = %q, want %q", lt.lines, want)
	}
	if got := lt.lastError(); got != "[fn]: boom" {
		t.Fatalf("lastError() = %q", got)
	}
	if got := newLogTail(3).lastError(); got != "" {
		t.Fatalf("lastError() on an empty tail = %q", got)
	}
}

func TestFindControllerDeps(t *testing.T) {
	tools := []string{"Xvfb", "xdpyinfo", "xdotool", "import"}
	setup := func(t *testing.T, names ...string) string {
		dir := t.TempDir()
		for _, n := range names {
			writeScript(t, filepath.Join(dir, n), "exit 0")
		}
		t.Setenv("PATH", dir)
		return dir
	}

	t.Run("prefers xfreerdp3", func(t *testing.T) {
		dir := setup(t, append(tools, "xfreerdp", "xfreerdp3")...)
		got, err := findControllerDeps()
		if err != nil || got != filepath.Join(dir, "xfreerdp3") {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("falls back to xfreerdp", func(t *testing.T) {
		dir := setup(t, append(tools, "xfreerdp")...)
		got, err := findControllerDeps()
		if err != nil || got != filepath.Join(dir, "xfreerdp") {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("names everything missing", func(t *testing.T) {
		setup(t, "xdotool")
		_, err := findControllerDeps()
		if err == nil {
			t.Fatal("no error with most dependencies missing")
		}
		for _, want := range []string{"xfreerdp3", "Xvfb", "xdpyinfo", "import", "x11-utils"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %s", err, want)
			}
		}
		if strings.Contains(err.Error(), "xdotool,") {
			t.Errorf("error %q lists xdotool, which is present", err)
		}
	})
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func waitExit(t *testing.T, c *rdpClient) {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(10 * time.Second):
		c.kill()
		t.Fatal("client did not exit")
	}
}

func TestXdotoolCombo(t *testing.T) {
	cases := map[string]string{
		"ctrl+c":         "ctrl+c",
		"ctrl+shift+esc": "ctrl+shift+Escape",
		"win+e":          "super+e",
		"alt+F4":         "alt+F4",
		"alt+f4":         "alt+F4",
		"F12":            "F12",
		"f24":            "F24",
		"f25":            "f25", // not a function key: passed through, xdotool rejects it
		"f04":            "f04",
		"CTRL+A":         "ctrl+a",
		" ctrl + c ":     "ctrl+c",
		"capslock":       "Caps_Lock",
		"KP_Enter":       "KP_Enter",
		"volumeup":       "XF86AudioRaiseVolume",
		"ctrl++":         "ctrl",
		"":               "",
	}
	for in, want := range cases {
		if got := xdotoolCombo(in); got != want {
			t.Errorf("xdotoolCombo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestXdotoolKeyNamesCoverTheAgentsNames(t *testing.T) {
	// Every key name the agent accepts (namedKeys in platform_windows.go) must
	// mean something here too. Kept as a literal because that file only builds
	// on Windows.
	agent := []string{
		"ctrl", "control", "alt", "shift", "win", "super", "cmd", "meta",
		"tab", "enter", "return", "esc", "escape", "space", "backspace", "delete", "del",
		"insert", "home", "end", "pageup", "pagedown", "up", "down", "left", "right",
		"capslock", "printscreen", "numlock", "volumemute", "volumedown", "volumeup",
	}
	for _, k := range agent {
		if xdotoolKeyNames[k] == "" {
			t.Errorf("agent key %q has no xdotool mapping", k)
		}
	}
}

func TestTypingSteps(t *testing.T) {
	type s = typingStep
	cases := []struct {
		in   string
		want []typingStep
	}{
		{"", nil},
		{"hello", []s{{text: "hello"}}},
		{"a\nb", []s{{text: "a"}, {key: "Return"}, {text: "b"}}},
		{"a\r\nb", []s{{text: "a"}, {key: "Return"}, {text: "b"}}},
		{"a\rb", []s{{text: "a"}, {key: "Return"}, {text: "b"}}},
		{"x\ty", []s{{text: "x"}, {key: "Tab"}, {text: "y"}}},
		{"\n\n", []s{{key: "Return"}, {key: "Return"}}},
		{"end\n", []s{{text: "end"}, {key: "Return"}}},
		{"héllo ✓ @#$%^&*()", []s{{text: "héllo ✓ @#$%^&*()"}}},
	}
	for _, c := range cases {
		if got := typingSteps(c.in); !slices.Equal(got, c.want) {
			t.Errorf("typingSteps(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestGlideArgs(t *testing.T) {
	got := glideArgs(0, 0, 30, 60, 3)
	want := []string{
		"mousemove", "10", "20",
		"sleep", "0.01", "mousemove", "20", "40",
		"sleep", "0.01", "mousemove", "30", "60",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("glideArgs = %q, want %q", got, want)
	}
	// Moving up-left, with steps that do not divide the distance, still ends
	// exactly on the target.
	got = glideArgs(100, 100, 7, 3, 7)
	if tail := got[len(got)-3:]; !slices.Equal(tail, []string{"mousemove", "7", "3"}) {
		t.Fatalf("last step = %q, want the target", tail)
	}
	if moves := strings.Count(strings.Join(got, " "), "mousemove"); moves != 7 {
		t.Fatalf("%d mousemoves, want 7", moves)
	}
}

func TestParseMouseLocation(t *testing.T) {
	x, y, err := parseMouseLocation("X=640\nY=400\nSCREEN=0\nWINDOW=543\n")
	if err != nil || x != 640 || y != 400 {
		t.Fatalf("got %d,%d,%v", x, y, err)
	}
	if _, _, err := parseMouseLocation("garbage"); err == nil {
		t.Fatal("no error for output without X/Y")
	}
}
