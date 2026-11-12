package epub

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSyntheticPDFAndMOBI(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Test synthetic PDF parsing
	pdfPath := filepath.Join(tmpDir, "distributed_consensus.pdf")
	if err := createSyntheticPDF(pdfPath, "Distributed Consensus with Raft", "Diego Ongaro",
		"Chapter 1 Leader Election\nRaft achieves consensus via an elected leader and replicated log entries."); err != nil {
		t.Fatalf("createSyntheticPDF failed: %v", err)
	}

	pdfBook, err := ParseBookFile(pdfPath, 2000, 500, 80)
	if err != nil {
		t.Fatalf("ParseBookFile(PDF) failed: %v", err)
	}
	if pdfBook.Format != "pdf" {
		t.Errorf("expected format 'pdf', got %q", pdfBook.Format)
	}
	if len(pdfBook.Chunks) == 0 {
		t.Fatal("expected non-empty chunks for PDF book")
	}
	if !strings.Contains(pdfBook.Chunks[0].Text, "replicated log") {
		t.Errorf("expected PDF chunk text to contain 'replicated log', got %q", pdfBook.Chunks[0].Text)
	}

	// 2. Test synthetic MOBI parsing (PalmDOC + MOBI + EXTH)
	mobiPath := filepath.Join(tmpDir, "kernel_internals.mobi")
	if err := createSyntheticMOBI(mobiPath, "Linux Kernel Internals", "Robert Love",
		"<html><body><h1>Process Scheduling</h1><p>The Completely Fair Scheduler (CFS) models an ideal multitasking CPU.</p></body></html>"); err != nil {
		t.Fatalf("createSyntheticMOBI failed: %v", err)
	}

	mobiBook, err := ParseBookFile(mobiPath, 2000, 500, 80)
	if err != nil {
		t.Fatalf("ParseBookFile(MOBI) failed: %v", err)
	}
	if mobiBook.Format != "mobi" {
		t.Errorf("expected format 'mobi', got %q", mobiBook.Format)
	}
	if mobiBook.Title != "Linux Kernel Internals" {
		t.Errorf("expected MOBI title 'Linux Kernel Internals', got %q", mobiBook.Title)
	}
	if mobiBook.Author != "Robert Love" {
		t.Errorf("expected MOBI author 'Robert Love', got %q", mobiBook.Author)
	}
	if len(mobiBook.Chunks) == 0 || !strings.Contains(mobiBook.Chunks[0].Text, "Completely Fair Scheduler") {
		t.Fatalf("expected MOBI chunk text to contain 'Completely Fair Scheduler'")
	}
}

func createSyntheticPDF(filePath, title, author, bodyText string) error {
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	streamContent := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", bodyText)
	if _, err := zw.Write([]byte(streamContent)); err != nil {
		return err
	}
	zw.Close()

	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n")
	pdf.WriteString(fmt.Sprintf("1 0 obj\n<< /Title (%s) /Author (%s) /CreationDate (D:20240101) >>\nendobj\n", title, author))
	pdf.WriteString(fmt.Sprintf("2 0 obj\n<< /Length %d /Filter /FlateDecode >>\nstream\n", compressed.Len()))
	pdf.Write(compressed.Bytes())
	pdf.WriteString("\nendstream\nendobj\n%%EOF\n")
	return os.WriteFile(filePath, pdf.Bytes(), 0o600)
}

func createSyntheticMOBI(filePath, title, author, htmlBody string) error {
	// Build EXTH block with Author (type 100) and Title (type 503)
	var exthRecords bytes.Buffer
	writeEXTHRecord := func(recType uint32, val string) {
		b := []byte(val)
		recLen := uint32(8 + len(b))
		_ = binary.Write(&exthRecords, binary.BigEndian, recType)
		_ = binary.Write(&exthRecords, binary.BigEndian, recLen)
		exthRecords.Write(b)
	}
	writeEXTHRecord(100, author)
	writeEXTHRecord(503, title)

	var exth bytes.Buffer
	exth.WriteString("EXTH")
	exthLen := uint32(12 + exthRecords.Len())
	_ = binary.Write(&exth, binary.BigEndian, exthLen)
	_ = binary.Write(&exth, binary.BigEndian, uint32(2)) // 2 records
	exth.Write(exthRecords.Bytes())
	for exth.Len()%4 != 0 {
		exth.WriteByte(0)
	}

	// Build Record 0 (16-byte PalmDOC + 232-byte MOBI header + EXTH + FullName)
	mobiHeaderLen := uint32(232)
	rec0 := make([]byte, 16+int(mobiHeaderLen))
	binary.BigEndian.PutUint16(rec0[0:2], 1)                     // Compression = 1 (uncompressed)
	binary.BigEndian.PutUint32(rec0[4:8], uint32(len(htmlBody))) // Text length
	binary.BigEndian.PutUint16(rec0[8:10], 1)                    // 1 text record
	binary.BigEndian.PutUint16(rec0[10:12], 4096)                // Record size
	copy(rec0[16:20], "MOBI")
	binary.BigEndian.PutUint32(rec0[20:24], mobiHeaderLen)
	binary.BigEndian.PutUint32(rec0[28:32], 65001)  // UTF-8
	binary.BigEndian.PutUint32(rec0[128:132], 0x40) // EXTH flag

	rec0 = append(rec0, exth.Bytes()...)
	fullNameOffset := uint32(len(rec0))
	rec0 = append(rec0, []byte(title)...)
	binary.BigEndian.PutUint32(rec0[84:88], fullNameOffset)
	binary.BigEndian.PutUint32(rec0[88:92], uint32(len(title)))

	// PDB Header (78 bytes + 2 * 8 bytes record list = 94 bytes)
	header := make([]byte, 78+16)
	copy(header[0:31], title)
	copy(header[60:68], "BOOKMOBI")
	binary.BigEndian.PutUint16(header[76:78], 2) // 2 records (Record 0 + Record 1)

	rec0Offset := uint32(len(header))
	rec1Offset := rec0Offset + uint32(len(rec0))
	binary.BigEndian.PutUint32(header[78:82], rec0Offset)
	binary.BigEndian.PutUint32(header[86:90], rec1Offset)

	var out bytes.Buffer
	out.Write(header)
	out.Write(rec0)
	out.WriteString(htmlBody)
	return os.WriteFile(filePath, out.Bytes(), 0o600)
}
