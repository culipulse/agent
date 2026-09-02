package main

import (
	"bytes"
	"encoding/hex"
	"regexp"
	"strings"
)

// decodePayload turns the (optional) transport spec into the raw bytes to send. Encoding "hex"
// decodes the payload from hex; anything else (incl. empty) treats it as utf8. Errors on bad hex.
// Shared by udpProbe and tcpProbe (banner checks).
func decodePayload(t *Transport) ([]byte, error) {
	if t == nil {
		return []byte{}, nil
	}
	if strings.ToLower(t.PayloadEncoding) == "hex" {
		if t.Payload == "" {
			return []byte{}, nil
		}
		return hex.DecodeString(t.Payload)
	}
	return []byte(t.Payload), nil
}

// matchReply reports whether `reply` satisfies the transport's expect clause. No expect ⇒ any reply
// passes. "regex" mode (ExpectMode) compiles Expect and matches it against the reply as text — so
// payloadEncoding doesn't constrain it. Otherwise "contains": a hex substring (when encoding=hex)
// or a utf8 substring. The server pre-validates Expect; the regex.Compile here is defense-in-depth.
func matchReply(t *Transport, reply []byte) (bool, error) {
	if t == nil || t.Expect == "" {
		return true, nil
	}
	if strings.ToLower(t.ExpectMode) == "regex" {
		re, err := regexp.Compile(t.Expect)
		if err != nil {
			return false, err
		}
		return re.Match(reply), nil
	}
	if strings.ToLower(t.PayloadEncoding) == "hex" {
		want, err := hex.DecodeString(t.Expect)
		if err != nil {
			return false, err
		}
		return bytes.Contains(reply, want), nil
	}
	return bytes.Contains(reply, []byte(t.Expect)), nil
}
