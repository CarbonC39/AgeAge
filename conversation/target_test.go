package conversation

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

func TestTargetBindingKeyIgnoresReplyAndSenderButIncludesOwner(t *testing.T) {
	base := Target{ChannelType: "matrix", ChannelID: "!room:example.org", ThreadID: "$root", OwnerID: "@owner:example.org"}
	variant := base
	variant.ReplyTo = "$reply"
	variant.SenderID = "@sender:example.org"
	variant.SenderName = "Sender"
	if base.BindingKey() != variant.BindingKey() {
		t.Fatal("reply and sender context changed the durable target key")
	}
	variant.OwnerID = "@other:example.org"
	if base.BindingKey() == variant.BindingKey() {
		t.Fatal("different owners shared a durable target key")
	}
}

func TestBindingKeySeparatesDelimiterLikeThreadAndChannelIDs(t *testing.T) {
	a := Target{ChannelType: "matrix", ChannelID: "room:t:thread", ThreadID: "tail"}
	b := Target{ChannelType: "matrix", ChannelID: "room", ThreadID: "t:thread:tail"}
	if a.BindingKey() == b.BindingKey() {
		t.Fatalf("delimiter-like IDs collided: %#v and %#v", a, b)
	}
}

func TestBindingKeyPreservesLegacyHashFormat(t *testing.T) {
	parts := []string{"matrix", "!room:example.org", "$thread", "@owner:example.org"}
	var encoded strings.Builder
	for _, part := range parts {
		fmt.Fprintf(&encoded, "%d:%s|", len(part), part)
	}
	sum := sha256.Sum256([]byte(encoded.String()))
	want := hex.EncodeToString(sum[:])
	if got := BindingKey(parts[0], parts[1], parts[2], parts[3]); got != want {
		t.Fatalf("BindingKey = %q, want legacy key %q", got, want)
	}
	if got := (Target{ChannelType: parts[0], ChannelID: parts[1], ThreadID: parts[2], OwnerID: parts[3]}).BindingKey(); got != want {
		t.Fatalf("Target.BindingKey = %q, want legacy key %q", got, want)
	}
}
