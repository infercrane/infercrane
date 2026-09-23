package acceleratorworker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFileReceiptStorePersistsAndProtectsIdempotency(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "receipts")
	store, err := NewFileReceiptStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	wanted := Receipt{RequestDigest: "sha256:one", Response: json.RawMessage(`{"ok":true}`)}
	if err = store.Put(context.Background(), "operation-1", wanted); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewFileReceiptStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := restarted.Get(context.Background(), "operation-1")
	if err != nil || !found || got.RequestDigest != wanted.RequestDigest || string(got.Response) != string(wanted.Response) {
		t.Fatalf("receipt=%+v found=%v err=%v", got, found, err)
	}
	if err = restarted.Put(context.Background(), "operation-1", Receipt{RequestDigest: "sha256:two", Response: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("idempotency collision accepted")
	}
}

func TestFileReceiptStoreRejectsBroadDirectoryPermissions(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "receipts")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileReceiptStore(directory); err == nil {
		t.Fatal("broad receipt directory accepted")
	}
}
