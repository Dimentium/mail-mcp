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

func TestManagedDraftReadInfoReturnsSavedRevisionAndRejectsLegacyDrafts(t *testing.T) {
	key := bytes.Repeat([]byte{0x51}, 32)
	marker, err := newManagedDraftMarker(key)
	if err != nil {
		t.Fatalf("newManagedDraftMarker: %v", err)
	}
	raw, err := buildManagedDraft(&config.Account{FromAddress: "me@example.com"}, "Subject", "Body", "", marker)
	if err != nil {
		t.Fatalf("buildManagedDraft: %v", err)
	}
	info := managedDraftReadInfo(raw, key)
	if info == nil || info.Revision != managedDraftRevision(raw) {
		t.Fatalf("managedDraftReadInfo = %#v, want the saved revision", info)
	}

	humanEdit := []byte(strings.Replace(string(raw), "Body", "Human edit", 1))
	humanInfo := managedDraftReadInfo(humanEdit, key)
	if humanInfo == nil || humanInfo.Revision != info.Revision {
		t.Fatalf("human edit changed the saved revision token: %#v", humanInfo)
	}
	if managedDraftRevision(humanEdit) == info.Revision {
		t.Fatal("human edit did not invalidate the saved revision")
	}

	legacy, err := buildManagedDraftRaw(&config.Account{FromAddress: "me@example.com"}, "Subject", "Body", "", marker, "")
	if err != nil {
		t.Fatalf("buildManagedDraftRaw: %v", err)
	}
	if info := managedDraftReadInfo(legacy, key); info != nil {
		t.Fatalf("legacy managed draft unexpectedly exposed revision: %#v", info)
	}
}
