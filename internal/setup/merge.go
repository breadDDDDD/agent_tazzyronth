package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

type field struct {
	key string
	val json.RawMessage
}

// fields parses a JSON object, keeping its keys in order.
func fields(b []byte) ([]field, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var out []field
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, field{t.(string), v})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err == nil {
		return nil, errors.New("data after the top-level object")
	}
	return out, nil
}

func object(fs []field) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range fs {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(f.key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(f.val)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// mergeServer adds mcpServers.<server> = entry to the JSON config old (empty
// = new file). An existing entry is left alone, so is every other key.
func mergeServer(old []byte, entry any) ([]byte, bool, error) {
	old = bytes.TrimPrefix(old, []byte("\xef\xbb\xbf"))
	var top []field
	if len(bytes.TrimSpace(old)) > 0 {
		var err error
		if top, err = fields(old); err != nil {
			return nil, false, err
		}
	}
	e, err := json.Marshal(entry)
	if err != nil {
		return nil, false, err
	}
	i := slicesIndex(top, "mcpServers")
	if i < 0 {
		top = append(top, field{"mcpServers", object([]field{{server, e}})})
	} else {
		servers, err := fields(top[i].val)
		if err != nil {
			return nil, false, errors.New("mcpServers: " + err.Error())
		}
		if slicesIndex(servers, server) >= 0 {
			return old, false, nil
		}
		top[i].val = object(append(servers, field{server, e}))
	}
	return indent(object(top))
}

// mergeAllow adds rule to permissions.allow (a list of strings) in the JSON
// settings old (empty = new file), keeping everything else. A rule already
// there means no change.
func mergeAllow(old []byte, rule string) ([]byte, bool, error) {
	old = bytes.TrimPrefix(old, []byte("\xef\xbb\xbf"))
	var top []field
	if len(bytes.TrimSpace(old)) > 0 {
		var err error
		if top, err = fields(old); err != nil {
			return nil, false, err
		}
	}
	r, _ := json.Marshal([]string{rule})
	i := slicesIndex(top, "permissions")
	if i < 0 {
		top = append(top, field{"permissions", object([]field{{"allow", r}})})
	} else {
		perms, err := fields(top[i].val)
		if err != nil {
			return nil, false, errors.New("permissions: " + err.Error())
		}
		j := slicesIndex(perms, "allow")
		if j < 0 {
			perms = append(perms, field{"allow", r})
		} else {
			var allow []json.RawMessage
			if err := json.Unmarshal(perms[j].val, &allow); err != nil {
				return nil, false, errors.New("permissions.allow: not a list")
			}
			for _, a := range allow {
				var s string
				if json.Unmarshal(a, &s) == nil && s == rule {
					return old, false, nil
				}
			}
			q, _ := json.Marshal(rule)
			perms[j].val, _ = json.Marshal(append(allow, q))
		}
		top[i].val = object(perms)
	}
	return indent(object(top))
}

// indent re-indents a merged config (2 spaces, trailing newline).
func indent(b []byte) ([]byte, bool, error) {
	var compact, out bytes.Buffer
	if err := json.Compact(&compact, b); err != nil {
		return nil, false, err
	}
	json.Indent(&out, compact.Bytes(), "", "  ")
	out.WriteByte('\n')
	return out.Bytes(), true, nil
}

func slicesIndex(fs []field, key string) int {
	for i, f := range fs {
		if f.key == key {
			return i
		}
	}
	return -1
}

// hasTOMLTable reports whether the TOML text defines the dotted table name,
// as a [table] header or a dotted key (detection by text: no TOML parser).
func hasTOMLTable(text, name string) bool {
	for _, line := range strings.Split(text, "\n") {
		l := strings.NewReplacer(" ", "", "\t", "", `"`, "", "'", "").Replace(line)
		if strings.HasPrefix(l, "["+name+"]") || strings.HasPrefix(l, "["+name+".") || strings.HasPrefix(l, name+".") {
			return true
		}
	}
	return false
}

// tomlString quotes s as a TOML string: a literal string when it can be one
// (Windows paths need no escaping), a basic string otherwise.
func tomlString(s string) string {
	if !strings.ContainsAny(s, "'\n\r") {
		return "'" + s + "'"
	}
	return strconv.Quote(s)
}
