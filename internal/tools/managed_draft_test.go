package tools

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kacperkwapisz/mail-mcp/internal/config"
)

func TestManagedDraftMarkerAuthenticatesAndRejectsRecipients(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	marker, err := newManagedDraftMarker(key)
	if err != nil {
		t.Fatalf("newManagedDraftMarker: %v", err)
	}
	raw, err := buildManagedDraft(&config.Account{FromAddress: "me@example.com"}, "Subject", "Body", "", marker)
	if err != nil {
		t.Fatalf("buildManagedDraft: %v", err)
	}
	got, ok, err := validManagedDraftMarker(raw, key)
	if err != nil || !ok || got != marker {
		t.Fatalf("validManagedDraftMarker = (%q, %v, %v), want (%q, true, nil)", got, ok, err, marker)
	}

	withRecipient := bytes.Replace(raw, []byte("Subject: Subject"), []byte("To: person@example.com\r\nSubject: Subject"), 1)
	if _, ok, err := validManagedDraftMarker(withRecipient, key); err != nil || ok {
		t.Fatalf("recipient-bearing draft = ok %v, err %v; want rejected", ok, err)
	}

	wrongKey := bytes.Repeat([]byte{0x99}, 32)
	if _, ok, err := validManagedDraftMarker(raw, wrongKey); err != nil || ok {
		t.Fatalf("wrong-key draft = ok %v, err %v; want rejected", ok, err)
	}
}

func TestManagedDraftRevisionChangesOnHumanEdit(t *testing.T) {
	key := bytes.Repeat([]byte{0x24}, 32)
	marker, err := newManagedDraftMarker(key)
	if err != nil {
		t.Fatalf("newManagedDraftMarker: %v", err)
	}
	raw, err := buildManagedDraft(&config.Account{FromAddress: "me@example.com"}, "Subject", "Body", "", marker)
	if err != nil {
		t.Fatalf("buildManagedDraft: %v", err)
	}
	if managedDraftRevision(raw) == managedDraftRevision([]byte(strings.Replace(string(raw), "Body", "Human edit", 1))) {
		t.Fatal("revision did not change after draft content changed")
	}
}
