package backupfmt

import (
	"bytes"
	"io"
	"testing"
)

func TestBackupEncryption(t *testing.T) {
	plain := bytes.Repeat([]byte("polaris backup "), 20000) // several chunks
	var buf bytes.Buffer
	w, err := EncryptWriter(&buf, "pw")
	if err != nil {
		t.Fatal(err)
	}
	w.Write(plain)
	w.Close()
	if bytes.Contains(buf.Bytes(), []byte("polaris backup")) {
		t.Fatal("plaintext visible")
	}
	r, err := DecryptReader(bytes.NewReader(buf.Bytes()), "pw")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("roundtrip: %v (%d bytes)", err, len(got))
	}
	r, _ = DecryptReader(bytes.NewReader(buf.Bytes()), "nope")
	if _, err := io.ReadAll(r); err == nil {
		t.Fatal("wrong passphrase decrypted")
	}
	r, _ = DecryptReader(bytes.NewReader(buf.Bytes()[:buf.Len()-100]), "pw") // truncated
	if _, err := io.ReadAll(r); err == nil {
		t.Fatal("truncated archive accepted")
	}
}
