package tools

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	stdmail "net/mail"
	"strings"

	imap "github.com/emersion/go-imap/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kacperkwapisz/mail-mcp/internal/config"
	"github.com/kacperkwapisz/mail-mcp/internal/mailbox"
	"github.com/kacperkwapisz/mail-mcp/internal/msgid"
	"github.com/kacperkwapisz/mail-mcp/internal/send"
)

const managedDraftHeader = "X-MacMCP-Managed-Draft"
const managedDraftRevisionHeader = "X-MacMCP-Managed-Revision"

// managedDraftInput intentionally has no recipient, reply, threading, or
// attachment fields. A managed draft can be reviewed and addressed only in a
// human mail client, never sent through this MCP surface.
type managedDraftInput struct {
	accountInput
	Subject  string `json:"subject" jsonschema:"draft subject line, single line, no line breaks"`
	BodyText string `json:"body_text,omitempty" jsonschema:"plain text draft body; use separately from body_html"`
	BodyHTML string `json:"body_html,omitempty" jsonschema:"HTML draft body using real HTML tags; use separately from body_text"`
}

type managedDraftUpdateInput struct {
	messageInput
	Revision string `json:"revision" jsonschema:"exact revision returned when this managed draft was created or last updated"`
	Subject  string `json:"subject" jsonschema:"replacement draft subject line, single line, no line breaks"`
	BodyText string `json:"body_text,omitempty" jsonschema:"replacement plain text draft body; use separately from body_html"`
	BodyHTML string `json:"body_html,omitempty" jsonschema:"replacement HTML draft body using real HTML tags; use separately from body_text"`
}

type managedDraftReadOutput struct {
	Revision string `json:"revision" jsonschema:"saved revision required by update_managed_draft; unchanged until the draft is updated through this server"`
}

type managedDraftOutput struct {
	Summary         string `json:"summary" jsonschema:"one-line description of the result"`
	Status          string `json:"status" jsonschema:"saved or saved_unretired when the replacement exists but the previous draft could not be hidden"`
	AccountID       string `json:"account_id" jsonschema:"account the draft belongs to"`
	Folder          string `json:"folder" jsonschema:"Drafts folder containing the managed draft"`
	MessageID       string `json:"message_id" jsonschema:"opaque handle for the managed draft; pass it unchanged to update_managed_draft"`
	Revision        string `json:"revision" jsonschema:"exact revision required for the next update; a human edit invalidates it"`
	RetiredPrevious bool   `json:"retired_previous,omitempty" jsonschema:"true when the previous revision was hidden after the replacement was saved"`
}

func (s *Server) registerManagedDrafts(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "create_managed_draft",
		Title: "Create a recipient-free managed draft",
		Description: "Create a locally managed draft without any recipient fields. It cannot send email. " +
			"Only drafts carrying this server-authenticated marker can later be updated.",
		Annotations: managedDraftTool(),
	}, s.createManagedDraft)

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "update_managed_draft",
		Title: "Update a recipient-free managed draft",
		Description: "Replace a locally managed recipient-free draft using its exact current revision. " +
			"The operation refuses drafts without the authenticated marker, drafts edited by a human, and drafts with recipient fields.",
		Annotations: managedDraftTool(),
	}, s.updateManagedDraft)
}

func managedDraftTool() *mcp.ToolAnnotations {
	no := false
	return &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &no, IdempotentHint: false}
}

func (s *Server) createManagedDraft(ctx context.Context, _ *mcp.CallToolRequest, in managedDraftInput) (*mcp.CallToolResult, managedDraftOutput, error) {
	acc, err := s.resolveAccount(in.AccountID)
	if err != nil {
		return nil, managedDraftOutput{}, err
	}
	key, err := s.managedDraftKey()
	if err != nil {
		return nil, managedDraftOutput{}, err
	}
	marker, err := newManagedDraftMarker(key)
	if err != nil {
		return nil, managedDraftOutput{}, err
	}
	return s.saveManagedDraft(ctx, acc.ID, "", in.Subject, in.BodyText, in.BodyHTML, marker)
}

func (s *Server) updateManagedDraft(ctx context.Context, _ *mcp.CallToolRequest, in managedDraftUpdateInput) (*mcp.CallToolResult, managedDraftOutput, error) {
	id, acc, err := s.resolveMessage(in.MessageID)
	if err != nil {
		return nil, managedDraftOutput{}, err
	}
	key, err := s.managedDraftKey()
	if err != nil {
		return nil, managedDraftOutput{}, err
	}

	var marker string
	err = s.withSession(ctx, acc, func(sess *mailbox.Session) error {
		drafts, err := sess.FolderForRole("drafts")
		if err != nil {
			return err
		}
		if id.Mailbox != firstNonEmpty(drafts, "Drafts") {
			return fmt.Errorf("managed drafts can only be updated from the Drafts folder")
		}
		if err := sess.SelectFor(id, false); err != nil {
			return err
		}
		raw, err := sess.FetchRaw(imap.UID(id.UID), true)
		if err != nil {
			return err
		}
		storedRevision, hasStoredRevision := managedDraftStoredRevision(raw)
		if !hasStoredRevision || !hmac.Equal([]byte(in.Revision), []byte(storedRevision)) {
			return fmt.Errorf("managed draft revision metadata is missing or stale; do not overwrite a human edit")
		}
		if !hmac.Equal([]byte(in.Revision), []byte(managedDraftRevision(raw))) {
			return fmt.Errorf("managed draft revision is stale; do not overwrite a human edit")
		}
		var valid bool
		marker, valid, err = validManagedDraftMarker(raw, key)
		if err != nil {
			return err
		}
		if !valid {
			return fmt.Errorf("managed draft marker is missing or was not created by this local MacMCP runtime")
		}
		return nil
	})
	if err != nil {
		return nil, managedDraftOutput{}, err
	}
	return s.saveManagedDraft(ctx, acc.ID, in.MessageID, in.Subject, in.BodyText, in.BodyHTML, marker)
}

func (s *Server) saveManagedDraft(
	ctx context.Context,
	accountID, previousHandle, subject, bodyText, bodyHTML, marker string,
) (*mcp.CallToolResult, managedDraftOutput, error) {
	acc, err := s.resolveAccount(accountID)
	if err != nil {
		return nil, managedDraftOutput{}, err
	}
	raw, err := buildManagedDraft(acc, subject, bodyText, bodyHTML, marker)
	if err != nil {
		return nil, managedDraftOutput{}, err
	}

	out := managedDraftOutput{AccountID: acc.ID, Status: "saved"}
	err = s.withSession(ctx, acc, func(sess *mailbox.Session) error {
		folder, err := sess.FolderForRole("drafts")
		if err != nil {
			return err
		}
		out.Folder = firstNonEmpty(folder, "Drafts")
		if !sess.SupportsStableAppendHandle() {
			return fmt.Errorf("managed drafts require IMAP UIDPLUS so a saved draft has a stable handle")
		}
		appended, err := sess.AppendWithResult(out.Folder, raw, []imap.Flag{imap.FlagDraft, imap.FlagSeen})
		if err != nil {
			return err
		}
		if appended.UID == 0 || appended.UIDValidity == 0 {
			return fmt.Errorf("IMAP server did not return a stable handle for the saved managed draft")
		}
		out.MessageID = msgid.Encode(acc.ID, out.Folder, appended.UIDValidity, uint32(appended.UID))
		out.Revision = managedDraftRevision(raw)

		if previousHandle == "" {
			return nil
		}
		previous, err := msgid.Parse(previousHandle)
		if err != nil {
			return err
		}
		if err := sess.SelectFor(previous, false); err != nil {
			out.Status = "saved_unretired"
			return nil
		}
		if err := sess.RetireDraft(imap.UID(previous.UID)); err != nil {
			out.Status = "saved_unretired"
			return nil
		}
		out.RetiredPrevious = true
		return nil
	})
	if err != nil {
		return nil, managedDraftOutput{}, err
	}
	if out.Status == "saved_unretired" {
		out.Summary = "replacement managed draft saved; the previous revision could not be hidden"
	} else if previousHandle == "" {
		out.Summary = "recipient-free managed draft saved"
	} else {
		out.Summary = "recipient-free managed draft updated"
	}
	return nil, out, nil
}

func buildManagedDraft(acc *config.Account, subject, bodyText, bodyHTML, marker string) ([]byte, error) {
	raw, err := buildManagedDraftRaw(acc, subject, bodyText, bodyHTML, marker, "")
	if err != nil {
		return nil, err
	}
	return withManagedDraftRevisionHeader(raw, managedDraftRevision(raw))
}

func buildManagedDraftRaw(acc *config.Account, subject, bodyText, bodyHTML, marker, revision string) ([]byte, error) {
	headers := map[string]string{managedDraftHeader: marker}
	if revision != "" {
		headers[managedDraftRevisionHeader] = revision
	}
	msg, err := send.Build(acc, &send.Composition{
		Subject:              subject,
		BodyText:             bodyText,
		BodyHTML:             bodyHTML,
		AllowEmptyRecipients: true,
		Headers:              headers,
	})
	if err != nil {
		return nil, err
	}
	var raw bytes.Buffer
	if _, err := msg.WriteTo(&raw); err != nil {
		return nil, fmt.Errorf("serialize managed draft: %w", err)
	}
	return raw.Bytes(), nil
}

func (s *Server) managedDraftKey() ([]byte, error) {
	encoded := strings.TrimSpace(s.cfg.ManagedDraftKey)
	if encoded == "" {
		return nil, fmt.Errorf("managed drafts are disabled: this local server has no managed draft key")
	}
	key, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("managed drafts are disabled: invalid managed draft key")
	}
	return key, nil
}

func newManagedDraftMarker(key []byte) (string, error) {
	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate managed draft marker: %w", err)
	}
	payload := "v1." + base64.RawURLEncoding.EncodeToString(nonce)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func validManagedDraftMarker(raw, key []byte) (string, bool, error) {
	message, err := stdmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return "", false, fmt.Errorf("read managed draft headers: %w", err)
	}
	if hasRecipientHeaders(message.Header) {
		return "", false, nil
	}
	marker := strings.TrimSpace(message.Header.Get(managedDraftHeader))
	parts := strings.Split(marker, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return "", false, nil
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", false, nil
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return "", false, nil
	}
	return marker, true, nil
}

func managedDraftReadInfo(raw, key []byte) *managedDraftReadOutput {
	if _, valid, err := validManagedDraftMarker(raw, key); err != nil || !valid {
		return nil
	}
	revision, valid := managedDraftStoredRevision(raw)
	if !valid {
		return nil
	}
	return &managedDraftReadOutput{Revision: revision}
}

func hasRecipientHeaders(header stdmail.Header) bool {
	for _, name := range []string{
		"To", "Cc", "Bcc", "Reply-To", "Resent-To", "Resent-Cc", "Resent-Bcc", "Resent-Reply-To",
	} {
		if strings.TrimSpace(header.Get(name)) != "" {
			return true
		}
	}
	return false
}

func managedDraftRevision(raw []byte) string {
	digest := sha256.Sum256(normalizeManagedDraftRaw(withoutManagedDraftRevisionHeader(raw)))
	return hex.EncodeToString(digest[:])
}

func normalizeManagedDraftRaw(raw []byte) []byte {
	if bytes.HasSuffix(raw, []byte("\r\n")) {
		return raw[:len(raw)-2]
	}
	if bytes.HasSuffix(raw, []byte("\n")) {
		return raw[:len(raw)-1]
	}
	return raw
}

func managedDraftStoredRevision(raw []byte) (string, bool) {
	message, err := stdmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return "", false
	}
	revision := strings.TrimSpace(message.Header.Get(managedDraftRevisionHeader))
	if len(revision) != sha256.Size*2 {
		return "", false
	}
	if _, err := hex.DecodeString(revision); err != nil {
		return "", false
	}
	return revision, true
}

func withManagedDraftRevisionHeader(raw []byte, revision string) ([]byte, error) {
	separator := []byte("\r\n\r\n")
	lineEnding := "\r\n"
	boundary := bytes.Index(raw, separator)
	if boundary < 0 {
		separator = []byte("\n\n")
		lineEnding = "\n"
		boundary = bytes.Index(raw, separator)
	}
	if boundary < 0 {
		return nil, fmt.Errorf("managed draft has no header/body separator")
	}

	line := []byte(lineEnding + managedDraftRevisionHeader + ": " + revision)
	out := make([]byte, 0, len(raw)+len(line))
	out = append(out, raw[:boundary]...)
	out = append(out, line...)
	out = append(out, raw[boundary:]...)
	return out, nil
}

func withoutManagedDraftRevisionHeader(raw []byte) []byte {
	separator := []byte("\r\n\r\n")
	lineEnding := "\r\n"
	separatorLength := len(separator)
	boundary := bytes.Index(raw, separator)
	if boundary < 0 {
		separator = []byte("\n\n")
		lineEnding = "\n"
		separatorLength = len(separator)
		boundary = bytes.Index(raw, separator)
	}
	if boundary < 0 {
		return raw
	}

	var header bytes.Buffer
	skipContinuation := false
	removedRevision := false
	for _, line := range bytes.SplitAfter(raw[:boundary], []byte(lineEnding)) {
		lineText := strings.TrimSuffix(string(line), lineEnding)
		if skipContinuation && (strings.HasPrefix(lineText, " ") || strings.HasPrefix(lineText, "\t")) {
			continue
		}
		skipContinuation = false
		if strings.HasPrefix(strings.ToLower(lineText), strings.ToLower(managedDraftRevisionHeader)+":") {
			skipContinuation = true
			removedRevision = true
			continue
		}
		header.Write(line)
	}
	headerBytes := header.Bytes()
	if removedRevision && bytes.HasSuffix(headerBytes, []byte(lineEnding)) {
		headerBytes = headerBytes[:len(headerBytes)-len(lineEnding)]
	}
	var result bytes.Buffer
	result.Write(headerBytes)
	result.Write(separator)
	result.Write(raw[boundary+separatorLength:])
	return result.Bytes()
}
