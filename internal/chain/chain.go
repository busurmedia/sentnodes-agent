// Package chain builds, signs, and broadcasts a Cosmos bank MsgSend over the
// node's CometBFT RPC endpoints (abci_query for account/balance,
// broadcast_tx_sync to submit). The transaction protobuf is hand-encoded so the
// agent needs no cosmos-sdk dependency, and it reuses the RPC addresses already
// in the node config (no separate LCD endpoint required).
package chain

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/busurmedia/sentnodes-agent/internal/keys"
)

const signModeDirect = 1

type Client struct {
	http      *http.Client
	endpoints []string
}

// New takes CometBFT RPC endpoints, tried in order until one responds.
func New(endpoints []string) *Client {
	clean := make([]string, 0, len(endpoints))
	for _, e := range endpoints {
		if e = strings.TrimRight(strings.TrimSpace(e), "/"); e != "" {
			clean = append(clean, e)
		}
	}
	return &Client{http: &http.Client{Timeout: 20 * time.Second}, endpoints: clean}
}

// Account returns the account number and sequence for addr.
func (c *Client) Account(ctx context.Context, addr string) (number, sequence uint64, err error) {
	req := &pb{}
	req.str(1, addr) // QueryAccountRequest{ address }
	val, err := c.abciQuery(ctx, "/cosmos.auth.v1beta1.Query/Account", req.b)
	if err != nil {
		return 0, 0, err
	}
	// QueryAccountResponse{ account: Any } -> Any{ value: BaseAccount }
	base := pbBytesField(pbBytesField(val, 1), 2)
	if base == nil {
		return 0, 0, fmt.Errorf("could not parse account (vesting/module accounts are unsupported)")
	}
	// BaseAccount{ account_number=3, sequence=4 } (proto3 omits zero values).
	return pbVarintField(base, 3), pbVarintField(base, 4), nil
}

// Balance returns the balance of denom for addr.
func (c *Client) Balance(ctx context.Context, addr, denom string) (uint64, error) {
	req := &pb{}
	req.str(1, addr) // QueryBalanceRequest{ address, denom }
	req.str(2, denom)
	val, err := c.abciQuery(ctx, "/cosmos.bank.v1beta1.Query/Balance", req.b)
	if err != nil {
		return 0, err
	}
	// QueryBalanceResponse{ balance: Coin } -> Coin{ amount=2 (string) }
	amount := string(pbBytesField(pbBytesField(val, 1), 2))
	if amount == "" {
		return 0, nil
	}
	v, _ := strconv.ParseUint(amount, 10, 64)
	return v, nil
}

// Broadcast submits tx bytes via broadcast_tx_sync and returns the tx hash.
func (c *Client) Broadcast(ctx context.Context, txBytes []byte) (string, error) {
	raw, err := c.call(ctx, "/broadcast_tx_sync?tx=0x"+hex.EncodeToString(txBytes))
	if err != nil {
		return "", err
	}
	var res struct {
		Code      int    `json:"code"`
		Log       string `json:"log"`
		Hash      string `json:"hash"`
		Codespace string `json:"codespace"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", err
	}
	if res.Code != 0 {
		return res.Hash, fmt.Errorf("tx rejected (code %d, %s): %s", res.Code, res.Codespace, res.Log)
	}
	return res.Hash, nil
}

func (c *Client) abciQuery(ctx context.Context, path string, data []byte) ([]byte, error) {
	q := "/abci_query?path=" + url.QueryEscape("\""+path+"\"") + "&data=0x" + hex.EncodeToString(data)
	raw, err := c.call(ctx, q)
	if err != nil {
		return nil, err
	}
	var res struct {
		Response struct {
			Code  int    `json:"code"`
			Log   string `json:"log"`
			Value string `json:"value"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	if res.Response.Code != 0 {
		return nil, fmt.Errorf("query %s failed (code %d): %s", path, res.Response.Code, res.Response.Log)
	}
	return base64.StdEncoding.DecodeString(res.Response.Value)
}

// call issues a CometBFT RPC GET and returns the "result" object, trying each
// endpoint until one succeeds.
func (c *Client) call(ctx context.Context, pathAndQuery string) (json.RawMessage, error) {
	var lastErr error
	for _, ep := range c.endpoints {
		req, _ := http.NewRequestWithContext(ctx, "GET", ep+pathAndQuery, nil)
		req.Header.Set("Accept", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		var env struct {
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
				Data    string `json:"data"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			lastErr = fmt.Errorf("%s: bad response: %s", ep, strings.TrimSpace(string(body)))
			continue
		}
		if env.Error != nil {
			lastErr = fmt.Errorf("%s: rpc error: %s %s", ep, env.Error.Message, env.Error.Data)
			continue
		}
		return env.Result, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no RPC endpoints configured")
	}
	return nil, lastErr
}

// BuildSignedSend builds a signed (SIGN_MODE_DIRECT) bank MsgSend transaction.
func BuildSignedSend(acct *keys.Account, chainID, to, denom string, amount, fee, gas, accountNumber, sequence uint64, memo string) []byte {
	coin := func(d string, amt uint64) []byte {
		w := &pb{}
		w.str(1, d)
		w.str(2, strconv.FormatUint(amt, 10))
		return w.b
	}

	// cosmos.bank.v1beta1.MsgSend
	msg := &pb{}
	msg.str(1, acct.OperatorAddr())
	msg.str(2, to)
	msg.bytesField(3, coin(denom, amount))
	msgAny := anyMsg("/cosmos.bank.v1beta1.MsgSend", msg.b)

	// cosmos.tx.v1beta1.TxBody
	body := &pb{}
	body.bytesField(1, msgAny)
	if memo != "" {
		body.str(2, memo)
	}
	bodyBytes := body.b

	// secp256k1 pubkey Any
	pk := &pb{}
	pk.bytesField(1, acct.PubKeyBytes())
	pkAny := anyMsg("/cosmos.crypto.secp256k1.PubKey", pk.b)

	// ModeInfo{ Single{ mode } }
	single := &pb{}
	single.uint64Field(1, signModeDirect)
	modeInfo := &pb{}
	modeInfo.bytesField(1, single.b)

	// SignerInfo
	si := &pb{}
	si.bytesField(1, pkAny)
	si.bytesField(2, modeInfo.b)
	si.uint64Field(3, sequence)

	// Fee
	feeMsg := &pb{}
	feeMsg.bytesField(1, coin(denom, fee))
	feeMsg.uint64Field(2, gas)

	// AuthInfo
	ai := &pb{}
	ai.bytesField(1, si.b)
	ai.bytesField(2, feeMsg.b)
	authInfoBytes := ai.b

	// SignDoc -> signature
	sd := &pb{}
	sd.bytesField(1, bodyBytes)
	sd.bytesField(2, authInfoBytes)
	sd.str(3, chainID)
	sd.uint64Field(4, accountNumber)
	sig := acct.Sign(sd.b)

	// TxRaw
	raw := &pb{}
	raw.bytesField(1, bodyBytes)
	raw.bytesField(2, authInfoBytes)
	raw.bytesField(3, sig)
	return raw.b
}

// --- minimal protobuf writer ---

type pb struct{ b []byte }

func (w *pb) uvarint(v uint64) {
	for v >= 0x80 {
		w.b = append(w.b, byte(v)|0x80)
		v >>= 7
	}
	w.b = append(w.b, byte(v))
}
func (w *pb) tag(field, wire int) { w.uvarint(uint64(field)<<3 | uint64(wire)) }
func (w *pb) bytesField(field int, val []byte) {
	w.tag(field, 2)
	w.uvarint(uint64(len(val)))
	w.b = append(w.b, val...)
}
func (w *pb) str(field int, s string)         { w.bytesField(field, []byte(s)) }
func (w *pb) uint64Field(field int, v uint64) { w.tag(field, 0); w.uvarint(v) }

func anyMsg(typeURL string, value []byte) []byte {
	w := &pb{}
	w.str(1, typeURL)
	w.bytesField(2, value)
	return w.b
}

// --- minimal protobuf reader ---

func pbScan(buf []byte, fn func(field, wire int, val []byte, v uint64) bool) {
	i := 0
	for i < len(buf) {
		tag, n := binary.Uvarint(buf[i:])
		if n <= 0 {
			return
		}
		i += n
		field, wire := int(tag>>3), int(tag&7)
		switch wire {
		case 0:
			v, n := binary.Uvarint(buf[i:])
			if n <= 0 {
				return
			}
			i += n
			if !fn(field, 0, nil, v) {
				return
			}
		case 1:
			if i+8 > len(buf) {
				return
			}
			i += 8
		case 5:
			if i+4 > len(buf) {
				return
			}
			i += 4
		case 2:
			l, n := binary.Uvarint(buf[i:])
			if n <= 0 {
				return
			}
			i += n
			if i+int(l) > len(buf) {
				return
			}
			if !fn(field, 2, buf[i:i+int(l)], 0) {
				return
			}
			i += int(l)
		default:
			return
		}
	}
}

func pbBytesField(buf []byte, num int) []byte {
	var out []byte
	pbScan(buf, func(f, w int, val []byte, _ uint64) bool {
		if f == num && w == 2 {
			out = val
			return false
		}
		return true
	})
	return out
}

func pbVarintField(buf []byte, num int) uint64 {
	var out uint64
	pbScan(buf, func(f, w int, _ []byte, v uint64) bool {
		if f == num && w == 0 {
			out = v
			return false
		}
		return true
	})
	return out
}
