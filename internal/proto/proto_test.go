package proto

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

func must(m *Message, err error) *Message {
	if err != nil {
		panic(err)
	}
	return m
}

func TestRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		msg  *Message
		want string // exact wire encoding, without the trailing newline
		kind string
	}{
		{
			name: "request int id",
			msg:  must(NewRequest(1, "tools/call", map[string]any{"name": "tazzy_next"})),
			want: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"tazzy_next"}}`,
			kind: "request",
		},
		{
			name: "request string id no params",
			msg:  must(NewRequest("abc", "ping", nil)),
			want: `{"jsonrpc":"2.0","id":"abc","method":"ping"}`,
			kind: "request",
		},
		{
			name: "notification",
			msg:  must(NewNotification("notifications/initialized", nil)),
			want: `{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			kind: "notification",
		},
		{
			name: "result",
			msg:  must(NewResult(json.RawMessage(`7`), map[string]int{"n": 2})),
			want: `{"jsonrpc":"2.0","id":7,"result":{"n":2}}`,
			kind: "response",
		},
		{
			name: "null result",
			msg:  must(NewResult(json.RawMessage(`"x"`), nil)),
			want: `{"jsonrpc":"2.0","id":"x","result":null}`,
			kind: "response",
		},
		{
			name: "error",
			msg:  NewError(json.RawMessage(`3`), &Error{Code: CodeMethodNotFound, Message: "nope", Data: json.RawMessage(`{"m":"x"}`)}),
			want: `{"jsonrpc":"2.0","id":3,"error":{"code":-32601,"message":"nope","data":{"m":"x"}}}`,
			kind: "response",
		},
		{
			name: "error null id",
			msg:  NewError(nil, &Error{Code: CodeParseError, Message: "bad"}),
			want: `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"bad"}}`,
			kind: "response",
		},
		{
			name: "newlines are escaped and raw params compacted",
			msg: &Message{JSONRPC: Version, Method: "say", Params: json.RawMessage(`{
  "text": "a\nb"
}`)},
			want: `{"jsonrpc":"2.0","method":"say","params":{"text":"a\nb"}}`,
			kind: "notification",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := NewWriter(&buf).Write(tt.msg); err != nil {
				t.Fatal(err)
			}
			if got := buf.String(); got != tt.want+"\n" {
				t.Fatalf("wire:\n got %s\nwant %s", got, tt.want)
			}
			got, err := NewReader(&buf).Read()
			if err != nil {
				t.Fatal(err)
			}
			if k := kind(got); k != tt.kind {
				t.Errorf("kind = %s, want %s", k, tt.kind)
			}
			var again bytes.Buffer
			NewWriter(&again).Write(got)
			if again.String() != tt.want+"\n" {
				t.Errorf("re-encoded:\n got %s\nwant %s", again.String(), tt.want)
			}
		})
	}
}

func kind(m *Message) string {
	switch {
	case m.IsRequest():
		return "request"
	case m.IsNotification():
		return "notification"
	case m.IsResponse():
		return "response"
	}
	return "unknown"
}

func TestReadMalformed(t *testing.T) {
	tests := []struct {
		name string
		line string
		code int
	}{
		{"not json", `hello`, CodeInvalidRequest},
		{"truncated", `{"jsonrpc":"2.0","method":"x"`, CodeParseError},
		{"batch", `[{"jsonrpc":"2.0","method":"x"}]`, CodeInvalidRequest},
		{"wrong version", `{"jsonrpc":"1.0","method":"x"}`, CodeInvalidRequest},
		{"missing version", `{"method":"x"}`, CodeInvalidRequest},
		{"wrong field type", `{"jsonrpc":"2.0","method":5}`, CodeParseError},
		{"empty object", `{"jsonrpc":"2.0"}`, CodeInvalidRequest},
		{"response without result", `{"jsonrpc":"2.0","id":1}`, CodeInvalidRequest},
		{"response with both", `{"jsonrpc":"2.0","id":1,"result":1,"error":{"code":1,"message":"m"}}`, CodeInvalidRequest},
		{"request with result", `{"jsonrpc":"2.0","id":1,"method":"x","result":1}`, CodeInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewReader(strings.NewReader(tt.line + "\n")).Read()
			var rpcErr *Error
			if !errors.As(err, &rpcErr) {
				t.Fatalf("err = %v, want *Error", err)
			}
			if rpcErr.Code != tt.code {
				t.Errorf("code = %d, want %d (%s)", rpcErr.Code, tt.code, rpcErr.Message)
			}
		})
	}
}

func TestReadStream(t *testing.T) {
	// Blank lines and CRLF are tolerated; a bad line doesn't end the stream;
	// a final line without a newline is still read.
	in := "\n{\"jsonrpc\":\"2.0\",\"method\":\"a\"}\r\n  \r\ngarbage\n{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}"
	r := NewReader(strings.NewReader(in))

	m, err := r.Read()
	if err != nil || m.Method != "a" {
		t.Fatalf("1st: %+v, %v", m, err)
	}
	if _, err := r.Read(); err == nil {
		t.Fatal("2nd: want error for garbage line")
	}
	m, err = r.Read()
	if err != nil || string(m.ID) != "2" {
		t.Fatalf("3rd: %+v, %v", m, err)
	}
	if _, err := r.Read(); err != io.EOF {
		t.Fatalf("4th: err = %v, want io.EOF", err)
	}
}

func TestWriterConcurrent(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			m, _ := NewRequest(i, "x", strings.Repeat("p", 1000))
			if err := w.Write(m); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	r := NewReader(&buf)
	for range 50 {
		if _, err := r.Read(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Read(); err != io.EOF {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}
