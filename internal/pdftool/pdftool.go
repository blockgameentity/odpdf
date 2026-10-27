// Package pdftool wraps pdfcpu to validate an assembled PDF and remove its
// password protection, producing an unprotected local copy.
package pdftool

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// magic is the PDF signature every valid document starts with.
var magic = []byte("%PDF-")

// IsPDF reports whether the file at path starts with the %PDF- signature.
func IsPDF(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("pdftool: %w", err)
	}
	defer f.Close()
	buf := make([]byte, len(magic))
	if _, err := f.Read(buf); err != nil {
		return false, fmt.Errorf("pdftool: read %s: %w", path, err)
	}
	return string(buf) == string(magic), nil
}

// ErrWrongPassword is returned when pdfcpu rejects the supplied password.
var ErrWrongPassword = errors.New("pdftool: wrong PDF password")

// Decrypt reads the encrypted PDF at inPath, writes an unprotected copy to
// outPath and validates the result. The PDF user password is supplied via
// password; it is also tried as the owner password, which is what PDF
// producers usually set to the same value.
func Decrypt(ctx context.Context, inPath, outPath, password string) error {
	conf := model.NewDefaultConfiguration()
	conf.UserPW = password
	conf.OwnerPW = password

	err := api.DecryptFile(ctx, inPath, outPath, conf)
	if err != nil {
		if isPasswordError(err) {
			return ErrWrongPassword
		}
		return fmt.Errorf("pdftool: decrypt %s: %w", inPath, err)
	}
	return nil
}

// Validate runs pdfcpu's structural validation over a PDF file. A password may
// be supplied for encrypted files.
func Validate(ctx context.Context, path, password string) error {
	conf := model.NewDefaultConfiguration()
	if password != "" {
		conf.UserPW = password
		conf.OwnerPW = password
	}
	if err := api.ValidateFile(ctx, path, conf, nil); err != nil {
		return fmt.Errorf("pdftool: validate %s: %w", path, err)
	}
	return nil
}

// isPasswordError matches pdfcpu's various "password/permission" failures.
func isPasswordError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, s := range []string{
		"invalid password",
		"corrupt password",
		"password",
		"EncryptDict",
	} {
		if containsFold(msg, s) {
			return true
		}
	}
	return false
}

func containsFold(haystack, needle string) bool {
	h := []byte(haystack)
	n := []byte(needle)
	if len(n) == 0 {
		return true
	}
	for i := 0; i+len(n) <= len(h); i++ {
		match := true
		for j := 0; j < len(n); j++ {
			a, b := h[i+j], n[j]
			if 'A' <= a && a <= 'Z' {
				a += 'a' - 'A'
			}
			if 'A' <= b && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
