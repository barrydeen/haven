package nip05namecoin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// Namecoin script opcodes used by the name-index script and NAME_UPDATE
// outputs. Matches electrumx/lib/coins.py (Namecoin fork) and the Kotlin
// reference implementation.
const (
	opNameUpdate byte = 0x53 // OP_3, repurposed by Namecoin as OP_NAME_UPDATE
	op2Drop      byte = 0x6d
	opDrop       byte = 0x75
	opReturn     byte = 0x6a
	opPushData1  byte = 0x4c
	opPushData2  byte = 0x4d
	opPushData4  byte = 0x4e
)

// buildNameIndexScript constructs the canonical script used by the
// Namecoin ElectrumX fork to index names on-chain.
//
// Format:
//
//	OP_NAME_UPDATE <push(name)> <push(empty)> OP_2DROP OP_DROP OP_RETURN
//
// The resulting script's SHA-256 (reversed, hex-encoded) is the
// scripthash queried via `blockchain.scripthash.get_history`.
func buildNameIndexScript(nameBytes []byte) []byte {
	out := make([]byte, 0, 4+len(nameBytes)+4)
	out = append(out, opNameUpdate)
	out = append(out, pushData(nameBytes)...)
	out = append(out, pushData(nil)...)
	out = append(out, op2Drop, opDrop, opReturn)
	return out
}

// pushData returns the Bitcoin-style push-data encoding of `data`.
func pushData(data []byte) []byte {
	n := len(data)
	switch {
	case n < int(opPushData1): // 0x4c
		return append([]byte{byte(n)}, data...)
	case n <= 0xff:
		return append([]byte{opPushData1, byte(n)}, data...)
	default:
		hi := byte((n >> 8) & 0xff)
		lo := byte(n & 0xff)
		return append([]byte{opPushData2, lo, hi}, data...)
	}
}

// electrumScriptHash computes the Electrum scripthash: SHA-256 of the
// script, byte-reversed, then hex-encoded. This is the format expected
// by `blockchain.scripthash.get_history` and friends.
//
// Worked example: for "d/testls" the resulting scripthash is
// b519574e96740a4b3627674a0708e71a73e654a95117fc828b8e177a0579ab42.
func electrumScriptHash(script []byte) string {
	digest := sha256.Sum256(script)
	for i, j := 0, len(digest)-1; i < j; i, j = i+1, j-1 {
		digest[i], digest[j] = digest[j], digest[i]
	}
	return hex.EncodeToString(digest[:])
}

// parseNameScript extracts the name and value from a NAME_UPDATE output
// script. Layout:
//
//	OP_NAME_UPDATE <push(name)> <push(value)> OP_2DROP OP_DROP <address-script>
//
// We only care about the leading push-data pair; the address script
// portion is ignored.
func parseNameScript(script []byte) (name string, value string, err error) {
	if len(script) == 0 || script[0] != opNameUpdate {
		return "", "", errors.New("nip05namecoin: script is not a NAME_UPDATE")
	}
	pos := 1

	nameBytes, next, err := readPushData(script, pos)
	if err != nil {
		return "", "", err
	}
	pos = next

	valueBytes, _, err := readPushData(script, pos)
	if err != nil {
		return "", "", err
	}

	return string(nameBytes), string(valueBytes), nil
}

// readPushData decodes one push-data element starting at `pos` and
// returns the payload bytes and the next read position.
func readPushData(script []byte, pos int) ([]byte, int, error) {
	if pos >= len(script) {
		return nil, 0, errors.New("nip05namecoin: truncated script")
	}
	op := script[pos]

	switch {
	case op == 0x00:
		return []byte{}, pos + 1, nil

	case op < opPushData1:
		length := int(op)
		end := pos + 1 + length
		if end > len(script) {
			return nil, 0, errors.New("nip05namecoin: push length exceeds script")
		}
		return script[pos+1 : end], end, nil

	case op == opPushData1:
		if pos+2 > len(script) {
			return nil, 0, errors.New("nip05namecoin: truncated OP_PUSHDATA1")
		}
		length := int(script[pos+1])
		end := pos + 2 + length
		if end > len(script) {
			return nil, 0, errors.New("nip05namecoin: OP_PUSHDATA1 length exceeds script")
		}
		return script[pos+2 : end], end, nil

	case op == opPushData2:
		if pos+3 > len(script) {
			return nil, 0, errors.New("nip05namecoin: truncated OP_PUSHDATA2")
		}
		length := int(script[pos+1]) | int(script[pos+2])<<8
		end := pos + 3 + length
		if end > len(script) {
			return nil, 0, errors.New("nip05namecoin: OP_PUSHDATA2 length exceeds script")
		}
		return script[pos+3 : end], end, nil

	case op == opPushData4:
		if pos+5 > len(script) {
			return nil, 0, errors.New("nip05namecoin: truncated OP_PUSHDATA4")
		}
		length := int(script[pos+1]) |
			int(script[pos+2])<<8 |
			int(script[pos+3])<<16 |
			int(script[pos+4])<<24
		end := pos + 5 + length
		if end < 0 || end > len(script) {
			return nil, 0, errors.New("nip05namecoin: OP_PUSHDATA4 length exceeds script")
		}
		return script[pos+5 : end], end, nil

	default:
		return nil, 0, errors.New("nip05namecoin: unsupported push opcode")
	}
}
