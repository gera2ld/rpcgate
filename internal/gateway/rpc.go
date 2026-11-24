package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// request is one JSON-RPC call. Params stay raw: EVM methods take arrays,
// while Solana and by-name callers may send objects or omit them.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type errorResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   rpcError        `json:"error"`
}

// Error codes. Clients with an adaptive eth_getLogs range commonly treat
// -32005 with "rate limit" or "quota" as back-off-and-retry, and -32005 with
// "exceeds" as a range limit to shrink for.
const (
	codeParse    = -32700
	codeInvalid  = -32600
	codeMethod   = -32601
	codeParams   = -32602
	codeLimit    = -32005
	codeUpstream = -32603
	// codeUnauthorized answers an unknown, disabled or revoked token. The
	// message is the same in every case so it reveals nothing.
	codeUnauthorized = -32001

	nullID          = "null"
	contentTypeJSON = "application/json"
)

// parseBody reads a single call or a batch. It returns a JSON-RPC error
// message and code when the body is unusable.
func parseBody(body []byte, maxBatch int) (reqs []request, batch bool, msg string, code int) {
	trimmed := bytes.TrimSpace(body)
	batch = len(trimmed) > 0 && trimmed[0] == '['
	if !batch {
		var one request
		if err := json.Unmarshal(trimmed, &one); err != nil {
			return nil, false, "invalid JSON", codeParse
		}
		return []request{one}, false, "", 0
	}
	if err := json.Unmarshal(trimmed, &reqs); err != nil {
		return nil, true, "invalid JSON", codeParse
	}
	if len(reqs) == 0 {
		return nil, true, "empty batch", codeInvalid
	}
	if len(reqs) > maxBatch {
		return nil, true, fmt.Sprintf("batch of %d exceeds gateway limit of %d", len(reqs), maxBatch), codeLimit
	}
	return reqs, true, "", 0
}

// paramsArray decodes positional params; nil or absent params are empty.
func paramsArray(raw json.RawMessage) ([]json.RawMessage, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == nullID {
		return nil, true
	}
	var out []json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false
	}
	return out, true
}

func writeError(w http.ResponseWriter, status int, id json.RawMessage, code int, msg string) {
	if len(id) == 0 {
		id = json.RawMessage(nullID)
	}
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{JSONRPC: "2.0", ID: id, Error: rpcError{Code: code, Message: msg}})
}

// reject answers every request in the call with the same error, as an array
// for a batch so JSON-RPC clients can match the items by ID.
func reject(w http.ResponseWriter, status int, reqs []request, batch bool, code int, msg string) {
	if !batch {
		writeError(w, status, reqs[0].ID, code, msg)
		return
	}
	out := make([]errorResponse, len(reqs))
	for i, r := range reqs {
		id := r.ID
		if len(id) == 0 {
			id = json.RawMessage(nullID)
		}
		out[i] = errorResponse{JSONRPC: "2.0", ID: id, Error: rpcError{Code: code, Message: msg}}
	}
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(out)
}

func methodList(reqs []request) string {
	if len(reqs) == 1 {
		return safeMethod(reqs[0].Method)
	}
	counts := map[string]int{}
	var order []string
	for _, r := range reqs {
		m := safeMethod(r.Method)
		if counts[m] == 0 {
			order = append(order, m)
		}
		counts[m]++
	}
	parts := make([]string, len(order))
	for i, m := range order {
		parts[i] = fmt.Sprintf("%s×%d", m, counts[m])
	}
	return "batch[" + strings.Join(parts, ",") + "]"
}

// safeMethod shortens a caller-chosen method name for logs and messages, and
// keeps only printable ASCII so it cannot forge log lines.
func safeMethod(m string) string {
	const max = 64
	var b strings.Builder
	for _, r := range m {
		if b.Len() >= max {
			b.WriteString("…")
			break
		}
		if r < 0x20 || r > 0x7e {
			b.WriteByte('?')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
