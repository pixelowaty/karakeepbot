package filevalidator

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
)

// pdfSignature is the magic header that every valid PDF file starts with.
var pdfSignature = []byte("%PDF-")

// PDFValidator checks whether the file content starts with the PDF signature
// ("%PDF-") and returns its MIME type. It only verifies the signature, not the
// full structural validity of the PDF document.
func PDFValidator(file *os.File) (string, error) {
	// Ensure we read the file from the beginning.
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}

	// Read only the leading bytes needed to check the PDF signature.
	buffer := make([]byte, len(pdfSignature))
	n, err := io.ReadFull(file, buffer)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", err
	}

	if n < len(pdfSignature) || !bytes.Equal(buffer[:n], pdfSignature) {
		return "", fmt.Errorf("unsupported file format: content is not a valid PDF")
	}

	return "application/pdf", nil
}
