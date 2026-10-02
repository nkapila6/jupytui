package ui

import (
	"context"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/nkapila6/jupytui/internal/kernel"
)

// kernelHost owns every kernel the UI starts so Close can shut them all
// down, including one that finishes starting after the user quit.
type kernelHost struct {
	opts   kernel.Options
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
	live   map[*kernel.Kernel]bool
}

func newKernelHost(opts kernel.Options) *kernelHost {
	ctx, cancel := context.WithCancel(context.Background())
	return &kernelHost{opts: opts, ctx: ctx, cancel: cancel, live: map[*kernel.Kernel]bool{}}
}

// start shuts down old (if any) and starts a fresh kernel.
func (h *kernelHost) start(old *kernel.Kernel) tea.Cmd {
	return func() tea.Msg {
		// a cmd can run after Close, or never; only count ones that run
		h.mu.Lock()
		if h.closed {
			h.mu.Unlock()
			return nil
		}
		h.wg.Add(1)
		h.mu.Unlock()
		defer h.wg.Done()

		if old != nil {
			old.Shutdown()
			h.mu.Lock()
			delete(h.live, old)
			h.mu.Unlock()
		}
		opts := h.opts
		opts.Context = h.ctx
		k, err := kernel.Start(opts)
		if k != nil {
			h.mu.Lock()
			h.live[k] = true
			h.mu.Unlock()
		}
		return KernelMsg{Kernel: k, Err: err}
	}
}

func (h *kernelHost) Close() {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	h.cancel()
	h.wg.Wait()
	h.mu.Lock()
	defer h.mu.Unlock()
	for k := range h.live {
		k.Shutdown()
	}
	h.live = map[*kernel.Kernel]bool{}
}
