package bech32

import (
	"errors"
	"strings"
)

// IsValid reports whether addr is a valid bech32 string with the given HRP
// (e.g. IsValid("sent1...", "sent")): correct prefix, charset, and checksum.
func IsValid(addr, hrp string) bool {
	addr = strings.ToLower(strings.TrimSpace(addr))
	pos := strings.LastIndex(addr, "1")
	if pos < 1 || pos+7 > len(addr) {
		return false
	}
	if addr[:pos] != hrp {
		return false
	}
	data := make([]int, 0, len(addr)-pos-1)
	for _, ch := range addr[pos+1:] {
		idx := strings.IndexRune(charset, ch)
		if idx < 0 {
			return false
		}
		data = append(data, idx)
	}
	return polymod(append(hrpExpand(hrp), data...)) == 1
}

const charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

var gen = []int{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}

func polymod(values []int) int {
	chk := 1
	for _, v := range values {
		b := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ v
		for i := 0; i < 5; i++ {
			if (b>>i)&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

func hrpExpand(hrp string) []int {
	out := make([]int, 0, len(hrp)*2+1)
	for i := 0; i < len(hrp); i++ {
		out = append(out, int(hrp[i]>>5))
	}
	out = append(out, 0)
	for i := 0; i < len(hrp); i++ {
		out = append(out, int(hrp[i]&0x1f))
	}
	return out
}

func convertBits(data []byte, from, to uint, pad bool) ([]int, error) {
	acc, bits := 0, uint(0)
	maxv := (1 << to) - 1
	ret := []int{}
	for _, b := range data {
		value := int(b)
		if value>>from != 0 {
			return nil, errors.New("invalid data range")
		}
		acc = (acc << from) | value
		bits += from
		for bits >= to {
			bits -= to
			ret = append(ret, (acc>>bits)&maxv)
		}
	}
	if pad {
		if bits > 0 {
			ret = append(ret, (acc<<(to-bits))&maxv)
		}
	} else if bits >= from || (acc<<(to-bits))&maxv != 0 {
		return nil, errors.New("invalid padding")
	}
	return ret, nil
}

// Encode encodes raw bytes as a bech32 string with the given HRP (e.g. a 20-byte
// account hash -> "sent1...").
func Encode(hrp string, data []byte) (string, error) {
	conv, err := convertBits(data, 8, 5, true)
	if err != nil {
		return "", err
	}
	values := append(hrpExpand(hrp), conv...)
	values = append(values, 0, 0, 0, 0, 0, 0)
	pm := polymod(values) ^ 1
	out := hrp + "1"
	for _, d := range conv {
		out += string(charset[d])
	}
	for i := 0; i < 6; i++ {
		out += string(charset[(pm>>(5*(5-i)))&0x1f])
	}
	return out, nil
}
