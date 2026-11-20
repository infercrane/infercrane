package optimizationreport

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/passport"
)

func signingKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := passport.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func signedDocument(t *testing.T, report Report, key ed25519.PrivateKey) []byte {
	t.Helper()
	document, err := Sign(report, key)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestReportDigestIsRecomputedFromTheJSONBytes(t *testing.T) {
	kept := winnerFixture()
	kept.arms[0].state, kept.arms[1].state = optimizationcampaign.CandidateQualified, optimizationcampaign.CandidateCleaned
	simulated := winnerFixture()
	for index := range simulated.arms {
		simulated.arms[index].gpu = "SIMULATED-CPU"
	}
	for name, f := range map[string]fixture{"winner": winnerFixture(), "kept": kept, "simulated": simulated} {
		report := Assemble(f.evidence())
		encoded, _ := json.Marshal(report)
		computed, stated, err := contentDigest(encoded)
		if err != nil || computed != report.ReportDigest || stated != report.ReportDigest {
			t.Fatalf("%s: computed=%s stated=%s want %s err=%v", name, computed, stated, report.ReportDigest, err)
		}
	}
}

func TestSignedReportVerifiesUntouchedAndAfterReformatting(t *testing.T) {
	key := signingKey(t)
	report := Assemble(winnerFixture().evidence())
	saved := signedDocument(t, report, key)
	if again := signedDocument(t, report, key); !bytes.Equal(saved, again) {
		t.Fatal("signing the same report twice gave different documents")
	}
	verified, err := Verify(saved, nil)
	if err != nil {
		t.Fatal(err)
	}
	if verified.CampaignID != report.CampaignID || verified.Digest != report.ReportDigest || verified.KeyID != passport.KeyID(key.Public().(ed25519.PublicKey)) || verified.Pinned {
		t.Fatalf("verified = %+v", verified)
	}
	pinned, err := Verify(saved, key.Public().(ed25519.PublicKey))
	if err != nil || !pinned.Pinned {
		t.Fatalf("pinned = %+v err=%v", pinned, err)
	}
	// The CLI saves the document indented; whitespace is not content.
	var indented bytes.Buffer
	if err = json.Indent(&indented, saved, "", "  "); err != nil {
		t.Fatal(err)
	}
	if _, err = Verify(indented.Bytes(), nil); err != nil {
		t.Fatalf("indented report: %v", err)
	}
}

func TestVerifyFailsWhenOneByteOfTheReportChanges(t *testing.T) {
	saved := signedDocument(t, Assemble(winnerFixture().evidence()), signingKey(t))
	start := bytes.Index(saved, []byte(`{"report":`)) + len(`{"report":`)
	end := bytes.Index(saved, []byte(`,"signature":`))
	if start < len(`{"report":`) || end < start {
		t.Fatalf("unexpected document layout: %.120s", saved)
	}
	// Every byte of the head (schema, run) and the tail (report_digest), and
	// every 11th byte between them; checking all of them takes a minute under
	// the race detector.
	var positions []int
	for at := start; at < end; at++ {
		if at < start+96 || at >= end-96 || (at-start)%11 == 0 {
			positions = append(positions, at)
		}
	}
	const workers = 4
	for worker := range workers {
		t.Run("", func(t *testing.T) {
			t.Parallel()
			for index := worker; index < len(positions); index += workers {
				at := positions[index]
				changed := bytes.Clone(saved)
				changed[at] ^= 1
				if _, err := Verify(changed, nil); err == nil {
					t.Fatalf("verified after byte %d changed from %q to %q", at, saved[at], changed[at])
				}
			}
		})
	}
}

func TestVerifyRejectsTamperingOutsideTheReport(t *testing.T) {
	key := signingKey(t)
	report := Assemble(winnerFixture().evidence())
	document, err := Sign(report, key)
	if err != nil {
		t.Fatal(err)
	}
	other := signingKey(t)
	resigned, _ := Sign(report, other)
	changedReport := report
	changedReport.Outcome.Headline = "Roll out everything"
	changedReport.ReportDigest = digest(changedReport)
	forged, _ := Sign(changedReport, other)

	cases := map[string]func() (any, ed25519.PublicKey){
		"another key than the trusted one": func() (any, ed25519.PublicKey) {
			return resigned, key.Public().(ed25519.PublicKey)
		},
		"a changed report re-signed by another key": func() (any, ed25519.PublicKey) {
			return forged, key.Public().(ed25519.PublicKey)
		},
		"a signature for another report": func() (any, ed25519.PublicKey) {
			copied := document
			copied.Signature = forged.Signature
			return copied, nil
		},
		"a signature from another key under this key's ID": func() (any, ed25519.PublicKey) {
			copied, signature := document, *resigned.Signature
			signature.KeyID = document.Signature.KeyID
			copied.Signature = &signature
			return copied, nil
		},
		"a changed signature": func() (any, ed25519.PublicKey) {
			copied, signature := document, *document.Signature
			signature.Signature = resigned.Signature.Signature
			copied.Signature = &signature
			return copied, nil
		},
		"a removed signature": func() (any, ed25519.PublicKey) {
			copied := document
			copied.Signature = nil
			return copied, nil
		},
		"another schema version": func() (any, ed25519.PublicKey) {
			copied := document
			copied.Report.SchemaVersion = "infercrane.optimization-report/v0"
			return copied, nil
		},
		"an added top-level field": func() (any, ed25519.PublicKey) {
			return map[string]any{"report": document.Report, "signature": document.Signature, "note": "approved"}, nil
		},
	}
	for name, build := range cases {
		value, trusted := build()
		encoded, _ := json.Marshal(value)
		if _, err := Verify(encoded, trusted); err == nil {
			t.Errorf("%s verified", name)
		}
	}
	trailing := append(signedDocument(t, report, key), []byte(`{"report":{}}`)...)
	if _, err := Verify(trailing, nil); err == nil {
		t.Error("a document with trailing data verified")
	}
	// Re-signing with another key is detectable only against a trusted key;
	// the key ID always names who signed.
	encoded, _ := json.Marshal(forged)
	verified, err := Verify(encoded, nil)
	if err != nil || verified.KeyID == document.Signature.KeyID || verified.Pinned {
		t.Fatalf("unpinned verification of another key's report = %+v err=%v", verified, err)
	}
}

func TestOnlyAFinishedRunWithAKeyIsSigned(t *testing.T) {
	key := signingKey(t)
	running := winnerFixture()
	running.state = optimizationcampaign.CampaignRunning
	unfinished := Assemble(running.evidence())
	if unfinished.Finished {
		t.Fatal("running campaign reported as finished")
	}
	for name, test := range map[string]struct {
		report Report
		key    ed25519.PrivateKey
		reason string
	}{
		"unfinished run": {unfinished, key, UnsignedRunNotFinished},
		"no signing key": {Assemble(winnerFixture().evidence()), nil, UnsignedNoSigningKey},
	} {
		document, err := Sign(test.report, test.key)
		if err != nil || document.Signature != nil || document.UnsignedReason != test.reason {
			t.Fatalf("%s: document = %+v err=%v", name, document, err)
		}
		encoded, _ := json.Marshal(document)
		if !strings.Contains(string(encoded), `"signature":null,"unsigned_reason":"`+test.reason+`"`) {
			t.Fatalf("%s: signature is not an explicit null with its reason: %.200s", name, encoded[len(encoded)-200:])
		}
		if _, err = Verify(encoded, nil); err == nil || !strings.Contains(err.Error(), "not signed") {
			t.Fatalf("%s: verify err = %v", name, err)
		}
		if text := Text(document); !strings.Contains(text, "Not signed: "+UnsignedText(test.reason)) {
			t.Fatalf("%s: text does not say why it is unsigned:\n%s", name, text)
		}
	}
}

func TestRenderingsNameTheSigningKeyAndTheVerifyCommand(t *testing.T) {
	document, err := Sign(Assemble(winnerFixture().evidence()), signingKey(t))
	if err != nil {
		t.Fatal(err)
	}
	want := "Signed by key " + document.Signature.KeyID + " — verify with " + VerifyCommand
	page, err := HTML(document)
	if err != nil {
		t.Fatal(err)
	}
	for format, output := range map[string]string{"text": Text(document), "html": string(page)} {
		if !strings.Contains(output, want) {
			t.Fatalf("%s does not name the key and the verify command:\n%s", format, output)
		}
	}
}
