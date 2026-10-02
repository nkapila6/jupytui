package ui

import (
	"os"
	"os/exec"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/nkapila6/jupytui/internal/envs"
	"github.com/nkapila6/jupytui/internal/kernel"
	"github.com/nkapila6/jupytui/internal/session"
)

// :detach hands the kernel to a background keeper and quits; opening the
// notebook again picks it back up (see session.RunKeeper).

type detachedMsg struct{ err error }

// Attach tells New to reuse a kernel from a detached session.
type Attach struct {
	Session *session.Session
	Running []session.Running
}

func (m *Model) detach() tea.Cmd {
	k := m.k
	if k == nil {
		m.msg = "no kernel to keep"
		return nil
	}
	if m.remote != nil {
		m.msg = "detach isn't supported for remote kernels (" + m.remote.host + ") yet"
		return nil
	}
	if m.mode == editMode {
		m.stopEdit()
	}
	// the keeper finds running cells by id, and works on the saved file
	var running []session.Running
	for c := range m.runs {
		id, ok := m.msgIDs[c]
		if !ok {
			continue
		}
		n := 0
		if c.ExecutionCount != nil {
			n = *c.ExecutionCount
		}
		running = append(running, session.Running{CellID: c.EnsureID(), MsgID: id, ExecCount: n})
	}
	m.save()
	if m.dirty {
		return nil // save failed, message already set
	}
	s := &session.Session{
		Notebook: m.path,
		ConnFile: k.ConnFile(),
		PID:      k.PID(),
		Socket:   session.SocketPath(m.path),
		Env:      m.env.Label(),
		Started:  time.Now(),
		Running:  running,
	}
	m.msg = "detaching…"
	return func() tea.Msg {
		if err := session.Save(s); err != nil {
			return detachedMsg{err}
		}
		self, err := os.Executable()
		if err != nil {
			return detachedMsg{err}
		}
		cmd := exec.Command(self, "keep", session.Path(m.path))
		// its own session: no terminal, survives the terminal closing
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return detachedMsg{err}
		}
		go cmd.Wait()
		return detachedMsg{session.WaitReady(s, 10*time.Second)}
	}
}

func (m *Model) handleDetached(msg detachedMsg) tea.Cmd {
	if msg.err != nil {
		session.Remove(m.path)
		m.msg = "detach failed: " + msg.err.Error()
		return nil
	}
	// the keeper owns the kernel now: let go without stopping it
	m.host.forget(m.k)
	m.k.Release()
	m.k = nil
	m.detached = true
	return tea.Quit
}

// adopt follows cells that were still running when the session was
// handed back to us.
func (m *Model) adopt(k *kernel.Kernel) tea.Cmd {
	var cmds []tea.Cmd
	for _, r := range m.attach.Running {
		c := m.nb.CellByID(r.CellID)
		if c == nil {
			continue
		}
		m.runs[c] = running
		m.msgIDs[c] = r.MsgID
		m.execSrc[c] = c.Source
		cmds = append(cmds, waitEvent(k, c, k.Watch(r.MsgID, r.ExecCount)))
	}
	if len(cmds) > 0 {
		cmds = append(cmds, m.startTick())
	}
	m.msg = "reattached to the running kernel"
	m.attach = nil
	return tea.Batch(cmds...)
}

// Detached reports whether the program ended with :detach.
func (m *Model) Detached() bool { return m.detached }

// remoteInfo is set when the kernel runs on another machine.
type remoteInfo struct {
	host string
}

func remoteInfoFor(e envs.Env) *remoteInfo {
	if e.Kind != envs.Remote {
		return nil
	}
	return &remoteInfo{host: e.Host}
}
