package api

import (
	"bytes"
	"image"
	"image/png"
	"testing"
)

func TestValidateDocument(t *testing.T) {
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	if got, err := validateDocument(b.Bytes()); err != nil || got != "image/png" {
		t.Fatalf("PNG: %s %v", got, err)
	}
	tests := [][]byte{[]byte("<script>alert(1)</script>"), []byte("\x89PNG\r\n\x1a\ninvalid"), []byte("%PDF-1.7\ntruncated"), {}, bytes.Repeat([]byte{'x'}, maxDocumentBytes+1)}
	for i, v := range tests {
		if _, err := validateDocument(v); err == nil {
			t.Fatalf("accepted invalid document %d", i)
		}
	}
	if got, err := validateDocument([]byte("%PDF-1.4\n1 0 obj\n<<>>\nendobj\n%%EOF\n")); err != nil || got != "application/pdf" {
		t.Fatalf("PDF: %s %v", got, err)
	}
}
func TestDocumentFilename(t *testing.T) {
	cases := map[string]string{"../../secret.pdf": "secret.pdf", `C:\private\identity.png`: "identity.png", "hello\r\nworld.pdf": "helloworld.pdf", "": "document", "..": "document"}
	for in, want := range cases {
		if got := documentFilename(in); got != want {
			t.Fatalf("%q => %q want %q", in, got, want)
		}
	}
}
func TestDocumentScope(t *testing.T) {
	for _, r := range []string{"member", "teller"} {
		if canReadDocument(r, 1, 2) {
			t.Fatal("cross-owner allowed", r)
		}
	}
	if !canReadDocument("admin", 1, 2) {
		t.Fatal("admin denied")
	}
	if canReadDocument("committee", 1, 2) {
		t.Fatal("committee must require an explicit active assignment")
	}
	if !canReadDocument("member", 1, 1) {
		t.Fatal("owner denied")
	}
}

func TestValidateCollateralDetails(t *testing.T) {
	vehicle := map[string]any{"bpkb_number": "B-123", "plate_number": "DK 1234 AB", "brand": "Toyota", "year": 2022}
	if err := validateCollateralDetails("vehicle", vehicle); err != nil {
		t.Fatalf("valid vehicle rejected: %v", err)
	}
	if err := validateCollateralDetails("vehicle", map[string]any{"bpkb_number": "B-123", "plate_number": "DK 1234 AB", "brand": "Toyota", "year": 1800}); err == nil {
		t.Fatal("invalid vehicle year accepted")
	}
	property := map[string]any{"certificate_number": "SHM-1", "area_m2": 100, "location": "Singaraja", "njop": 500000000}
	if err := validateCollateralDetails("property", property); err != nil {
		t.Fatalf("valid property rejected: %v", err)
	}
	if err := validateCollateralDetails("property", map[string]any{"certificate_number": "SHM-1", "area_m2": 0, "location": "Singaraja", "njop": 500000000}); err == nil {
		t.Fatal("zero property area accepted")
	}
	if err := validateCollateralDetails("savings", map[string]any{"asset_id": 1, "locked_amount": 0}); err == nil {
		t.Fatal("zero lock amount accepted")
	}
}
