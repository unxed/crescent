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
	"os"
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

	mu sync.Mutex
	// writeMu serialises writes to the server's stdin: a response can be sent
	// from a request handler while a call is in flight.
	writeMu sync.Mutex
	// onRequest answers requests the server sends us. Without an answer the
	// turn simply waits, which is how an unattended run stops being unattended.
	onRequest func(id int, method string, params json.RawMessage)
	nextID    int
	pending   map[int]chan rpcResponse
	notify    func(method string, params json.RawMessage)
	activity  func(Activity)
	stderrFn  func(string)
	// wire records every line in both directions, verbatim. Only what we
	// thought to parse is visible anywhere else; a request from the server, or
	// a message of a kind not anticipated, leaves no trace at all.
	wire     func(dir, line string)
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
	// OnNotification receives every server-initiated message, raw. Most callers
	// want OnActivity instead.
	OnNotification func(method string, params json.RawMessage)
	// OnActivity receives notifications already distilled to a readable line,
	// which is what a per-goal log wants.
	OnActivity func(Activity)
	// Stderr receives the server's diagnostics; nil discards them.
	Stderr io.Writer
	// LogLevel is the RUST_LOG filter handed to the server. Empty means info.
	LogLevel string
}

// Dial starts `codex app-server` and completes the handshake.
func Dial(ctx context.Context, o Options) (*Client, error) {
	if o.CodexPath == "" {
		return nil, errors.New("appserver: не указан путь к codex")
	}
	if o.LogLevel == "" {
		o.LogLevel = "info"
	}
	cmd := exec.CommandContext(ctx, o.CodexPath, "app-server")
	// app-server logs through `tracing` to stderr, filtered by
	// EnvFilter::from_default_env() — that is, RUST_LOG. With the variable
	// unset the filter passes nothing, which is why stderr was completely
	// silent on a live run while the server was dying. Asking for info level
	// costs nothing and turns the silence into an explanation.
	//
	// (Source: codex-rs/app-server/src/lib.rs, where the stderr layer is built
	// with EnvFilter::from_default_env() and LOG_FORMAT=json is offered.)
	env := os.Environ()
	if os.Getenv("RUST_LOG") == "" {
		env = append(env, "RUST_LOG="+o.LogLevel)
	}
	cmd.Env = env

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
		cmd:      cmd,
		in:       in,
		out:      bufio.NewScanner(out),
		pending:  map[int]chan rpcResponse{},
		notify:   o.OnNotification,
		activity: o.OnActivity,
	}
	// A single message can carry a whole file's worth of diff.
	c.out.Buffer(make([]byte, 0, 256<<10), 64<<20)

	go c.readLoop()
	go c.drainStderr(errPipe, o.Stderr)

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
	// The handshake has a second beat: a bare `initialized` notification, no id,
	// no params. Sending only the request left the server waiting for it, and it
	// closed the connection cleanly (code 0) — which read as "app-server closed
	// the stream" the moment the first real call went out.
	if err := c.Notify("initialized", nil); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("initialized не отправлен: %w", err)
	}
	return c, nil
}

// Notify sends a notification: a message with a method and no id, so the server
// never replies. Used for the `initialized` half of the handshake.
func (c *Client) Notify(method string, params any) error {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return c.closeErr
	}
	_, err = c.in.Write(append(body, '\n'))
	return err
}

// drainStderr reads the server's diagnostics. Every line goes to the sink as
// well as to any writer: when the server dies, its last words are the only
// evidence of why.
func (c *Client) drainStderr(r io.Reader, w io.Writer) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if w != nil {
			fmt.Fprintln(w, line)
		}
		c.mu.Lock()
		fn := c.stderrFn
		c.mu.Unlock()
		if fn != nil && line != "" {
			fn(line)
		}
	}
}

// stderrSink returns the diagnostic callback, if any.
func (c *Client) stderrSink() func(string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stderrFn
}

// SetRequestHandler installs the answer to server requests.
func (c *Client) SetRequestHandler(fn func(id int, method string, params json.RawMessage)) {
	c.mu.Lock()
	c.onRequest = fn
	c.mu.Unlock()
}

// Respond answers a request from the server.
func (c *Client) Respond(id int, result any) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.recordWire(">>", string(body))
	_, err = c.in.Write(append(body, '\n'))
	return err
}

// SetWireSink installs a recorder for the raw protocol in both directions.
func (c *Client) SetWireSink(fn func(dir, line string)) {
	c.mu.Lock()
	c.wire = fn
	c.mu.Unlock()
}

func (c *Client) recordWire(dir, line string) {
	c.mu.Lock()
	fn := c.wire
	c.mu.Unlock()
	if fn != nil {
		fn(dir, line)
	}
}

// SetStderrSink installs a callback for the server's diagnostic output.
func (c *Client) SetStderrSink(fn func(string)) {
	c.mu.Lock()
	c.stderrFn = fn
	c.mu.Unlock()
}

// readLoop dispatches replies to whoever is waiting and hands notifications to
// the callback.
func (c *Client) readLoop() {
	for c.out.Scan() {
		raw := c.out.Text()
		c.recordWire("<<", raw)

		var msg rpcResponse
		if json.Unmarshal([]byte(raw), &msg) != nil {
			c.recordWire("!!", "не разобрано как JSON-RPC")
			continue
		}
		// A message carrying both a method and an id is a request from the
		// server — it expects an answer. Nothing here answers one; recording
		// it is the point of this pass.
		// A message carrying both a method and an id is a request from the
		// server, and it blocks the turn until answered.
		if msg.Method != "" && msg.ID != 0 {
			c.mu.Lock()
			fn := c.onRequest
			c.mu.Unlock()
			if fn != nil {
				go fn(msg.ID, msg.Method, msg.Params)
			} else {
				c.recordWire("??", "ЗАПРОС СЕРВЕРА без ответа: "+msg.Method)
			}
			continue
		}
		if msg.Method != "" && msg.ID == 0 {
			if c.notify != nil {
				c.notify(msg.Method, msg.Params)
			}
			if c.activity != nil {
				if a, ok := Interpret(msg.Method, msg.Params); ok {
					c.activity(a)
				}
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
	// The stream ended. Wait for the process so its exit status can be
	// reported: on a live run stderr was completely silent, which leaves the
	// exit code as the only remaining evidence of why it stopped.
	why := "app-server завершился"
	if err := c.cmd.Wait(); err != nil {
		why += ": " + err.Error()
	} else if st := c.cmd.ProcessState; st != nil {
		why += fmt.Sprintf(": код выхода %d", st.ExitCode())
	}
	if fn := c.stderrSink(); fn != nil {
		fn(why)
	}
	c.fail(errors.New(why))
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
	c.recordWire(">>", string(body))
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

// SetActivitySink installs the interpreted-activity callback after Dial, which
// is when a journal is usually ready.
func (c *Client) SetActivitySink(fn func(Activity)) {
	c.mu.Lock()
	c.activity = fn
	c.mu.Unlock()
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
	// Wait may already have been called by readLoop when the stream ended;
	// calling it twice is an error, so a kill is enough here.
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		return nil
	}
}
