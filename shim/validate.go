package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
)

// Kiloswap: every request is read strictly (no duplicate keys anywhere, no
// unknown fields, parameters by position only), each method's parameters
// are checked against what the backend sends, and the node gets a request
// re-written from what the shim read, never the caller's bytes: a request
// the shim judged one way cannot be read another way by the node.
//
// The node builds each answer in memory: whole blocks only by hash and at
// verbosity 0 or 1 (never 2), at most maxGetblockPerBatch per batch and only
// raw (verbosity 0) there, and nothing verbose in batches.

const (
	// a batch carries at most maxRawPerBatch raw items (whole blocks and
	// raw transactions together), at most maxGetblockPerBatch of them blocks
	maxRawPerBatch      = 10
	maxGetblockPerBatch = 4
	maxSpendingPrevouts = 10
	maxMempoolAccept    = 25
	maxUnspentAddresses = 100
)

// maxResponse is the most the shim reads back from the node for one call
// (three whole 4 MB blocks in hex fit, the backend asks three at a time);
// with the batch held twice while patched it stays far under the shim's
// 256 MB memory limit.
var maxResponse = 32 << 20

var errDuplicateKey = errors.New("duplicate key")

// noDuplicateKeys walks raw and fails on any object with a key twice (Go's
// decoder keeps the last, the node's the first or last: never ambiguous).
func noDuplicateKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := walkValue(dec, 0); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

func walkValue(dec *json.Decoder, depth int) error {
	if depth > 16 {
		return errors.New("too deep")
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch tok {
	case json.Delim('{'):
		seen := map[string]bool{}
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := k.(string)
			if !ok {
				return errors.New("bad key")
			}
			if seen[key] {
				return fmt.Errorf("%w %q", errDuplicateKey, key)
			}
			seen[key] = true
			if err := walkValue(dec, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	case json.Delim('['):
		for dec.More() {
			if err := walkValue(dec, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	}
	return nil
}

// strictRequest reads one request: only the keys jsonrpc, id, method and
// params, spelt exactly, params an array (or absent/null), and returns it in
// canonical form (params re-encoded from the parsed values).
func strictRequest(raw []byte, inBatch bool) (rpcRequest, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return rpcRequest{}, err
	}
	var req rpcRequest
	for k, v := range fields {
		switch k {
		case "jsonrpc":
			if json.Unmarshal(v, &req.JSONRPC) != nil || (req.JSONRPC != "1.0" && req.JSONRPC != "2.0") {
				return rpcRequest{}, errors.New("bad jsonrpc")
			}
		case "id":
			var id any
			if json.Unmarshal(v, &id) != nil {
				return rpcRequest{}, errors.New("bad id")
			}
			switch id.(type) {
			case nil, string, float64:
			default:
				return rpcRequest{}, errors.New("bad id")
			}
			c, _ := json.Marshal(id)
			req.ID = c
		case "method":
			if json.Unmarshal(v, &req.Method) != nil {
				return rpcRequest{}, errors.New("bad method")
			}
		case "params":
		default:
			return rpcRequest{}, fmt.Errorf("unknown field %q", k)
		}
	}
	if req.Method == "" {
		return rpcRequest{}, errors.New("no method")
	}
	if !allowedMethods[req.Method] && !walletMethods[req.Method] {
		return rpcRequest{}, fmt.Errorf("%w: %s", errMethod, req.Method)
	}
	var params []any
	if p, ok := fields["params"]; ok && string(bytes.TrimSpace(p)) != "null" {
		dec := json.NewDecoder(bytes.NewReader(p))
		dec.UseNumber()
		if err := dec.Decode(&params); err != nil {
			return rpcRequest{}, errors.New("params must be an array")
		}
	}
	if err := checkParams(req.Method, params, inBatch); err != nil {
		return rpcRequest{}, fmt.Errorf("%s: %w", req.Method, err)
	}
	if params == nil {
		params = []any{}
	}
	c, err := json.Marshal(params)
	if err != nil {
		return rpcRequest{}, err
	}
	req.Params = c
	return req, nil
}

// canonical writes a request back with only the fields read.
func canonical(req rpcRequest) ([]byte, error) {
	m := map[string]json.RawMessage{"method": nil, "params": req.Params}
	m["method"], _ = json.Marshal(req.Method)
	if req.JSONRPC != "" {
		m["jsonrpc"], _ = json.Marshal(req.JSONRPC)
	}
	if req.ID != nil {
		m["id"] = req.ID
	}
	return json.Marshal(m)
}

func isHash(v any) bool {
	s, ok := v.(string)
	if !ok || len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func isHex(v any, maxLen int) bool {
	s, ok := v.(string)
	if !ok || len(s) == 0 || len(s)%2 != 0 || len(s) > maxLen {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func intIn(v any, lo, hi int64) bool {
	n, ok := v.(json.Number)
	if !ok {
		return false
	}
	i, err := n.Int64()
	return err == nil && i >= lo && i <= hi
}

func numIn(v any, lo, hi float64) bool {
	n, ok := v.(json.Number)
	if !ok {
		return false
	}
	f, err := n.Float64()
	return err == nil && !math.IsNaN(f) && f >= lo && f <= hi
}

func isBool(v any) bool { _, ok := v.(bool); return ok }

func isString(v any, maxLen int) bool {
	s, ok := v.(string)
	return ok && len(s) <= maxLen
}

// verbosity reads a getblock/getrawtransaction verbosity: 0/1 or false/true.
func verbosity(v any) (int64, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case json.Number:
		i, err := x.Int64()
		return i, err == nil
	}
	return 0, false
}

var (
	errParams = errors.New("parameters not allowed")
	errMethod = errors.New("method not allowed")
)

func checkParams(method string, p []any, inBatch bool) error {
	n := len(p)
	ok := false
	switch method {
	case "getblockchaininfo", "getbestblockhash", "getblockcount", "getmempoolinfo",
		"getnetworkinfo", "getzmqnotifications", "uptime":
		ok = n == 0
	case "getdeploymentinfo":
		ok = n == 0 || (n == 1 && isHash(p[0]))
	case "getblock":
		ok = n >= 1 && n <= 2 && isHash(p[0])
		if ok && n == 2 {
			v, good := verbosity(p[1])
			ok = good && (v == 0 || (v == 1 && !inBatch))
		} else if ok && inBatch {
			ok = false // verbosity 1 is the default: state 0 in a batch
		}
	case "getblockhash":
		ok = n == 1 && intIn(p[0], 0, math.MaxInt32)
	case "getblockheader":
		ok = n >= 1 && n <= 2 && isHash(p[0]) && (n == 1 || isBool(p[1]))
	case "getrawtransaction":
		ok = n >= 1 && n <= 3 && isHash(p[0])
		if ok && n >= 2 {
			v, good := verbosity(p[1])
			ok = good && (v == 0 || (v == 1 && !inBatch))
		}
		if ok && n == 3 {
			ok = isHash(p[2])
		}
	case "gettxout":
		ok = n >= 2 && n <= 3 && isHash(p[0]) && intIn(p[1], 0, 1<<20) && (n == 2 || isBool(p[2]))
	case "gettxspendingprevout":
		ok = n == 1 && !inBatch
		if ok {
			outs, isArr := p[0].([]any)
			ok = isArr && len(outs) >= 1 && len(outs) <= maxSpendingPrevouts
			for _, o := range outs {
				m, isObj := o.(map[string]any)
				if !ok || !isObj || len(m) != 2 || !isHash(m["txid"]) || !intIn(m["vout"], 0, 1<<20) {
					ok = false
				}
			}
		}
	case "getrawmempool":
		ok = n == 0 || (n == 1 && p[0] == false)
	case "getmempoolentry":
		ok = n == 1 && isHash(p[0])
	case "estimatesmartfee":
		ok = n >= 1 && n <= 2 && intIn(p[0], 1, 1008)
		if ok && n == 2 {
			ok = p[1] == "conservative" || p[1] == "economical" || p[1] == "unset"
		}
	case "sendrawtransaction":
		ok = n >= 1 && n <= 3 && isHex(p[0], 2*400_000)
		if ok && n >= 2 {
			ok = numIn(p[1], 0, 1)
		}
		if ok && n == 3 {
			ok = numIn(p[2], 0, 21e6)
		}
	case "testmempoolaccept":
		ok = n >= 1 && n <= 2 && !inBatch
		if ok {
			txs, isArr := p[0].([]any)
			ok = isArr && len(txs) >= 1 && len(txs) <= maxMempoolAccept
			for _, t := range txs {
				if !isHex(t, 2*400_000) {
					ok = false
				}
			}
			if ok && n == 2 {
				ok = numIn(p[1], 0, 1)
			}
		}
	case "getnewaddress":
		ok = n <= 2 && (n < 1 || isString(p[0], 100)) && (n < 2 || isString(p[1], 20))
	case "listunspent":
		ok = n <= 4 && (n < 1 || intIn(p[0], 0, 1<<30)) && (n < 2 || intIn(p[1], 0, 1<<30)) && (n < 4 || isBool(p[3]))
		if ok && n >= 3 {
			addrs, isArr := p[2].([]any)
			ok = isArr && len(addrs) <= maxUnspentAddresses
			for _, a := range addrs {
				if !isString(a, 100) {
					ok = false
				}
			}
		}
	case "gettransaction":
		ok = n >= 1 && n <= 3 && isHash(p[0]) && (n < 2 || isBool(p[1])) && (n < 3 || isBool(p[2]))
	}
	if !ok {
		return errParams
	}
	return nil
}
