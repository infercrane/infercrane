package proofreport

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/infercrane/infercrane/internal/passport"
)

// Why a report carries no signature. A signature is never made up: a report
// without one says which of these applies.
const (
	UnsignedNotFinished  = "proof_not_finished"
	UnsignedNoSigningKey = "signing_key_not_configured"
)

// VerifyCommand checks a report saved with --output json.
const VerifyCommand = "infercrane report proof verify FILE"

// Signature is the Ed25519 signature over the report's content digest.
type Signature = passport.DigestSignature

// Document is the report as the control plane serves it and the CLI saves
// it: the report and its signature, or why it has none.
type Document struct {
	Report         Report     `json:"report"`
	Signature      *Signature `json:"signature"`
	UnsignedReason string     `json:"unsigned_reason,omitempty"`
}

// Sign seals a report whose runs have all finished with the control plane's
// signing key. The report carries no clock time of its own and Ed25519 is
// deterministic, so the same records always give the same signature and
// nothing is stored.
func Sign(report Report, key ed25519.PrivateKey) (Document, error) {
	document := Document{Report: report}
	switch {
	case !report.Finished:
		document.UnsignedReason = UnsignedNotFinished
	case len(key) == 0:
		document.UnsignedReason = UnsignedNoSigningKey
	default:
		signature, err := passport.SignDigest(report.ReportDigest, key)
		if err != nil {
			return Document{}, fmt.Errorf("sign proof report: %w", err)
		}
		document.Signature = &signature
	}
	return document, nil
}

// UnsignedText says in words why a report carries no signature.
func UnsignedText(reason string) string {
	switch reason {
	case UnsignedNotFinished:
		return "a run has not finished; the report is signed once every run has"
	case UnsignedNoSigningKey:
		return "this control plane has no signing key (INFERCRANE_PASSPORT_SIGNING_KEY_FILE)"
	}
	return "the control plane sent no signature"
}

var errNotSaved = errors.New("not a saved proof report: expected the JSON of infercrane report proof RUN... --output json")

// Verified is what Verify established about a saved report.
type Verified struct {
	Digest string
	KeyID  string
	Runs   []string
	// Pinned is true when the caller named the key it trusts and the report
	// was signed by it.
	Pinned bool
}

// Verify checks a saved report: the schema, the digest recomputed from the
// report's own JSON bytes, the Ed25519 signature over that digest, and, with
// trusted set, that the trusted key signed it. Whitespace does not matter; any
// other change does.
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
		Workloads     []struct {
			Runs []struct {
				CampaignID string `json:"campaign_id"`
			} `json:"runs"`
		} `json:"workloads"`
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
	computed, stated, err := passport.TrailingDigest(document.Report, "report_digest")
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
	verified := Verified{Digest: computed, KeyID: document.Signature.KeyID}
	for _, workload := range head.Workloads {
		for _, run := range workload.Runs {
			verified.Runs = append(verified.Runs, run.CampaignID)
		}
	}
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

// signatureLine is the line every rendering ends with.
func signatureLine(d Document) string {
	if d.Signature == nil {
		return "Not signed: " + UnsignedText(d.UnsignedReason) + "."
	}
	return "Signed by key " + d.Signature.KeyID + ". Verify with " + VerifyCommand + ", where FILE is this report saved with --output json."
}
