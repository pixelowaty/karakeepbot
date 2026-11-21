package filevalidator

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPDFValidator verifies that PDFValidator accepts only files whose
// content starts with the PDF signature, regardless of the file name.
func TestPDFValidator(t *testing.T) {
	validPDF := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\ntrailer<</Size 1/Root 1 0 R>>\nstartxref\n9\n%%EOF")

	tests := []struct {
		name         string
		fileName     string
		content      []byte
		expectError  bool
		expectedMime string
	}{
		{
			name:         "Valid minimal PDF",
			fileName:     "document.pdf",
			content:      validPDF,
			expectError:  false,
			expectedMime: "application/pdf",
		},
		{
			name:         "Valid PDF with uppercase extension",
			fileName:     "document.PDF",
			content:      validPDF,
			expectError:  false,
			expectedMime: "application/pdf",
		},
		{
			name:         "PDF signature with different version header",
			fileName:     "document.pdf",
			content:      []byte("%PDF-2.0 rest of the file"),
			expectError:  false,
			expectedMime: "application/pdf",
		},
		{
			name:        "Text content disguised as PDF",
			fileName:    "document.pdf",
			content:     []byte("this is not a PDF file"),
			expectError: true,
		},
		{
			name:        "Empty file",
			fileName:    "empty.pdf",
			content:     []byte{},
			expectError: true,
		},
		{
			name:        "File shorter than the PDF signature",
			fileName:    "truncated.pdf",
			content:     []byte("%PF"),
			expectError: true,
		},
		{
			name:        "Leading bytes before the signature",
			fileName:    "corrupted.pdf",
			content:     []byte("\x00\x00%PDF-1.4"),
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filePath := filepath.Join(t.TempDir(), tt.fileName)
			if err := os.WriteFile(filePath, tt.content, 0600); err != nil {
				t.Fatalf("Failed to write test file: %v", err)
			}

			file, err := os.Open(filePath)
			if err != nil {
				t.Fatalf("Failed to open test file: %v", err)
			}
			defer func() { _ = file.Close() }()

			mimeType, err := PDFValidator(file)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected an error, but got none (mime type: %q)", mimeType)
				}
				return
			}

			if err != nil {
				t.Fatalf("Expected no error, but got: %v", err)
			}
			if mimeType != tt.expectedMime {
				t.Errorf("Expected MIME type %q, but got %q", tt.expectedMime, mimeType)
			}
		})
	}
}

// TestPDFValidator_SeeksToStart verifies that PDFValidator re-reads the file
// from the beginning even if the file offset is not at the start.
func TestPDFValidator_SeeksToStart(t *testing.T) {
	validPDF := []byte("%PDF-1.4\n%%EOF")

	filePath := filepath.Join(t.TempDir(), "document.pdf")
	if err := os.WriteFile(filePath, validPDF, 0600); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	file, err := os.Open(filePath)
	if err != nil {
		t.Fatalf("Failed to open test file: %v", err)
	}
	defer func() { _ = file.Close() }()

	// Move the file offset away from the start.
	if _, err := file.Seek(1, 0); err != nil {
		t.Fatalf("Failed to seek: %v", err)
	}

	mimeType, err := PDFValidator(file)
	if err != nil {
		t.Fatalf("Expected no error, but got: %v", err)
	}
	if mimeType != "application/pdf" {
		t.Errorf("Expected MIME type application/pdf, but got %q", mimeType)
	}
}
