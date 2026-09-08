// Package appserver is a small JSON-RPC client for `codex app-server`.
//
// This is the supported way in. The app-server is a documented subcommand of
// the Codex CLI, and its protocol ships published JSON Schema and TypeScript
// definitions in the Codex repository — a contract for third-party clients,
// which is exactly what crescent is. Nothing here parses a file format or
// speaks to an undocumented socket.
//
// The server runs as a child process over stdio, the default transport. That
// means one owner of the session store at a time: the ChatGPT desktop
// application must be closed while crescent works. Not a workaround — a thread
// can only have one writer, and this is how you become it legitimately.
package appserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// Client speaks JSON-RPC to a running app-server.
type Client struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Scanner
	stderr *bufio.Scanner

	mu       sync.Mutex
	nextID   int
	pending  map[int]chan rpcResponse
	notify   func(method string, params json.RawMessage)
	closed   bool
	closeErr error
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type rpcResponse struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

// Options configure a client.
type Options struct {
	// CodexPath is the binary to run. Required.
	CodexPath string
	// OnNotification receives server-initiated messages, which is how the
	// reset of a usage limit arrives without polling for it.
	OnNotification func(method string, params json.RawMessage)
	// Stderr receives the server's diagnostics; nil discards them.
	Stderr io.Writer
}

// Dial starts `codex app-server` and completes the handshake.
func Dial(ctx context.Context, o Options) (*Client, error) {
	if o.CodexPath == "" {
		return nil, errors.New("appserver: не указан путь к codex")
	}
	cmd := exec.CommandContext(ctx, o.CodexPath, "app-server")

	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("не удалось запустить %s app-server: %w", o.CodexPath, err)
	}

	c := &Client{
		cmd:     cmd,
		in:      in,
		out:     bufio.NewScanner(out),
		pending: map[int]chan rpcResponse{},
		notify:  o.OnNotification,
	}
	// A single message can carry a whole file's worth of diff.
	c.out.Buffer(make([]byte, 0, 256<<10), 64<<20)

	go c.readLoop()
	go drain(errPipe, o.Stderr)

	// The handshake tells the server who is asking; without it later calls are
	// rejected.
	if _, err := c.Call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "crescent",
			"title":   "crescent",
			"version": "0.1",
		},
	}); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("рукопожатие не прошло: %w", err)
	}
	return c, nil
}

func drain(r io.Reader, w io.Writer) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if w != nil {
			fmt.Fprintln(w, sc.Text())
		}
	}
}

// readLoop dispatches replies to whoever is waiting and hands notifications to
// the callback.
func (c *Client) readLoop() {
	for c.out.Scan() {
		var msg rpcResponse
		if json.Unmarshal(c.out.Bytes(), &msg) != nil {
			continue
		}
		if msg.Method != "" && msg.ID == 0 {
			if c.notify != nil {
				c.notify(msg.Method, msg.Params)
			}
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[msg.ID]
		delete(c.pending, msg.ID)
		c.mu.Unlock()
		if ok {
			ch <- msg
		}
	}
	c.fail(errors.New("app-server закрыл поток"))
}

func (c *Client) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed, c.closeErr = true, err
	for id, ch := range c.pending {
		ch <- rpcResponse{ID: id, Error: &rpcError{Message: err.Error()}}
		delete(c.pending, id)
	}
}

// Call sends a request and waits for its reply.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}

	c.mu.Lock()
	if c.closed {
		err := c.closeErr
		c.mu.Unlock()
		return nil, err
	}
	c.nextID++
	id := c.nextID
	ch := make(chan rpcResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	if _, err := c.in.Write(append(body, '\n')); err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("%s: %w", method, resp.Error)
		}
		return resp.Result, nil
	}
}

// Close shuts the server down.
func (c *Client) Close() error {
	c.mu.Lock()
	already := c.closed
	c.closed = true
	c.mu.Unlock()
	if !already {
		_ = c.in.Close()
	}
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		_ = c.cmd.Process.Kill()
		return <-done
	}
}
