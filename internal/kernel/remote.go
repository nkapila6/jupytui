package kernel

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Remote kernels: ipykernel runs on another machine over ssh, and a
// second ssh forwards its five ports to localhost, so the rest of
// jupytui can't tell the difference.

// Remote says where to run the kernel.
type Remote struct {
	Host string // anything ssh accepts, ~/.ssh/config aliases included
	Dir  string // working dir on the remote; falls back to ~
}

// remoteBootstrap runs on the remote. ipykernel picks its own ports and
// writes the connection file; we print it back over ssh's stdout (on a
// dup of fd 1, since ipykernel later captures fd 1 for cell output).
// When ssh's stdin closes (connection gone, jupytui killed) it exits, so
// nothing is left running on the remote.
const remoteBootstrap = `import json, os, sys, threading, time
conn = sys.argv[sys.argv.index("-f") + 1]
out = os.fdopen(os.dup(1), "w")
def announce():
    for _ in range(1200):
        try:
            with open(conn) as f:
                info = json.load(f)
            if info.get("shell_port"):
                out.write("JUPYTUI_CONN " + json.dumps(info) + "\n")
                out.flush()
                return
        except Exception:
            pass
        time.sleep(0.1)
def watch():
    try:
        while os.read(0, 1024):
            pass
    finally:
        try:
            os.remove(conn)
        except OSError:
            pass
        os._exit(0)
threading.Thread(target=announce, daemon=True).start()
threading.Thread(target=watch, daemon=True).start()
from ipykernel import kernelapp
kernelapp.launch_new_instance()
`

func sshBin() string {
	if s := os.Getenv("JUPYTUI_SSH"); s != "" {
		return s
	}
	return "ssh"
}

// shq quotes s for a POSIX shell.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// remoteDir quotes a remote path, keeping a leading ~ expandable.
func remoteDir(d string) string {
	if d == "" || d == "~" {
		return `"$HOME"`
	}
	if rest, ok := strings.CutPrefix(d, "~/"); ok {
		return `"$HOME"/` + shq(rest)
	}
	return shq(d)
}

func startRemote(opts Options) (*Kernel, error) {
	r := opts.Remote
	dir, err := os.MkdirTemp("", "jupytui-")
	if err != nil {
		return nil, err
	}
	logf, err := os.Create(filepath.Join(dir, "kernel.log"))
	if err != nil {
		return nil, err
	}
	defer logf.Close()

	id := newUUID()[:8]
	extra := ""
	for _, p := range opts.With {
		extra += " --with " + shq(p)
	}
	script := fmt.Sprintf(`mkdir -p "$HOME/.cache/jupytui"; cd %s 2>/dev/null || cd; exec uv run --with ipykernel%s python -c "$(echo %s | base64 -d)" -f "$HOME/.cache/jupytui/kernel-%s.json"`,
		remoteDir(r.Dir), extra, base64.StdEncoding.EncodeToString([]byte(remoteBootstrap)), id)
	common := []string{"-o", "BatchMode=yes", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3"}

	cmd := exec.Command(sshBin(), append(append([]string{"-T"}, common...), r.Host, "sh -lc "+shq(script))...)
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe() // kept open: closing it stops the remote kernel
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("ssh: %w", err)
	}
	fail := func(err error) (*Kernel, error) {
		stdin.Close()
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cmd.Wait()
		logs, _ := os.ReadFile(filepath.Join(dir, "kernel.log"))
		os.RemoveAll(dir)
		if s := strings.TrimSpace(string(logs)); s != "" {
			return nil, fmt.Errorf("%w:\n%s", err, s)
		}
		return nil, err
	}

	// wait for the remote to tell us its ports
	type result struct {
		info ConnInfo
		err  error
	}
	got := make(chan result, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if line, ok := strings.CutPrefix(sc.Text(), "JUPYTUI_CONN "); ok {
				var info ConnInfo
				got <- result{info, json.Unmarshal([]byte(line), &info)}
				// keep draining so the remote never blocks on a full pipe
				for sc.Scan() {
				}
				return
			}
			fmt.Fprintln(logf, sc.Text())
		}
		got <- result{err: errors.New("remote kernel exited before starting")}
	}()
	var remote ConnInfo
	select {
	case res := <-got:
		if res.err != nil {
			return fail(res.err)
		}
		remote = res.info
	case <-opts.Context.Done():
		return fail(opts.Context.Err())
	case <-time.After(opts.StartTimeout):
		return fail(errors.New("timed out waiting for the remote kernel"))
	}

	// forward the five ports
	local, err := freePorts(5)
	if err != nil {
		return fail(err)
	}
	remotePorts := []int{remote.ShellPort, remote.IOPubPort, remote.StdinPort, remote.ControlPort, remote.HBPort}
	targs := append([]string{"-T", "-o", "ExitOnForwardFailure=yes"}, common...)
	for i, p := range remotePorts {
		targs = append(targs, "-L", fmt.Sprintf("%d:127.0.0.1:%d", local[i], p))
	}
	// not -N: a remote cat on our stdin means the tunnel goes away when we
	// do, even if we're SIGKILLed
	tunnel := exec.Command(sshBin(), append(targs, r.Host, "cat >/dev/null")...)
	tunnel.Stderr = logf
	tunnel.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	tunnelIn, err := tunnel.StdinPipe()
	if err != nil {
		return fail(err)
	}
	if err := tunnel.Start(); err != nil {
		return fail(fmt.Errorf("ssh tunnel: %w", err))
	}

	conn := remote
	conn.IP = "127.0.0.1"
	conn.ShellPort, conn.IOPubPort, conn.StdinPort, conn.ControlPort, conn.HBPort = local[0], local[1], local[2], local[3], local[4]
	b, _ := json.MarshalIndent(conn, "", "  ")
	connFile := filepath.Join(dir, "kernel.json")
	if err := os.WriteFile(connFile, b, 0o600); err != nil {
		return fail(err)
	}

	k := newKernel(conn, connFile)
	k.cmd, k.pid, k.remote = cmd, cmd.Process.Pid, true
	k.extra = []*exec.Cmd{tunnel}
	k.remoteStdin = stdin
	k.tunnelStdin = tunnelIn
	go func() {
		k.waitErr = cmd.Wait()
		k.stop()
	}()
	go func() {
		// a dead tunnel is as good as a dead kernel
		tunnel.Wait()
		k.stop()
	}()
	if err := k.connect(opts.Context, opts.StartTimeout); err != nil {
		k.Shutdown()
		return nil, err
	}
	return k, nil
}
