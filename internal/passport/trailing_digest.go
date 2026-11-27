package passport

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"fmt"
)

// TrailingDigest recomputes the content digest a signed document states in
// its last member, field: the SHA-256 of the compact JSON with that member's
// value emptied. It works on the bytes, not on a decoded value, so a field
// the reader's build does not know, a renamed key, or a duplicate key all
// change the digest. Whitespace does not. stated is the digest the document
// itself claims; the two must be equal for the document to be unchanged.
func TrailingDigest(raw []byte, field string) (computed, stated string, err error) {
	var compact bytes.Buffer
	if err = json.Compact(&compact, raw); err != nil {
		return "", "", fmt.Errorf("read document: %w", err)
	}
	member := []byte(`,"` + field + `":`)
	body := compact.Bytes()
	at := bytes.LastIndex(body, member)
	if at < 0 || !bytes.HasSuffix(body, []byte("}")) || json.Unmarshal(body[at+len(member):len(body)-1], &stated) != nil {
		return "", "", fmt.Errorf("the document does not end with its %s, so its digest cannot be recomputed", field)
	}
	canonical := append(append(append([]byte{}, body[:at]...), member...), []byte(`""}`)...)
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), stated, nil
}
