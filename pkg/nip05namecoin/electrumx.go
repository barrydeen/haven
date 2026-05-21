package nip05namecoin

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Protocol version negotiated with the server.
const electrumProtocolVersion = "1.4"

// NameExpireDepth is the number of blocks after which a Namecoin name
// expires if not re-registered (≈ 250 days at 10 min/block). Sourced
// from Namecoin's chainparams.cpp → consensus.nNameExpirationDepth.
const NameExpireDepth = 36000

// NameShowResult is the structured outcome of a name_show lookup.
type NameShowResult struct {
	Name      string
	Value     string
	TxID      string
	Height    int
	ExpiresIn int // blocks until expiry; 0 if unknown
}

// Errors we surface to callers. NameNotFound and NameExpired are
// "definitive" (blockchain said so) — no point retrying other servers.
// ServersUnreachable is returned when every candidate server failed
// with a transport-level error.
var (
	ErrNameNotFound       = errors.New("nip05namecoin: name not found on Namecoin blockchain")
	ErrNameExpired        = errors.New("nip05namecoin: namecoin name has expired")
	ErrServersUnreachable = errors.New("nip05namecoin: all ElectrumX servers unreachable")
)

// rpcConn is the transport-agnostic RPC connection abstraction. HAVEN
// only ships the TCP/TCP+TLS transport — WSS is intentionally dropped
// from the server-only context.
type rpcConn interface {
	writeRequest(ctx context.Context, method string, params []any, id int64) error
	readResponse(ctx context.Context) (string, error)
	close() error
}

// ElectrumClient is a minimal, query-only Namecoin ElectrumX client.
// It opens a short-lived socket per request, which is plenty for the
// startup whitelist resolution that this package supports.
type ElectrumClient struct {
	ConnectTimeout time.Duration
	ReadTimeout    time.Duration
	requestID      atomic.Int64
}

// NewElectrumClient returns a client with sensible defaults.
func NewElectrumClient() *ElectrumClient {
	return &ElectrumClient{
		ConnectTimeout: 10 * time.Second,
		ReadTimeout:    15 * time.Second,
	}
}

// NameShow queries a single server and returns the current value for
// `identifier` (e.g. "d/example"). Returns nil + ErrNameNotFound when
// the name is provably absent, and a generic error for transport
// failures.
func (c *ElectrumClient) NameShow(ctx context.Context, identifier string, server ElectrumxServer) (*NameShowResult, error) {
	conn, err := c.dial(ctx, server)
	if err != nil {
		return nil, err
	}
	defer conn.close()

	reqCtx, cancel := context.WithTimeout(ctx, c.ReadTimeout)
	defer cancel()

	next := func() int64 { return c.requestID.Add(1) }

	// 1. Negotiate protocol version. The response is consumed and
	//    discarded — we only care that the socket is alive.
	if err := conn.writeRequest(reqCtx, "server.version", []any{"haven-namecoin/0.1", electrumProtocolVersion}, next()); err != nil {
		return nil, err
	}
	if _, err := conn.readResponse(reqCtx); err != nil {
		return nil, fmt.Errorf("nip05namecoin: read version response: %w", err)
	}

	// 2. Compute the name-index scripthash.
	script := buildNameIndexScript([]byte(identifier))
	scriptHash := electrumScriptHash(script)

	// 3. Fetch transaction history for that scripthash.
	if err := conn.writeRequest(reqCtx, "blockchain.scripthash.get_history", []any{scriptHash}, next()); err != nil {
		return nil, err
	}
	histLine, err := conn.readResponse(reqCtx)
	if err != nil {
		return nil, fmt.Errorf("nip05namecoin: read history response: %w", err)
	}
	entries, err := parseHistoryResponse(histLine)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, ErrNameNotFound
	}

	// Most recent transaction = last entry.
	latest := entries[len(entries)-1]

	// 4. Fetch the verbose transaction.
	if err := conn.writeRequest(reqCtx, "blockchain.transaction.get", []any{latest.TxHash, true}, next()); err != nil {
		return nil, err
	}
	txLine, err := conn.readResponse(reqCtx)
	if err != nil {
		return nil, fmt.Errorf("nip05namecoin: read transaction response: %w", err)
	}

	// 5. Get the current block height so we can compute expiry.
	if err := conn.writeRequest(reqCtx, "blockchain.headers.subscribe", []any{}, next()); err != nil {
		return nil, err
	}
	headerLine, _ := conn.readResponse(reqCtx)
	currentHeight := parseBlockHeight(headerLine)

	if currentHeight > 0 && latest.Height > 0 {
		if currentHeight-latest.Height >= NameExpireDepth {
			return nil, ErrNameExpired
		}
	}

	result, err := parseNameFromTransaction(identifier, latest.TxHash, latest.Height, txLine)
	if err != nil {
		return nil, err
	}
	if result != nil && currentHeight > 0 && latest.Height > 0 {
		result.ExpiresIn = NameExpireDepth - (currentHeight - latest.Height)
	}
	return result, nil
}

// NameShowWithFallback tries each server in order until one returns a
// result. Definitive errors (NameNotFound, NameExpired) are propagated
// immediately; transport errors are swallowed and the next server is
// tried.
func (c *ElectrumClient) NameShowWithFallback(ctx context.Context, identifier string, servers []ElectrumxServer) (*NameShowResult, error) {
	var lastErr error
	for _, srv := range servers {
		result, err := c.NameShow(ctx, identifier, srv)
		if err == nil {
			return result, nil
		}
		if errors.Is(err, ErrNameNotFound) || errors.Is(err, ErrNameExpired) {
			return nil, err
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = ErrServersUnreachable
	}
	return nil, fmt.Errorf("%w: last error: %v", ErrServersUnreachable, lastErr)
}

// dial picks the right transport implementation based on the server
// configuration and returns a ready-to-use rpcConn. HAVEN supports
// TCP and TCP+TLS only.
func (c *ElectrumClient) dial(ctx context.Context, server ElectrumxServer) (rpcConn, error) {
	switch effectiveTransport(server) {
	case TransportTCPTLS, TransportTCP:
		return c.dialTCP(ctx, server)
	default:
		return nil, fmt.Errorf("nip05namecoin: unsupported transport %d (HAVEN ships TCP/TCP+TLS only)", server.Transport)
	}
}

// dialTCP opens a raw TCP connection to the server, upgrading to TLS
// when the effective transport is TCPTLS. Honours both context
// cancellation and our connect timeout, whichever fires first.
func (c *ElectrumClient) dialTCP(ctx context.Context, server ElectrumxServer) (rpcConn, error) {
	dialer := &net.Dialer{Timeout: c.ConnectTimeout}
	address := net.JoinHostPort(server.Host, strconv.Itoa(server.Port))

	raw, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("nip05namecoin: dial %s: %w", address, err)
	}

	var nc net.Conn = raw
	if effectiveTransport(server) == TransportTCPTLS {
		cfg := tlsConfigFor(server)
		tlsConn := tls.Client(raw, cfg)

		if deadline, ok := ctx.Deadline(); ok {
			_ = tlsConn.SetDeadline(deadline)
		} else {
			_ = tlsConn.SetDeadline(time.Now().Add(c.ConnectTimeout))
		}
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, fmt.Errorf("nip05namecoin: TLS handshake with %s: %w", address, err)
		}
		_ = tlsConn.SetDeadline(time.Time{})
		nc = tlsConn
	}

	_ = nc.SetDeadline(time.Now().Add(c.ReadTimeout))
	return &tcpRPCConn{conn: nc, reader: bufio.NewReader(nc)}, nil
}

// tcpRPCConn is the newline-delimited JSON-RPC transport over raw
// TCP or TCP+TLS.
type tcpRPCConn struct {
	conn   net.Conn
	reader *bufio.Reader
}

func (t *tcpRPCConn) writeRequest(ctx context.Context, method string, params []any, id int64) error {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("nip05namecoin: marshal rpc request: %w", err)
	}
	encoded = append(encoded, '\n')
	if deadline, ok := ctx.Deadline(); ok {
		_ = t.conn.SetWriteDeadline(deadline)
	}
	if _, err := t.conn.Write(encoded); err != nil {
		return fmt.Errorf("nip05namecoin: write rpc request: %w", err)
	}
	return nil
}

func (t *tcpRPCConn) readResponse(ctx context.Context) (string, error) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = t.conn.SetReadDeadline(deadline)
	}
	line, err := t.reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return line, nil
}

func (t *tcpRPCConn) close() error {
	return t.conn.Close()
}

// historyEntry is one row of `blockchain.scripthash.get_history`.
type historyEntry struct {
	TxHash string
	Height int
}

// parseHistoryResponse extracts (tx_hash, height) pairs from a
// get_history response. An error response (non-null `error` field)
// yields an empty slice so callers can treat it as "no data".
func parseHistoryResponse(raw string) ([]historyEntry, error) {
	var envelope struct {
		Result []struct {
			TxHash string `json:"tx_hash"`
			Height int    `json:"height"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return nil, fmt.Errorf("nip05namecoin: parse history response: %w", err)
	}
	if len(envelope.Error) > 0 && !isJSONNull(envelope.Error) {
		return nil, nil
	}
	out := make([]historyEntry, 0, len(envelope.Result))
	for _, e := range envelope.Result {
		out = append(out, historyEntry{TxHash: e.TxHash, Height: e.Height})
	}
	return out, nil
}

// parseBlockHeight extracts `result.height` from a
// `blockchain.headers.subscribe` response, or returns 0 on any error.
func parseBlockHeight(raw string) int {
	if raw == "" {
		return 0
	}
	var envelope struct {
		Result struct {
			Height int `json:"height"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return 0
	}
	return envelope.Result.Height
}

// parseNameFromTransaction walks the verbose transaction's vouts
// looking for a NAME_UPDATE output that matches `identifier`.
// Returns (nil, nil) if no matching output exists.
func parseNameFromTransaction(identifier, txHash string, height int, raw string) (*NameShowResult, error) {
	var envelope struct {
		Result struct {
			Vout []struct {
				ScriptPubKey struct {
					Hex string `json:"hex"`
				} `json:"scriptPubKey"`
			} `json:"vout"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return nil, fmt.Errorf("nip05namecoin: parse transaction response: %w", err)
	}
	if len(envelope.Error) > 0 && !isJSONNull(envelope.Error) {
		return nil, nil
	}
	for _, vout := range envelope.Result.Vout {
		hexScript := vout.ScriptPubKey.Hex
		if !strings.HasPrefix(hexScript, "53") {
			continue
		}
		scriptBytes, err := hex.DecodeString(hexScript)
		if err != nil {
			continue
		}
		name, value, err := parseNameScript(scriptBytes)
		if err != nil {
			continue
		}
		if name == identifier {
			return &NameShowResult{
				Name:   name,
				Value:  value,
				TxID:   txHash,
				Height: height,
			}, nil
		}
	}
	return nil, nil
}

func isJSONNull(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}
