package opscontext

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"
)

// ConversationAnchor returns an opaque digest of the complete ordered history.
// It is continuation metadata, not semantic memory: persisting it lets a resumed
// SDK session receive only newly completed outer turns instead of another copy of
// history already present in that SDK transcript.
func ConversationAnchor(messages []ConversationMessage) string {
	if len(messages) == 0 {
		return ""
	}
	state := make([]byte, sha256.Size)
	for _, message := range messages {
		state = nextConversationDigest(state, message)
	}
	return hex.EncodeToString(state)
}

// ConversationAfterAnchor returns the suffix not represented by anchor. Empty
// anchor denotes the boundary before the first message. If a bounded history has
// already discarded the anchored prefix, ok is false so the caller can start a
// fresh SDK session with the complete currently available snapshot.
func ConversationAfterAnchor(messages []ConversationMessage, anchor string) (suffix []ConversationMessage, ok bool) {
	anchor = strings.TrimSpace(anchor)
	if anchor == "" {
		return append([]ConversationMessage(nil), messages...), true
	}
	if len(anchor) != sha256.Size*2 {
		return nil, false
	}
	if _, err := hex.DecodeString(anchor); err != nil {
		return nil, false
	}
	state := make([]byte, sha256.Size)
	for i, message := range messages {
		state = nextConversationDigest(state, message)
		if hex.EncodeToString(state) == anchor {
			return append([]ConversationMessage(nil), messages[i+1:]...), true
		}
	}
	return nil, false
}

func nextConversationDigest(previous []byte, message ConversationMessage) []byte {
	h := sha256.New()
	h.Write(previous)
	writeConversationDigestPart(h, message.Role)
	writeConversationDigestPart(h, message.Content)
	return h.Sum(nil)
}

type digestWriter interface {
	Write([]byte) (int, error)
}

func writeConversationDigestPart(dst digestWriter, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = dst.Write(size[:])
	_, _ = dst.Write([]byte(value))
}
