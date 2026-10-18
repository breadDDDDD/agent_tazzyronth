// Package proto implements JSON-RPC 2.0 messages framed as NDJSON (one JSON
// object per line). It is shared by the MCP stdio server and the hub wire.
package proto

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

const Version = "2.0"

// Standard JSON-RPC 2.0 error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// MaxLineSize bounds a single NDJSON line.
const MaxLineSize = 32 << 20

// Message is any JSON-RPC 2.0 message: a request (ID and Method), a
// notification (Method only) or a response (ID and Result or Error).
// ID and payloads stay raw so callers decode them into their own types.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

func (m *Message) IsRequest() bool      { return m.Method != "" && m.ID != nil }
func (m *Message) IsNotification() bool { return m.Method != "" && m.ID == nil }
func (m *Message) IsResponse() bool     { return m.Method == "" && m.ID != nil }

// Error is a JSON-RPC error object. It also satisfies the error interface so
// Reader can report parse/invalid-request failures that callers reply with.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message) }

// NewRequest builds a request; id is typically an int or string.
func NewRequest(id any, method string, params any) (*Message, error) {
	m := &Message{JSONRPC: Version, Method: method}
	var err error
	if m.ID, err = json.Marshal(id); err != nil {
		return nil, err
	}
	m.Params, err = marshalOptional(params)
	return m, err
}

// NewNotification builds a notification (no id, no response expected).
func NewNotification(method string, params any) (*Message, error) {
	p, err := marshalOptional(params)
	return &Message{JSONRPC: Version, Method: method, Params: p}, err
}

// NewResult builds a success response to the request with the given id.
func NewResult(id json.RawMessage, result any) (*Message, error) {
	r, err := json.Marshal(result)
	return &Message{JSONRPC: Version, ID: id, Result: r}, err
}

// NewError builds an error response. A nil id becomes null, as the spec
// requires when the request id could not be determined.
func NewError(id json.RawMessage, e *Error) *Message {
	if id == nil {
		id = json.RawMessage("null")
	}
	return &Message{JSONRPC: Version, ID: id, Error: e}
}

func marshalOptional(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

// Reader reads messages from an NDJSON stream. It is not safe for concurrent use.
type Reader struct {
	s *bufio.Scanner
}

func NewReader(r io.Reader) *Reader {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64<<10), MaxLineSize)
	return &Reader{s: s}
}

// Read returns the next message, skipping blank lines. A malformed line yields
// an *Error (CodeParseError or CodeInvalidRequest) and the stream stays usable.
// io.EOF is returned at end of input; other errors are fatal for the stream.
func (r *Reader) Read() (*Message, error) {
	for r.s.Scan() {
		line := bytes.TrimSpace(r.s.Bytes())
		if len(line) == 0 {
			continue
		}
		return parse(line)
	}
	if err := r.s.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

func parse(line []byte) (*Message, error) {
	if line[0] != '{' {
		// Includes batches, which MCP no longer supports.
		return nil, &Error{Code: CodeInvalidRequest, Message: "expected a JSON object"}
	}
	var m Message
	if err := json.Unmarshal(line, &m); err != nil {
		return nil, &Error{Code: CodeParseError, Message: err.Error()}
	}
	if m.JSONRPC != Version {
		return nil, &Error{Code: CodeInvalidRequest, Message: `jsonrpc must be "2.0"`}
	}
	switch {
	case m.Method != "":
		if m.Result != nil || m.Error != nil {
			return nil, &Error{Code: CodeInvalidRequest, Message: "request must not carry result or error"}
		}
	case m.ID == nil:
		return nil, &Error{Code: CodeInvalidRequest, Message: "message has neither method nor id"}
	case (m.Result != nil) == (m.Error != nil):
		return nil, &Error{Code: CodeInvalidRequest, Message: "response must have exactly one of result or error"}
	}
	return &m, nil
}

// Writer writes messages as NDJSON. It is safe for concurrent use.
type Writer struct {
	mu sync.Mutex
	w  io.Writer
}

func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

func (w *Writer) Write(m *Message) error {
	// json.Marshal escapes newlines in strings and compacts RawMessage
	// fields, so the encoding is always a single line.
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err = w.w.Write(b)
	return err
}
