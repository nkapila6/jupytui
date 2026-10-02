// Package session keeps kernels alive between jupytui runs. ":detach"
// hands the kernel to a background keeper (jupytui keep), which follows
// cells that are still running and writes their outputs into the
// notebook. Opening the notebook again asks the keeper to hand over.
package session

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/nkapila6/jupytui/internal/kernel"
	"github.com/nkapila6/jupytui/internal/notebook"
)

// Running is a cell whose execution was in flight at handover.
type Running struct {
	CellID    string `json:"cell"`
	MsgID     string `json:"msg"`
	ExecCount int    `json:"count"`
}

type Session struct {
	Notebook string    `json:"notebook"`
	ConnFile string    `json:"conn_file"`
	PID      int       `json:"pid"` // kernel process group leader
	Keeper   int       `json:"keeper"`
	Socket   string    `json:"socket"`
	Env      string    `json:"env"`
	Started  time.Time `json:"started"`
	Running  []Running `json:"running"`
}

// Dir is where session files live ($XDG_STATE_HOME/jupytui/sessions).
func Dir() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "jupytui", "sessions")
}

func key(notebook string) string {
	sum := sha1.Sum([]byte(notebook))
	return hex.EncodeToString(sum[:6])
}

// Path is the session file for a notebook (absolute path).
func Path(notebook string) string { return filepath.Join(Dir(), key(notebook)+".json") }

// SocketPath is the keeper's control socket for a notebook.
func SocketPath(notebook string) string { return filepath.Join(Dir(), key(notebook)+".sock") }

func Save(s *Session) error {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := Path(s.Notebook) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, Path(s.Notebook))
}

// Load returns the live session for a notebook, cleaning up a dead one.
func Load(notebook string) (*Session, bool) {
	s, err := read(Path(notebook))
	if err != nil {
		return nil, false
	}
	if !alive(s.PID) {
		Remove(notebook)
		return nil, false
	}
	return s, true
}

func read(path string) (*Session, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func Remove(notebook string) {
	os.Remove(Path(notebook))
	os.Remove(SocketPath(notebook))
}

// List returns live sessions, newest first, dropping dead ones.
func List() []*Session {
	files, _ := filepath.Glob(filepath.Join(Dir(), "*.json"))
	var out []*Session
	for _, f := range files {
		s, err := read(f)
		if err != nil {
			continue
		}
		if !alive(s.PID) {
			Remove(s.Notebook)
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out
}

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// TakeOwnership points the kernel's watchdog at pid. Do this before the
// keeper lets go so there's never a moment with nobody owning it.
func TakeOwnership(s *Session, pid int) error {
	return os.WriteFile(filepath.Join(filepath.Dir(s.ConnFile), "owner.pid"), []byte(strconv.Itoa(pid)), 0o600)
}

// call sends one command to the keeper and returns its reply line.
func call(sock, cmd string, timeout time.Duration) (string, error) {
	c, err := net.DialTimeout("unix", sock, timeout)
	if err != nil {
		return "", err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(timeout))
	if _, err := fmt.Fprintln(c, cmd); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return "", err
	}
	return line, nil
}

// Release asks the keeper to write the notebook and hand the kernel
// over. It returns the cells that are still running.
func Release(s *Session) ([]Running, error) {
	if !alive(s.Keeper) {
		// keeper gone (killed?) but the kernel lives: just take it
		return s.Running, nil
	}
	line, err := call(s.Socket, "release", 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("keeper didn't answer: %w", err)
	}
	var running []Running
	if err := json.Unmarshal([]byte(line), &running); err != nil {
		return nil, err
	}
	return running, nil
}

// Kill stops a session's kernel and keeper.
func Kill(s *Session) {
	if alive(s.Keeper) {
		call(s.Socket, "kill", 10*time.Second)
	}
	if alive(s.PID) {
		syscall.Kill(-s.PID, syscall.SIGKILL)
	}
	os.RemoveAll(filepath.Dir(s.ConnFile))
	Remove(s.Notebook)
}

// WaitReady waits for a freshly started keeper to answer.
func WaitReady(s *Session, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if line, err := call(s.Socket, "ping", time.Second); err == nil && line == "ok\n" {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("keeper didn't start")
}

// RunKeeper is `jupytui keep <session file>`: hold the kernel, follow the
// running cells into the notebook, and hand over when asked.
func RunKeeper(path string) error {
	s, err := read(path)
	if err != nil {
		return err
	}
	k, err := kernel.Attach(context.Background(), s.ConnFile, s.PID)
	if err != nil {
		Remove(s.Notebook)
		return err
	}
	if err := k.SetOwner(os.Getpid()); err != nil {
		return err
	}
	nb, err := notebook.Load(s.Notebook)
	if err != nil {
		return err
	}

	os.Remove(s.Socket)
	ln, err := net.Listen("unix", s.Socket)
	if err != nil {
		return err
	}
	defer ln.Close()
	s.Keeper = os.Getpid()
	if err := Save(s); err != nil {
		return err
	}

	var mu sync.Mutex
	running := map[string]Running{}
	for _, r := range s.Running {
		c := nb.CellByID(r.CellID)
		if c == nil {
			continue
		}
		running[r.MsgID] = r
		go func(r Running, c *notebook.Cell) {
			for ev := range k.Watch(r.MsgID, r.ExecCount) {
				mu.Lock()
				switch ev.Kind {
				case kernel.EvStarted:
					n := ev.ExecCount
					c.ExecutionCount = &n
				case kernel.EvOutput:
					c.AddOutput(ev.Output)
				case kernel.EvClear:
					c.Outputs = nil
				case kernel.EvDone:
					if ev.ExecCount > 0 {
						n := ev.ExecCount
						c.ExecutionCount = &n
					}
					delete(running, r.MsgID)
					nb.Save(s.Notebook)
				}
				mu.Unlock()
			}
		}(r, c)
	}

	quit := make(chan struct{})
	var once sync.Once
	done := func() { once.Do(func() { close(quit) }) }
	go func() {
		<-k.Dead()
		Remove(s.Notebook)
		done()
	}()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadString('\n')
			switch line {
			case "ping\n":
				fmt.Fprintln(conn, "ok")
			case "release\n":
				mu.Lock()
				nb.Save(s.Notebook)
				left := make([]Running, 0, len(running))
				for _, r := range running {
					if c := nb.CellByID(r.CellID); c != nil && c.ExecutionCount != nil {
						r.ExecCount = *c.ExecutionCount
					}
					left = append(left, r)
				}
				b, _ := json.Marshal(left)
				conn.Write(append(b, '\n'))
				conn.Close()
				k.Release()
				Remove(s.Notebook)
				mu.Unlock()
				done()
				return
			case "kill\n":
				mu.Lock()
				nb.Save(s.Notebook)
				mu.Unlock()
				fmt.Fprintln(conn, "ok")
				conn.Close()
				k.Shutdown()
				Remove(s.Notebook)
				done()
				return
			}
			conn.Close()
		}
	}()
	<-quit
	return nil
}
