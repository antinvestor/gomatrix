package gomatrix

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Constants for userID encoding/decoding.
const (
	ASCIILowerOffset  = 0x20 // ASCII offset to convert uppercase to lowercase (A-Z to a-z)
	MatrixIDSeparator = ":"  // Separator used in Matrix IDs between parts
	MatrixIDParts     = 2    // Number of parts expected in a Matrix user ID when split by ":"
	lowerhex          = "0123456789abcdef"
)

// encode the given byte using quoted-printable encoding (e.g "=2f")
// and writes it to the buffer
// See https://golang.org/src/mime/quotedprintable/writer.go
func encode(buf *bytes.Buffer, b byte) {
	buf.WriteByte('=')
	buf.WriteByte(lowerhex[b>>4])
	buf.WriteByte(lowerhex[b&0x0f])
}

// escape the given alpha character and writes it to the buffer.
func escape(buf *bytes.Buffer, b byte) {
	buf.WriteByte('_')
	if b >= 'A' && b <= 'Z' {
		buf.WriteByte(b + ASCIILowerOffset) // ASCII shift A-Z to a-z
	} else {
		buf.WriteByte(b)
	}
}

func shouldEncode(b byte) bool {
	return b != '-' && b != '.' && b != '_' && (b < '0' || (b > '9' && b < 'A') || (b > 'Z' && b < 'a') || b > 'z')
}

func shouldEscape(b byte) bool {
	return (b >= 'A' && b <= 'Z') || b == '_'
}

func isValidByte(b byte) bool {
	return isValidEscapedChar(b) || (b >= '0' && b <= '9') || b == '.' || b == '=' || b == '-'
}

func isValidEscapedChar(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z')
}

// EncodeUserLocalpart encodes the given string into Matrix-compliant user ID localpart form.
// See http://matrix.org/docs/spec/intro.html#mapping-from-other-character-sets
//
// This returns a string with only the characters "a-z0-9._=-". The uppercase range A-Z
// are encoded using leading underscores ("_"). Characters outside the aforementioned ranges
// (including literal underscores ("_") and equals ("=")) are encoded as UTF8 code points (NOT NCRs)
// and converted to lower-case hex with a leading "=". For example:
//
//	Alph@Bet_50up  => _alph=40_bet=5f50up
func EncodeUserLocalpart(str string) string {
	strBytes := []byte(str)
	var outputBuffer bytes.Buffer
	for _, b := range strBytes {
		switch {
		case shouldEncode(b):
			encode(&outputBuffer, b)
		case shouldEscape(b):
			escape(&outputBuffer, b)
		default:
			outputBuffer.WriteByte(b)
		}
	}
	return outputBuffer.String()
}

// processEscapedCharacter handles the processing of characters that were escaped with '_'
// and returns the appropriate character to add to the output buffer.
func processEscapedCharacter(strBytes []byte, i int) (byte, int, error) {
	if i >= len(strBytes) {
		return 0, i, errors.New("_ was the last character - it should be followed by something to escape")
	}

	// check the next byte is valid to escape
	if !isValidEscapedChar(strBytes[i]) {
		return 0, i, fmt.Errorf("cannot escape byte %v", strBytes[i])
	}

	// the next byte is a-z, so it may need to be shifted back up
	if strBytes[i] >= 'a' && strBytes[i] <= 'z' {
		return strBytes[i] - ASCIILowerOffset, i, nil // ASCII shift a-z to A-Z
	}

	// just copy the byte as-is
	return strBytes[i], i, nil
}

// processHexEncodedCharacter handles decoding quoted-printable encoded bytes (e.g., "=40" -> "@")
// and returns the decoded byte to add to the output buffer.
func processHexEncodedCharacter(strBytes []byte, i int) (byte, int, error) {
	if i+1 >= len(strBytes) {
		return 0, i, errors.New("= was not followed by two hex digits")
	}

	dst := make([]byte, 1)
	_, err := hex.Decode(dst, strBytes[i:i+2])
	if err != nil {
		return 0, i, err
	}

	// We consumed 2 bytes total
	return dst[0], i + 1, nil
}

// DecodeUserLocalpart decodes the given string back into the original input string.
// Returns an error if the given string is not a valid user ID localpart encoding.
// See http://matrix.org/docs/spec/intro.html#mapping-from-other-character-sets
//
// This decodes quoted-printable bytes back into UTF8, and unescapes casing. For
// example:
//
//	_alph=40_bet=5f50up  =>  Alph@Bet_50up
//
// Returns an error if the input string contains characters outside the
// range "a-z0-9._=-", has an invalid quote-printable byte (e.g. not hex), or has
// an invalid _ escaped byte (e.g. "_5").
func DecodeUserLocalpart(str string) (string, error) {
	if str == "" {
		return "", nil
	}

	var outputBuffer bytes.Buffer
	strBytes := []byte(str)

	for i := 0; i < len(strBytes); i++ {
		b := strBytes[i]
		if !isValidByte(b) {
			return "", fmt.Errorf("not a valid byte: %v", b)
		}

		var err error
		var decodedByte byte

		switch b {
		case '_':
			i++
			decodedByte, i, err = processEscapedCharacter(strBytes, i)
			if err != nil {
				return "", err
			}
			outputBuffer.WriteByte(decodedByte)

		case '=':
			i++
			decodedByte, i, err = processHexEncodedCharacter(strBytes, i)
			if err != nil {
				return "", err
			}
			outputBuffer.WriteByte(decodedByte)

		default:
			outputBuffer.WriteByte(b)
		}
	}

	return outputBuffer.String(), nil
}

// ExtractUserLocalpart extracts the localpart portion of a user ID.
// See http://matrix.org/docs/spec/intro.html#user-identifiers
func ExtractUserLocalpart(userID string) (string, error) {
	if len(userID) == 0 || userID[0] != '@' {
		return "", errors.New("user ID must start with '@'")
	}

	parts := strings.SplitN(userID, MatrixIDSeparator, MatrixIDParts)
	if len(parts) != MatrixIDParts {
		return "", errors.New("user ID missing ':' separator")
	}

	return parts[0][1:], nil // @foo:bar:8448 => [ "@foo", "bar:8448" ]
}
