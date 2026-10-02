package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/nkapila6/jupytui/internal/notebook"
)

// Attaching to a kernel someone else started (detach/reattach), and
// following executions someone else sent.

func writeOwner(dir string, pid int) error {
	return os.WriteFile(filepath.Join(dir, "owner.pid"), []byte(strconv.Itoa(pid)), 0o600)
}

// ConnFile is the kernel's connection file.
func (k *Kernel) ConnFile() string { return k.connFile }

// PID is the kernel's process group leader.
func (k *Kernel) PID() int { return k.pid }

// SetOwner makes pid the process the kernel's watchdog follows: when it
// dies, the kernel exits too.
func (k *Kernel) SetOwner(pid int) error {
	return writeOwner(filepath.Dir(k.connFile), pid)
}

// Release disconnects without stopping the kernel or deleting its
// files, for handing it to another owner.
func (k *Kernel) Release() {
	k.released = true
	k.stop()
}

// Attach connects to a running kernel from its connection file. pid is
// its process group leader, polled to notice when it dies.
func Attach(ctx context.Context, connFile string, pid int) (*Kernel, error) {
	b, err := os.ReadFile(connFile)
	if err != nil {
		return nil, err
	}
	var conn ConnInfo
	if err := json.Unmarshal(b, &conn); err != nil {
		return nil, fmt.Errorf("connection file: %w", err)
	}
	if !alive(pid) {
		return nil, errors.New("kernel isn't running anymore")
	}
	cctx, cancel := context.WithCancel(context.Background())
	k := &Kernel{
		conn:     conn,
		connFile: connFile,
		key:      []byte(conn.Key),
		session:  newUUID(),
		pid:      pid,
		ctx:      cctx,
		cancel:   cancel,
		replies:  map[string]chan *Message{},
		watchers: map[string]chan *Message{},
		status:   make(chan string, 32),
		dead:     make(chan struct{}),
	}
	// not our child, so no Wait: poll instead
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-k.dead:
				return
			case <-t.C:
				if !alive(pid) {
					k.stop()
					return
				}
			}
		}
	}()
	if err := k.connectWith(ctx, 20*time.Second, true); err != nil {
		k.Release()
		return nil, err
	}
	return k, nil
}

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Watch follows an execution sent by another client: its outputs on
// iopub until the kernel goes idle for it. The execute_reply went to
// whoever sent the request, so an empty silent execute acts as a
// barrier: executions run one after another, so its reply means the
// watched one is finished even if we missed its idle status. (Not
// kernel_info: ipykernel 7 answers that right away, mid-execution.)
func (k *Kernel) Watch(msgID string, execCount int) <-chan Event {
	iopub := k.watch(msgID)
	events := make(chan Event, 64)
	barrier := make(chan struct{})
	go func() {
		m, err := k.newMessage("execute_request", map[string]any{
			"code": "", "silent": true, "store_history": false,
			"user_expressions": map[string]any{}, "allow_stdin": false, "stop_on_error": false,
		})
		if err != nil {
			return
		}
		// as long as it takes; a training loop can run for hours
		if _, err := k.request(k.shell, m, 365*24*time.Hour); err == nil {
			close(barrier)
		}
	}()
	go func() {
		defer close(events)
		defer k.unwatch(msgID)
		done := Event{Kind: EvDone, Status: "ok", ExecCount: execCount}
		for {
			select {
			case msg := <-iopub:
				switch t := msg.Header.MsgType; t {
				case "status":
					if bytes.Contains(msg.Content, []byte(`"idle"`)) {
						events <- done
						return
					}
				case "execute_input":
					var c struct {
						ExecCount int `json:"execution_count"`
					}
					json.Unmarshal(msg.Content, &c)
					done.ExecCount = c.ExecCount
					events <- Event{Kind: EvStarted, ExecCount: c.ExecCount}
				case "stream", "display_data", "execute_result", "error":
					if t == "error" {
						done.Status = "error"
					}
					if out, err := notebook.NewOutput(t, msg.Content); err == nil {
						events <- Event{Kind: EvOutput, Output: out}
					}
				case "clear_output":
					events <- Event{Kind: EvClear}
				}
			case <-barrier:
				// drain anything that arrived with the barrier, then stop
				for {
					select {
					case msg := <-iopub:
						if t := msg.Header.MsgType; t == "stream" || t == "display_data" || t == "execute_result" || t == "error" {
							if out, err := notebook.NewOutput(t, msg.Content); err == nil {
								events <- Event{Kind: EvOutput, Output: out}
							}
						}
					default:
						events <- done
						return
					}
				}
			case <-k.dead:
				done.Status = "dead"
				done.Err = errors.New("kernel died")
				events <- done
				return
			}
		}
	}()
	return events
}

// ConnDir is where the kernel's files live.
func (k *Kernel) ConnDir() string { return filepath.Dir(k.connFile) }
