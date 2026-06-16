// Package keys reads the operator key from the dvpn node's cosmos keyring (test
// backend) and signs enrollment challenges. It parses the keyring directly
// (99designs/keyring + decred secp256k1) to avoid depending on cosmos-sdk.
package keys

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"path/filepath"

	"github.com/99designs/keyring"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/ripemd160" //nolint:staticcheck // cosmos address derivation requires ripemd160

	"github.com/busurmedia/sentnodes-agent/internal/bech32"
)

// Identity is the public part of the operator key: its compressed pubkey and
// derived addresses. It holds no private key, so it is safe to keep for the
// whole process lifetime (the private key is wiped after each use - see Wipe).
type Identity struct {
	pubCompressed []byte
	operatorAddr  string
	nodeAddr      string
}

func (i Identity) OperatorAddr() string { return i.operatorAddr }
func (i Identity) NodeAddr() string     { return i.nodeAddr }
func (i Identity) PubKeyHex() string    { return hex.EncodeToString(i.pubCompressed) }
func (i Identity) PubKeyBytes() []byte  { return i.pubCompressed }

// Account is a short-lived signing handle: the public Identity plus the
// in-memory private key. Open it only to sign (enrollment challenge,
// withdrawals) and Wipe it immediately after, so the operator key is never
// resident for the process lifetime, long-lived callers keep only the Identity.
type Account struct {
	Identity
	priv *secp256k1.PrivateKey
}

// Open loads the key named fromName from the cosmos `test` keyring under
// nodeHome, deriving the operator (acctPrefix) and node (nodePrefix) addresses.
func Open(nodeHome, backend, name, acctPrefix, nodePrefix string) (*Account, error) {
	if backend != "test" {
		return nil, errors.New("only keyring backend 'test' is supported")
	}

	ring, err := keyring.Open(keyring.Config{
		AllowedBackends:  []keyring.BackendType{keyring.FileBackend},
		FileDir:          filepath.Join(nodeHome, "keyring-"+backend),
		FilePasswordFunc: func(string) (string, error) { return "test", nil },
	})
	if err != nil {
		return nil, err
	}

	item, err := ring.Get(name + ".info")
	if err != nil {
		return nil, err
	}

	keyBytes, err := extractPrivKey(item.Data)
	if err != nil {
		return nil, err
	}

	priv := secp256k1.PrivKeyFromBytes(keyBytes)
	pub := priv.PubKey().SerializeCompressed()
	for i := range keyBytes { // don't leave the raw key bytes lingering in memory
		keyBytes[i] = 0
	}

	sha := sha256.Sum256(pub)
	rmd := ripemd160.New()
	_, _ = rmd.Write(sha[:])
	accHash := rmd.Sum(nil) // 20 bytes

	opAddr, err := bech32.Encode(acctPrefix, accHash)
	if err != nil {
		return nil, err
	}
	ndAddr, err := bech32.Encode(nodePrefix, accHash)
	if err != nil {
		return nil, err
	}

	return &Account{
		Identity: Identity{pubCompressed: pub, operatorAddr: opAddr, nodeAddr: ndAddr},
		priv:     priv,
	}, nil
}

// Wipe zeroes the in-memory private key. After Wipe the Account must not be used
// to Sign; open a fresh Account when a signature is next needed.
func (a *Account) Wipe() {
	if a.priv != nil {
		a.priv.Zero()
		a.priv = nil
	}
}

// Sign signs the raw message bytes the cosmos way (ECDSA over sha256(msg),
// low-S, 64-byte r||s) and returns the raw signature.
func (a *Account) Sign(msg []byte) []byte {
	h := sha256.Sum256(msg)
	sig := ecdsa.SignCompact(a.priv, h[:], false) // [recoveryID, r(32), s(32)]
	return sig[1:]                                // drop recovery byte -> 64-byte r||s
}

// SignHex is Sign as a hex string (used for the enrollment challenge).
func (a *Account) SignHex(msg []byte) string {
	return hex.EncodeToString(a.Sign(msg))
}

// extractPrivKey pulls the 32-byte secp256k1 key out of a cosmos keyring Record:
//
//	Record.local(3).priv_key(1) = Any{ value(2) = secp256k1.PrivKey{ key(1) } }
func extractPrivKey(record []byte) ([]byte, error) {
	local, err := pbBytes(record, 3)
	if err != nil {
		return nil, errors.New("keyring record has no local key (ledger/offline not supported)")
	}
	privAny, err := pbBytes(local, 1)
	if err != nil {
		return nil, err
	}
	privVal, err := pbBytes(privAny, 2)
	if err != nil {
		return nil, err
	}
	key, err := pbBytes(privVal, 1)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("unexpected private key length")
	}
	return key, nil
}

// pbBytes returns the first length-delimited (wire type 2) field `want` in a
// protobuf message, skipping other fields.
func pbBytes(buf []byte, want int) ([]byte, error) {
	i := 0
	for i < len(buf) {
		tag, n := binary.Uvarint(buf[i:])
		if n <= 0 {
			return nil, errors.New("bad protobuf tag")
		}
		i += n
		field := int(tag >> 3)
		wire := int(tag & 7)
		switch wire {
		case 0:
			_, n := binary.Uvarint(buf[i:])
			if n <= 0 {
				return nil, errors.New("bad varint")
			}
			i += n
		case 1:
			i += 8
		case 5:
			i += 4
		case 2:
			l, n := binary.Uvarint(buf[i:])
			if n <= 0 {
				return nil, errors.New("bad length")
			}
			i += n
			end := i + int(l)
			if end > len(buf) {
				return nil, errors.New("length overrun")
			}
			val := buf[i:end]
			i = end
			if field == want {
				return val, nil
			}
		default:
			return nil, errors.New("unsupported wire type")
		}
	}
	return nil, errors.New("field not found")
}
