// Package kernel launches a Jupyter kernel and talks to it over the
// Jupyter wire protocol (ZMQ), without a Jupyter server.
package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-zeromq/zmq4"
	"github.com/nkapila6/jupytui/internal/notebook"
)

// DefaultCmd runs ipykernel in the project env via uv. --with layers
// ipykernel on top so the project doesn't need it as a dependency.
var DefaultCmd = []string{"uv", "run", "--with", "ipykernel", "python", "-c", bootstrap, "-f", "{connection_file}"}

// CmdForPython runs the kernel on a specific interpreter (a venv, conda
// env or bare python), with ipykernel overlaid by uv so the env itself
// doesn't need it installed.
func CmdForPython(python string) []string {
	return []string{"uv", "run", "--no-project", "--python", python, "--with", "ipykernel", "python", "-c", bootstrap, "-f", "{connection_file}"}
}

// bootstrap starts ipykernel with a watchdog on its owner. ipykernel's
// own parent poller only watches its direct parent, which is uv, and uv
// happily outlives us if we get SIGKILLed or the terminal goes away.
// The owner's pid lives in a file next to the connection file so a
// detached session can hand the kernel to a keeper process and back.
const bootstrap = `import os, shutil, sys, threading, time
def _watch():
    conn_dir = os.path.dirname(sys.argv[sys.argv.index("-f") + 1])
    pidfile = os.environ.get("JUPYTUI_PIDFILE", "")
    while True:
        time.sleep(1)
        try:
            pid = int(open(pidfile).read().strip())
        except Exception:
            pid = int(os.environ.get("JUPYTUI_PID", "0"))
        if not pid:
            continue
        try:
            os.kill(pid, 0)
        except ProcessLookupError:
            shutil.rmtree(conn_dir, ignore_errors=True)
            os._exit(1)
threading.Thread(target=_watch, daemon=True).start()
from ipykernel import kernelapp
kernelapp.launch_new_instance()
`

type Options struct {
	Context      context.Context // cancel to abort startup
	Dir          string          // working dir, uv picks the project env from here
	Cmd          []string        // {connection_file} gets substituted
	StartTimeout time.Duration   // first uv run may have to download ipykernel
	Remote       *Remote         // run it on another machine over ssh
}

type Kernel struct {
	conn     ConnInfo
	connFile string
	key      []byte
	session  string
	cmd      *exec.Cmd // nil when we attached to someone else's kernel
	pid      int       // process group leader (uv, or ssh for remote)
	released bool      // handed to another owner: don't delete its files

	remote      bool
	extra       []*exec.Cmd    // ssh tunnel for remote kernels
	remoteStdin io.WriteCloser // closing it stops the remote kernel
	tunnelStdin io.WriteCloser // and this the tunnel

	ctx     context.Context
	cancel  context.CancelFunc
	shell   *sock
	control *sock
	stdin   *sock
	iopub   zmq4.Socket

	mu       sync.Mutex
	replies  map[string]chan *Message // shell/control replies by parent msg_id
	watchers map[string]chan *Message // iopub messages by parent msg_id
	inputs   map[string]chan *Message // input_requests by parent msg_id

	status   chan string
	dead     chan struct{}
	waitErr  error
	stopOnce sync.Once
}

// sock serializes sends since zmq sockets aren't safe for concurrent use.
type sock struct {
	mu sync.Mutex
	s  zmq4.Socket
}

func (s *sock) send(m zmq4.Msg) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.s.SendMulti(m)
}

func Start(opts Options) (*Kernel, error) {
	if len(opts.Cmd) == 0 {
		opts.Cmd = DefaultCmd
	}
	if opts.Context == nil {
		opts.Context = context.Background()
	}
	if opts.StartTimeout == 0 {
		opts.StartTimeout = 2 * time.Minute
	}
	if opts.Remote != nil {
		return startRemote(opts)
	}
	conn, err := newConnInfo()
	if err != nil {
		return nil, err
	}
	connFile, err := writeConnFile(conn)
	if err != nil {
		return nil, err
	}

	args := make([]string, len(opts.Cmd))
	for i, a := range opts.Cmd {
		args[i] = strings.ReplaceAll(a, "{connection_file}", connFile)
	}
	dir := filepath.Dir(connFile)
	if err := writeOwner(dir, os.Getpid()); err != nil {
		return nil, err
	}
	// a file, not a pipe: the kernel may outlive us (detach), and writing
	// to a pipe nobody reads anymore would break it
	logf, err := os.Create(filepath.Join(dir, "kernel.log"))
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = opts.Dir
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("JUPYTUI_PID=%d", os.Getpid()),
		"JUPYTUI_PIDFILE="+filepath.Join(dir, "owner.pid"))
	cmd.Stdout = logf
	cmd.Stderr = logf
	// own process group: keeps terminal signals away from the kernel and
	// lets us kill uv and python together
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		os.RemoveAll(filepath.Dir(connFile))
		return nil, fmt.Errorf("start kernel: %w", err)
	}

	k := newKernel(conn, connFile)
	k.cmd, k.pid = cmd, cmd.Process.Pid
	go func() {
		k.waitErr = cmd.Wait()
		k.stop()
	}()

	if err := k.connect(opts.Context, opts.StartTimeout); err != nil {
		k.Shutdown()
		return nil, err
	}
	return k, nil
}

func newKernel(conn ConnInfo, connFile string) *Kernel {
	ctx, cancel := context.WithCancel(context.Background())
	return &Kernel{
		conn:     conn,
		connFile: connFile,
		key:      []byte(conn.Key),
		session:  newUUID(),
		ctx:      ctx,
		cancel:   cancel,
		replies:  map[string]chan *Message{},
		watchers: map[string]chan *Message{},
		inputs:   map[string]chan *Message{},
		status:   make(chan string, 32),
		dead:     make(chan struct{}),
	}
}

// Remote reports whether the kernel runs on another machine.
func (k *Kernel) Remote() bool { return k.remote }

func (k *Kernel) connect(ctx context.Context, timeout time.Duration) error {
	return k.connectWith(ctx, timeout, false)
}

// connectWith dials the kernel. attaching=true is for a kernel that may
// be busy running someone else's cell: shell requests queue behind that
// cell, so the handshake goes over control (answered right away) and the
// iopub subscription just gets a moment to settle.
func (k *Kernel) connectWith(ctx context.Context, timeout time.Duration, attaching bool) error {
	deadline := time.Now().Add(timeout)
	// wait until the kernel has bound its ports
	for {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", k.conn.IP, k.conn.ShellPort), 200*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		select {
		case <-k.dead:
			return k.deathError("kernel exited during startup")
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return k.deathError("timed out waiting for kernel")
		}
	}

	k.control = &sock{s: zmq4.NewDealer(k.ctx)}
	// stdin has to share the shell socket's identity: the kernel sends
	// input_request to whoever sent the execute_request
	id := zmq4.SocketIdentity(k.session)
	k.shell = &sock{s: zmq4.NewDealer(k.ctx, zmq4.WithID(id))}
	k.stdin = &sock{s: zmq4.NewDealer(k.ctx, zmq4.WithID(id))}
	k.iopub = zmq4.NewSub(k.ctx)
	for _, d := range []struct {
		s    zmq4.Socket
		port int
	}{{k.shell.s, k.conn.ShellPort}, {k.control.s, k.conn.ControlPort}, {k.stdin.s, k.conn.StdinPort}, {k.iopub, k.conn.IOPubPort}} {
		if err := d.s.Dial(k.conn.addr(d.port)); err != nil {
			return fmt.Errorf("dial kernel: %w", err)
		}
	}
	if err := k.iopub.SetOption(zmq4.OptionSubscribe, ""); err != nil {
		return err
	}
	go k.replyLoop(k.shell.s)
	go k.replyLoop(k.control.s)
	go k.stdinLoop()
	go k.iopubLoop()

	if attaching {
		m, err := k.newMessage("kernel_info_request", struct{}{})
		if err != nil {
			return err
		}
		if _, err := k.request(k.control, m, time.Until(deadline)); err != nil {
			return k.deathError("kernel didn't answer")
		}
		// zmq's slow-joiner: give the SUB subscription time to reach the kernel
		time.Sleep(300 * time.Millisecond)
		return nil
	}

	// SUB sockets drop anything published before the subscription is live,
	// so keep pinging until a kernel_info shows up on iopub too
	for time.Now().Before(deadline) {
		m, err := k.newMessage("kernel_info_request", struct{}{})
		if err != nil {
			return err
		}
		watch := k.watch(m.Header.MsgID)
		reply, err := k.request(k.shell, m, time.Second)
		if err == nil {
			select {
			case <-watch:
				k.unwatch(m.Header.MsgID)
				_ = reply
				return nil
			case <-time.After(500 * time.Millisecond):
			}
		}
		k.unwatch(m.Header.MsgID)
		select {
		case <-k.dead:
			return k.deathError("kernel exited during startup")
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	return k.deathError("kernel never answered kernel_info")
}

func (k *Kernel) replyLoop(s zmq4.Socket) {
	for {
		raw, err := s.Recv()
		if err != nil {
			return
		}
		m, err := decode(k.key, raw)
		if err != nil {
			continue
		}
		k.mu.Lock()
		ch := k.replies[m.ParentHeader.MsgID]
		delete(k.replies, m.ParentHeader.MsgID)
		k.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
}

// stdinLoop routes input_requests to the execution that asked.
func (k *Kernel) stdinLoop() {
	for {
		raw, err := k.stdin.s.Recv()
		if err != nil {
			return
		}
		m, err := decode(k.key, raw)
		if err != nil || m.Header.MsgType != "input_request" {
			continue
		}
		k.mu.Lock()
		ch := k.inputs[m.ParentHeader.MsgID]
		k.mu.Unlock()
		if ch != nil {
			select {
			case ch <- m:
			case <-k.dead:
				return
			}
		}
	}
}

// InputRequest is input() / getpass() asking for a line.
type InputRequest struct {
	Prompt   string
	Password bool
	msg      *Message
}

// Reply answers an input request.
func (k *Kernel) Reply(req *InputRequest, value string) error {
	m, err := k.newMessage("input_reply", map[string]string{"value": value})
	if err != nil {
		return err
	}
	m.ParentHeader = req.msg.Header
	m.Identities = req.msg.Identities
	frames, err := m.encode(k.key)
	if err != nil {
		return err
	}
	return k.stdin.send(frames)
}

func (k *Kernel) iopubLoop() {
	for {
		raw, err := k.iopub.Recv()
		if err != nil {
			return
		}
		m, err := decode(k.key, raw)
		if err != nil {
			continue
		}
		if m.Header.MsgType == "status" {
			var c struct {
				State string `json:"execution_state"`
			}
			json.Unmarshal(m.Content, &c)
			select {
			case k.status <- c.State:
			default:
			}
		}
		k.mu.Lock()
		ch := k.watchers[m.ParentHeader.MsgID]
		k.mu.Unlock()
		if ch != nil {
			select {
			case ch <- m:
			case <-k.dead:
				return
			}
		}
	}
}

func (k *Kernel) watch(id string) chan *Message {
	ch := make(chan *Message, 256)
	k.mu.Lock()
	k.watchers[id] = ch
	k.mu.Unlock()
	return ch
}

func (k *Kernel) unwatch(id string) {
	k.mu.Lock()
	delete(k.watchers, id)
	k.mu.Unlock()
}

// send registers for the reply before sending so a fast reply can't be missed.
func (k *Kernel) send(s *sock, m *Message) (chan *Message, error) {
	ch := make(chan *Message, 1)
	k.mu.Lock()
	k.replies[m.Header.MsgID] = ch
	k.mu.Unlock()
	frames, err := m.encode(k.key)
	if err == nil {
		err = s.send(frames)
	}
	if err != nil {
		k.mu.Lock()
		delete(k.replies, m.Header.MsgID)
		k.mu.Unlock()
		return nil, err
	}
	return ch, nil
}

func (k *Kernel) request(s *sock, m *Message, timeout time.Duration) (*Message, error) {
	ch, err := k.send(s, m)
	if err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		return r, nil
	case <-k.dead:
		return nil, errors.New("kernel died")
	case <-time.After(timeout):
		k.mu.Lock()
		delete(k.replies, m.Header.MsgID)
		k.mu.Unlock()
		return nil, fmt.Errorf("%s timed out", m.Header.MsgType)
	}
}

type EventKind int

const (
	EvStarted EventKind = iota // kernel picked up the cell, ExecCount set
	EvOutput                   // Output set
	EvClear                    // clear_output
	EvDone                     // ExecCount, Status (ok/error/aborted) or Err set
	EvInput                    // Input set: the cell is waiting for a line
)

type Event struct {
	Kind      EventKind
	Input     *InputRequest
	Output    *notebook.Output
	ExecCount int
	Status    string
	Err       error
}

// Execute queues code on the kernel. The channel closes after EvDone,
// which only fires once the reply is in and iopub went idle, so no
// trailing outputs get lost.
func (k *Kernel) Execute(code string) (<-chan Event, error) {
	_, ch, err := k.execute(code, true)
	return ch, err
}

// ExecuteID is Execute plus the request's msg_id, which a detached
// session needs to keep following the run.
func (k *Kernel) ExecuteID(code string) (string, <-chan Event, error) {
	return k.execute(code, true)
}

// Eval runs helper code without touching history or the execution
// count, and returns what it printed. Used for introspection (variable
// explorer, dataframe pages); the code should print one JSON value.
func (k *Kernel) Eval(code string, timeout time.Duration) (string, error) {
	_, events, err := k.execute(code, false)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return out.String(), nil
			}
			switch ev.Kind {
			case EvOutput:
				switch ev.Output.OutputType {
				case "stream":
					if ev.Output.Name == "stdout" {
						out.WriteString(ev.Output.Text)
					}
				case "error":
					return "", fmt.Errorf("%s: %s", ev.Output.Ename, ev.Output.Evalue)
				}
			case EvDone:
				if ev.Err != nil {
					return "", ev.Err
				}
			}
		case <-deadline:
			return "", errors.New("kernel took too long")
		}
	}
}

// execute sends an execute_request. history=false is for our own
// helper code: no history entry and no bump of the [n] counter. (Not
// silent=true, which would also swallow the stdout we read the answer from.)
func (k *Kernel) execute(code string, history bool) (string, <-chan Event, error) {
	m, err := k.newMessage("execute_request", map[string]any{
		"code":             code,
		"silent":           false,
		"store_history":    history,
		"user_expressions": map[string]any{},
		// our own helper code must never block on input()
		"allow_stdin":   history,
		"stop_on_error": history,
	})
	if err != nil {
		return "", nil, err
	}
	id := m.Header.MsgID
	iopub := k.watch(id)
	inputs := make(chan *Message, 4)
	k.mu.Lock()
	k.inputs[id] = inputs
	k.mu.Unlock()
	reply, err := k.send(k.shell, m)
	if err != nil {
		k.unwatch(id)
		return "", nil, err
	}

	events := make(chan Event, 64)
	go func() {
		defer close(events)
		defer k.unwatch(id)
		defer func() {
			k.mu.Lock()
			delete(k.inputs, id)
			k.mu.Unlock()
		}()
		var (
			done     Event
			gotReply bool
			idle     bool
		)
		done.Kind = EvDone
		for !gotReply || !idle {
			select {
			case req := <-inputs:
				var c struct {
					Prompt   string `json:"prompt"`
					Password bool   `json:"password"`
				}
				json.Unmarshal(req.Content, &c)
				events <- Event{Kind: EvInput, Input: &InputRequest{Prompt: c.Prompt, Password: c.Password, msg: req}}
			case r := <-reply:
				gotReply = true
				var c struct {
					Status    string `json:"status"`
					ExecCount int    `json:"execution_count"`
				}
				json.Unmarshal(r.Content, &c)
				done.Status, done.ExecCount = c.Status, c.ExecCount
			case msg := <-iopub:
				switch t := msg.Header.MsgType; t {
				case "status":
					if bytes.Contains(msg.Content, []byte(`"idle"`)) {
						idle = true
					}
				case "execute_input":
					var c struct {
						ExecCount int `json:"execution_count"`
					}
					json.Unmarshal(msg.Content, &c)
					events <- Event{Kind: EvStarted, ExecCount: c.ExecCount}
				case "stream", "display_data", "execute_result", "error":
					out, err := notebook.NewOutput(t, msg.Content)
					if err == nil {
						events <- Event{Kind: EvOutput, Output: out}
					}
				case "clear_output":
					events <- Event{Kind: EvClear}
				}
			case <-k.dead:
				done.Status = "dead"
				done.Err = k.deathError("kernel died")
				events <- done
				return
			}
		}
		events <- done
	}()
	return id, events, nil
}

// Interrupt asks the kernel to interrupt over the control channel, and
// falls back to SIGINT for kernels that don't support that.
func (k *Kernel) Interrupt() error {
	m, err := k.newMessage("interrupt_request", struct{}{})
	if err != nil {
		return err
	}
	if _, err := k.request(k.control, m, 2*time.Second); err == nil {
		return nil
	}
	if k.remote {
		// k.pid is the local ssh, signalling it would just drop the connection
		return errors.New("remote kernel didn't take the interrupt")
	}
	return syscall.Kill(-k.pid, syscall.SIGINT)
}

// Shutdown asks nicely, then kills the whole process group.
func (k *Kernel) Shutdown() error {
	select {
	case <-k.dead:
		return nil
	default:
	}
	// never connected (startup failed or was cancelled), nothing to ask nicely
	grace := time.Duration(0)
	if k.control != nil {
		if m, err := k.newMessage("shutdown_request", map[string]bool{"restart": false}); err == nil {
			k.request(k.control, m, 2*time.Second)
		}
		grace = 3 * time.Second
	}
	select {
	case <-k.dead:
	case <-time.After(grace):
		if k.remoteStdin != nil {
			k.remoteStdin.Close()
		}
		syscall.Kill(-k.pid, syscall.SIGKILL)
		<-k.dead
	}
	return nil
}

func (k *Kernel) stop() {
	k.stopOnce.Do(func() {
		// clean up before closing dead, callers may exit as soon as it closes
		k.cancel()
		for _, s := range []zmq4.Socket{k.iopub} {
			if s != nil {
				s.Close()
			}
		}
		for _, s := range []*sock{k.shell, k.control, k.stdin} {
			if s != nil {
				s.s.Close()
			}
		}
		for _, c := range []io.WriteCloser{k.remoteStdin, k.tunnelStdin} {
			if c != nil {
				c.Close()
			}
		}
		for _, c := range k.extra {
			if c.Process != nil {
				syscall.Kill(-c.Process.Pid, syscall.SIGTERM)
			}
		}
		if !k.released {
			os.RemoveAll(filepath.Dir(k.connFile))
		}
		close(k.dead)
		select {
		case k.status <- "dead":
		default:
		}
	})
}

// Status reports busy/idle/starting and finally dead.
func (k *Kernel) Status() <-chan string { return k.status }

// Dead closes when the kernel process exits.
func (k *Kernel) Dead() <-chan struct{} { return k.dead }

// Logs is the tail of the kernel process's stdout/stderr.
func (k *Kernel) Logs() string {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(k.connFile), "kernel.log"))
	if err != nil {
		return ""
	}
	if len(b) > 16<<10 {
		b = b[len(b)-16<<10:]
	}
	return string(b)
}

func (k *Kernel) deathError(msg string) error {
	logs := strings.TrimSpace(k.Logs())
	if logs == "" {
		return errors.New(msg)
	}
	return fmt.Errorf("%s:\n%s", msg, logs)
}
