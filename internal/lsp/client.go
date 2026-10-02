// Package lsp is a minimal Language Server Protocol client over stdio,
// enough for completion, hover, definition, signature help and
// diagnostics. JSON-RPC framing is done by hand with the stdlib.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// DefaultCmd runs basedpyright through uvx: no install into the
// project, and its PyPI package bundles node.
var DefaultCmd = []string{"uvx", "--quiet", "--from", "basedpyright", "basedpyright-langserver", "--stdio"}

type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"` // utf-16 code units
}

type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity"` // 1 error, 2 warning, 3 info, 4 hint
	Message  string `json:"message"`
	Source   string `json:"source"`
	Code     any    `json:"code"`
}

type CompletionItem struct {
	Label      string `json:"label"`
	Kind       int    `json:"kind"`
	Detail     string `json:"detail"`
	InsertText string `json:"insertText"`
	FilterText string `json:"filterText"`
	SortText   string `json:"sortText"`
	TextEdit   *struct {
		Range   Range  `json:"range"`
		NewText string `json:"newText"`
	} `json:"textEdit"`
}

type Signature struct {
	Label           string
	ActiveParameter int
	Params          [][2]int // label offsets of each parameter
}

// Notification is a server -> client notification we care about.
type Notification struct {
	Method string
	URI    string
	Diags  []Diagnostic
}

type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	wmu    sync.Mutex
	nextID atomic.Int64

	mu       sync.Mutex
	pending  map[int64]chan response
	settings map[string]any

	notify chan Notification
	done   chan struct{}
	stderr *tail
}

type response struct {
	Result json.RawMessage
	Err    *rpcError
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("lsp error %d: %s", e.Code, e.Message) }

// Start launches the server in dir and runs the initialize handshake.
// settings answers workspace/configuration requests, keyed by section.
func Start(ctx context.Context, cmdline []string, dir string, settings map[string]any) (*Client, error) {
	if len(cmdline) == 0 {
		cmdline = DefaultCmd
	}
	cmd := exec.Command(cmdline[0], cmdline[1:]...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	c := &Client{
		cmd:      cmd,
		stdin:    stdin,
		pending:  map[int64]chan response{},
		settings: settings,
		notify:   make(chan Notification, 16),
		done:     make(chan struct{}),
		stderr:   &tail{max: 8 << 10},
	}
	cmd.Stderr = c.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start language server: %w", err)
	}
	go func() {
		c.readLoop(bufio.NewReader(stdout))
		cmd.Wait()
		close(c.done)
	}()

	root := FileURI(dir)
	init := map[string]any{
		// the server exits on its own if this pid goes away, so a killed
		// jupytui doesn't leave node running
		"processId": os.Getpid(),
		"rootUri":   root,
		"workspaceFolders": []map[string]string{
			{"uri": root, "name": filepath.Base(dir)},
		},
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"synchronization":    map[string]any{"didSave": false},
				"completion":         map[string]any{"completionItem": map[string]any{"snippetSupport": false}},
				"hover":              map[string]any{"contentFormat": []string{"markdown", "plaintext"}},
				"definition":         map[string]any{},
				"signatureHelp":      map[string]any{"signatureInformation": map[string]any{"parameterInformation": map[string]any{"labelOffsetSupport": true}}},
				"publishDiagnostics": map[string]any{},
			},
			"workspace": map[string]any{"configuration": true, "workspaceFolders": true},
		},
	}
	// first run of uvx downloads the server, give it time
	ictx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if _, err := c.Call(ictx, "initialize", init); err != nil {
		c.Close()
		return nil, c.withStderr(err)
	}
	c.Notify("initialized", map[string]any{})
	c.Notify("workspace/didChangeConfiguration", map[string]any{"settings": settings})
	return c, nil
}

// Notifications delivers diagnostics as they're published.
func (c *Client) Notifications() <-chan Notification { return c.notify }

// Done closes when the server process exits.
func (c *Client) Done() <-chan struct{} { return c.done }

func (c *Client) withStderr(err error) error {
	if s := strings.TrimSpace(c.stderr.String()); s != "" {
		return fmt.Errorf("%w: %s", err, s)
	}
	return err
}

// SetSettings updates what workspace/configuration returns and pushes it.
func (c *Client) SetSettings(settings map[string]any) {
	c.mu.Lock()
	c.settings = settings
	c.mu.Unlock()
	c.Notify("workspace/didChangeConfiguration", map[string]any{"settings": settings})
}

func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	ch := make(chan response, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		if r.Err != nil {
			return nil, r.Err
		}
		return r.Result, nil
	case <-c.done:
		return nil, errors.New("language server exited")
	case <-ctx.Done():
		c.Notify("$/cancelRequest", map[string]any{"id": id})
		return nil, ctx.Err()
	}
}

func (c *Client) Notify(method string, params any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *Client) write(msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n", len(b)); err != nil {
		return err
	}
	_, err = c.stdin.Write(b)
	return err
}

func (c *Client) readLoop(r *bufio.Reader) {
	for {
		n := -1
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				break
			}
			if v, ok := strings.CutPrefix(line, "Content-Length:"); ok {
				n, _ = strconv.Atoi(strings.TrimSpace(v))
			}
		}
		if n < 0 {
			continue
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(r, body); err != nil {
			return
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if json.Unmarshal(body, &msg) != nil {
			continue
		}
		switch {
		case msg.Method == "" && len(msg.ID) > 0:
			id, _ := strconv.ParseInt(string(msg.ID), 10, 64)
			c.mu.Lock()
			ch := c.pending[id]
			c.mu.Unlock()
			if ch != nil {
				ch <- response{Result: msg.Result, Err: msg.Error}
			}
		case msg.Method != "" && len(msg.ID) > 0:
			c.answer(msg.ID, msg.Method, msg.Params)
		case msg.Method == "textDocument/publishDiagnostics":
			var p struct {
				URI         string       `json:"uri"`
				Diagnostics []Diagnostic `json:"diagnostics"`
			}
			if json.Unmarshal(msg.Params, &p) == nil {
				select {
				case c.notify <- Notification{Method: msg.Method, URI: p.URI, Diags: p.Diagnostics}:
				default:
					// UI is behind; it'll get the next publish
				}
			}
		}
	}
}

// answer handles requests the server sends us.
func (c *Client) answer(id json.RawMessage, method string, params json.RawMessage) {
	var result any
	switch method {
	case "workspace/configuration":
		var p struct {
			Items []struct {
				Section string `json:"section"`
			} `json:"items"`
		}
		json.Unmarshal(params, &p)
		c.mu.Lock()
		out := make([]any, len(p.Items))
		for i, it := range p.Items {
			out[i] = lookup(c.settings, it.Section)
		}
		c.mu.Unlock()
		result = out
	}
	// everything else (registerCapability, workDoneProgress/create, ...)
	// just gets an empty success
	c.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// lookup walks a dotted section like "basedpyright.analysis".
func lookup(m map[string]any, section string) any {
	if section == "" {
		return m
	}
	var cur any = m
	for _, k := range strings.Split(section, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func (c *Client) DidOpen(uri, lang string, version int, text string) error {
	return c.Notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": uri, "languageId": lang, "version": version, "text": text},
	})
}

// DidChange sends the full text; notebooks are small enough that
// incremental sync isn't worth it.
func (c *Client) DidChange(uri string, version int, text string) error {
	return c.Notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": version},
		"contentChanges": []map[string]any{{"text": text}},
	})
}

func at(uri string, p Position) map[string]any {
	return map[string]any{"textDocument": map[string]any{"uri": uri}, "position": p}
}

func (c *Client) Completion(ctx context.Context, uri string, p Position) ([]CompletionItem, error) {
	raw, err := c.Call(ctx, "textDocument/completion", at(uri, p))
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return nil, err
	}
	var list struct {
		Items []CompletionItem `json:"items"`
	}
	if raw[0] == '[' {
		err = json.Unmarshal(raw, &list.Items)
	} else {
		err = json.Unmarshal(raw, &list)
	}
	return list.Items, err
}

func (c *Client) Hover(ctx context.Context, uri string, p Position) (string, error) {
	raw, err := c.Call(ctx, "textDocument/hover", at(uri, p))
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return "", err
	}
	var h struct {
		Contents json.RawMessage `json:"contents"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return "", err
	}
	return markupText(h.Contents), nil
}

// markupText flattens MarkupContent / MarkedString / []MarkedString.
func markupText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var mc struct {
		Kind     string `json:"kind"`
		Value    string `json:"value"`
		Language string `json:"language"`
	}
	if json.Unmarshal(raw, &mc) == nil && mc.Value != "" {
		if mc.Language != "" {
			return "```" + mc.Language + "\n" + mc.Value + "\n```"
		}
		return mc.Value
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		var parts []string
		for _, a := range arr {
			parts = append(parts, markupText(a))
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

func (c *Client) Definition(ctx context.Context, uri string, p Position) ([]Location, error) {
	raw, err := c.Call(ctx, "textDocument/definition", at(uri, p))
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return nil, err
	}
	if raw[0] == '{' {
		var l Location
		err = json.Unmarshal(raw, &l)
		return []Location{l}, err
	}
	var ls []struct {
		Location
		TargetURI   string `json:"targetUri"`
		TargetRange Range  `json:"targetSelectionRange"`
	}
	if err := json.Unmarshal(raw, &ls); err != nil {
		return nil, err
	}
	out := make([]Location, 0, len(ls))
	for _, l := range ls {
		if l.TargetURI != "" {
			out = append(out, Location{URI: l.TargetURI, Range: l.TargetRange})
		} else {
			out = append(out, l.Location)
		}
	}
	return out, nil
}

func (c *Client) SignatureHelp(ctx context.Context, uri string, p Position) (*Signature, error) {
	raw, err := c.Call(ctx, "textDocument/signatureHelp", at(uri, p))
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return nil, err
	}
	var sh struct {
		Signatures []struct {
			Label      string `json:"label"`
			Parameters []struct {
				Label json.RawMessage `json:"label"`
			} `json:"parameters"`
			ActiveParameter *int `json:"activeParameter"`
		} `json:"signatures"`
		ActiveSignature int `json:"activeSignature"`
		ActiveParameter int `json:"activeParameter"`
	}
	if err := json.Unmarshal(raw, &sh); err != nil || len(sh.Signatures) == 0 {
		return nil, err
	}
	s := sh.Signatures[min(sh.ActiveSignature, len(sh.Signatures)-1)]
	sig := &Signature{Label: s.Label, ActiveParameter: sh.ActiveParameter}
	if s.ActiveParameter != nil {
		sig.ActiveParameter = *s.ActiveParameter
	}
	for _, p := range s.Parameters {
		var off [2]int
		var str string
		if json.Unmarshal(p.Label, &off) == nil {
			sig.Params = append(sig.Params, off)
		} else if json.Unmarshal(p.Label, &str) == nil {
			if i := strings.Index(s.Label, str); i >= 0 {
				sig.Params = append(sig.Params, [2]int{i, i + len(str)})
			}
		}
	}
	return sig, nil
}

// Close asks the server to shut down, then kills it.
func (c *Client) Close() {
	select {
	case <-c.done:
		return
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	c.Call(ctx, "shutdown", nil)
	cancel()
	c.Notify("exit", nil)
	c.stdin.Close()
	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
		<-c.done
	}
}

func FileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}

func URIPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return uri
	}
	return u.Path
}

type tail struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
