package encoding

import (
	"fmt"
	"strings"
)

// EncodeText encodes a byte slice as text with \xHH escapes for non-printable bytes.
func EncodeText(data []byte) string {
	var b strings.Builder
	for _, c := range data {
		switch {
		case c >= 0x20 && c <= 0x7E:
			b.WriteByte(c)
		case c == '\n':
			b.WriteString("\\n")
		case c == '\r':
			b.WriteString("\\r")
		case c == '\t':
			b.WriteString("\\t")
		default:
			fmt.Fprintf(&b, "\\x%02x", c)
		}
	}
	return b.String()
}

// DecodeText decodes a text string with \xHH, \n, \r, \t escapes back to bytes.
func DecodeText(s string) []byte {
	var result []byte
	i := 0
	for i < len(s) {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				result = append(result, '\n')
				i += 2
				continue
			case 'r':
				result = append(result, '\r')
				i += 2
				continue
			case 't':
				result = append(result, '\t')
				i += 2
				continue
			case 'x':
				if i+3 < len(s) {
					var b byte
					fmt.Sscanf(s[i+2:i+4], "%02x", &b)
					result = append(result, b)
					i += 4
					continue
				}
			case '\\':
				result = append(result, '\\')
				i += 2
				continue
			}
		}
		result = append(result, s[i])
		i++
	}
	return result
}

// HexDecode decodes a hex string to bytes. Returns an error for malformed hex.
func HexDecode(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("hex string has odd length %d", len(s))
	}
	result := make([]byte, 0, len(s)/2)
	for i := 0; i < len(s); i += 2 {
		var b byte
		if _, err := fmt.Sscanf(s[i:i+2], "%02x", &b); err != nil {
			return nil, fmt.Errorf("invalid hex at position %d: %w", i, err)
		}
		result = append(result, b)
	}
	return result, nil
}
