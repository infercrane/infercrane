package workercredential

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
)

const Version = "tenant-hmac-v1"

func Derive(master, tenant, deploymentID string) string {
	if master == "" || tenant == "" || deploymentID == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(master))
	_, _ = mac.Write([]byte("infercrane.worker.v1\x00" + tenant + "\x00" + deploymentID))
	return "icw_" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
