package pdftool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// buildMinimalPDF returns a tiny valid one-page PDF with a correct xref.
func buildMinimalPDF() []byte {
	var b bytes.Buffer
	offsets := make([]int, 4)
	b.WriteString("%PDF-1.4\n")
	offsets[1] = b.Len()
	b.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	offsets[2] = b.Len()
	b.WriteString("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
	offsets[3] = b.Len()
	b.WriteString("3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>\nendobj\n")
	xref := b.Len()
	b.WriteString("xref\n0 4\n0000000000 65535 f \n")
	for i := 1; i <= 3; i++ {
		fmt.Fprintf(&b, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&b, "trailer\n<< /Size 4 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref)
	return b.Bytes()
}

// makeEncrypted writes a password-protected copy of a minimal PDF and
// returns its path.
func makeEncrypted(t *testing.T, password string) string {
	t.Helper()
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.pdf")
	enc := filepath.Join(dir, "enc.pdf")
	if err := os.WriteFile(plain, buildMinimalPDF(), 0o644); err != nil {
		t.Fatal(err)
	}
	conf := model.NewAESConfiguration(password, password, 256)
	if err := api.EncryptFile(context.Background(), plain, enc, conf); err != nil {
		t.Fatalf("encrypt fixture: %v", err)
	}
	return enc
}

func TestIsPDF(t *testing.T) {
	dir := t.TempDir()
	pdf := filepath.Join(dir, "x.pdf")
	if err := os.WriteFile(pdf, buildMinimalPDF(), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, err := IsPDF(pdf)
	if err != nil || !ok {
		t.Fatalf("IsPDF(valid) = %v, %v", ok, err)
	}

	bin := filepath.Join(dir, "x.bin")
	if err := os.WriteFile(bin, []byte{0x00, 0x01, 0x02}, 0o644); err != nil {
		t.Fatal(err)
	}
	ok, err = IsPDF(bin)
	if err != nil || ok {
		t.Fatalf("IsPDF(binary) = %v, %v", ok, err)
	}
}

func TestDecryptRemovesPassword(t *testing.T) {
	enc := makeEncrypted(t, "hunter2")
	out := filepath.Join(t.TempDir(), "out.pdf")

	if err := Decrypt(context.Background(), enc, out, "hunter2"); err != nil {
		t.Fatalf("Decrypt: %v", err)
	}

	ok, err := IsPDF(out)
	if err != nil || !ok {
		t.Fatalf("decrypted output is not a PDF: %v", err)
	}
	// The decrypted copy must validate without any password.
	if err := Validate(context.Background(), out, ""); err != nil {
		t.Fatalf("decrypted output failed validation: %v", err)
	}
}

func TestDecryptWrongPassword(t *testing.T) {
	enc := makeEncrypted(t, "hunter2")
	out := filepath.Join(t.TempDir(), "out.pdf")

	err := Decrypt(context.Background(), enc, out, "wrong")
	if err == nil {
		t.Fatal("expected error for wrong password")
	}
	if !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("err = %v, want ErrWrongPassword", err)
	}
}
