package optimizationreport

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/infercrane/infercrane/internal/passport"
)

// Why a report carries no signature. A signature is never made up: a report
// without one says which of these applies.
const (
	UnsignedRunNotFinished = "run_not_finished"
	UnsignedNoSigningKey   = "signing_key_not_configured"
)

const (
	// VerifyCommand checks a report saved with --output json.
	VerifyCommand = "infercrane optimize report verify FILE"
	digestMember  = `,"report_digest":`
	emptiedDigest = `,"report_digest":""}`
)

// Signature is the Ed25519 signature of a finished run's report. Digest is the
// report's own report_digest, so the signature covers every byte of the
// report's JSON.
type Signature = passport.DigestSignature

// Document is a report as the control plane serves it and the CLI saves it:
// the report and its signature, or why it has none.
type Document struct {
	Report         Report     `json:"report"`
	Signature      *Signature `json:"signature"`
	UnsignedReason string     `json:"unsigned_reason,omitempty"`
}

// Sign seals the report of a finished run with the control plane's signing
// key. The report has no timestamps and Ed25519 is deterministic, so the same
// evidence always gives the same signature and nothing is stored. A run that
// has not finished, or a control plane without a key, gets no signature and
// the reason instead.
func Sign(report Report, key ed25519.PrivateKey) (Document, error) {
	document := Document{Report: report}
	switch {
	case !report.Finished:
		document.UnsignedReason = UnsignedRunNotFinished
	case len(key) == 0:
		document.UnsignedReason = UnsignedNoSigningKey
	default:
		signature, err := passport.SignDigest(report.ReportDigest, key)
		if err != nil {
			return Document{}, fmt.Errorf("sign optimization report: %w", err)
		}
		document.Signature = &signature
	}
	return document, nil
}

var errNotSaved = errors.New("not a saved optimization report: expected the JSON of infercrane optimize report RUN_ID --output json")

// Verified is what Verify established about a saved report.
type Verified struct {
	CampaignID string
	Digest     string
	KeyID      string
	// Pinned is true when the caller named the key it trusts and the report
	// was signed by it. Otherwise the signature only proves the report is
	// unchanged since the key named by KeyID signed it.
	Pinned bool
}

// Verify checks a saved report document: the schema version, the digest
// recomputed from the report's own JSON bytes, the Ed25519 signature over that
// digest, and that the key ID names the signing key. Whitespace does not
// matter; any other change to the report does. With trusted set, the report
// must also have been signed by that key.
func Verify(saved []byte, trusted ed25519.PublicKey) (Verified, error) {
	var document struct {
		Report         json.RawMessage `json:"report"`
		Signature      *Signature      `json:"signature"`
		UnsignedReason string          `json:"unsigned_reason"`
	}
	decoder := json.NewDecoder(bytes.NewReader(saved))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil || len(document.Report) == 0 {
		return Verified{}, errNotSaved
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return Verified{}, errNotSaved
	}
	var head struct {
		SchemaVersion string `json:"schema_version"`
		CampaignID    string `json:"campaign_id"`
	}
	if err := json.Unmarshal(document.Report, &head); err != nil {
		return Verified{}, fmt.Errorf("read report: %w", err)
	}
	if head.SchemaVersion != SchemaVersion {
		return Verified{}, fmt.Errorf("unsupported report schema %q: this CLI verifies %s", head.SchemaVersion, SchemaVersion)
	}
	if document.Signature == nil {
		return Verified{}, fmt.Errorf("the report is not signed: %s", UnsignedText(document.UnsignedReason))
	}
	computed, stated, err := contentDigest(document.Report)
	if err != nil {
		return Verified{}, err
	}
	if computed != stated {
		return Verified{}, fmt.Errorf("the report was changed: its content digest is %s, not the %s it states", computed, stated)
	}
	if document.Signature.Digest != computed {
		return Verified{}, fmt.Errorf("the signature is for report %s, not this report (%s)", document.Signature.Digest, computed)
	}
	if err = passport.VerifyDigest(*document.Signature); err != nil {
		return Verified{}, fmt.Errorf("the signature is not valid: %w", err)
	}
	verified := Verified{CampaignID: head.CampaignID, Digest: computed, KeyID: document.Signature.KeyID}
	if len(trusted) > 0 {
		if passport.KeyID(trusted) != document.Signature.KeyID {
			return Verified{}, fmt.Errorf("signed by key %s, not the key you trust (%s)", document.Signature.KeyID, passport.KeyID(trusted))
		}
		signer, _ := passport.DecodePublicKey(document.Signature.PublicKey)
		if !signer.Equal(trusted) {
			return Verified{}, errors.New("signed by a different key with the same key ID as the key you trust")
		}
		verified.Pinned = true
	}
	return verified, nil
}

// contentDigest recomputes report_digest from the report's JSON bytes: the
// SHA-256 of the compact report with report_digest emptied. It works on the
// bytes, not on a decoded Report, so a field this build does not know, a
// renamed key, or a duplicate key all change the digest. report_digest is the
// last field of a report, as json.Marshal writes it.
func contentDigest(raw json.RawMessage) (computed, stated string, err error) {
	var compact bytes.Buffer
	if err = json.Compact(&compact, raw); err != nil {
		return "", "", fmt.Errorf("read report: %w", err)
	}
	body := compact.Bytes()
	at := bytes.LastIndex(body, []byte(digestMember))
	if at < 0 || !bytes.HasSuffix(body, []byte("}")) || json.Unmarshal(body[at+len(digestMember):len(body)-1], &stated) != nil {
		return "", "", errors.New("the report does not end with its report_digest, so its digest cannot be recomputed")
	}
	canonical := append(append([]byte{}, body[:at]...), emptiedDigest...)
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), stated, nil
}

// UnsignedText says in words why a report carries no signature.
func UnsignedText(reason string) string {
	switch reason {
	case UnsignedRunNotFinished:
		return "the run has not finished; its report is signed once it finishes"
	case UnsignedNoSigningKey:
		return "this control plane has no signing key (INFERCRANE_PASSPORT_SIGNING_KEY_FILE)"
	}
	return "the control plane sent no signature"
}

// signatureLine is the one line every rendering ends with: who signed the
// report and how to check it, or why it is not signed.
func signatureLine(d Document) string {
	if d.Signature == nil {
		return "Not signed: " + UnsignedText(d.UnsignedReason) + "."
	}
	return "Signed by key " + d.Signature.KeyID + " — verify with " + VerifyCommand + ", where FILE is this report saved with --output json."
}
