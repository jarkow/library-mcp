package epub

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// ErrSkipNonBookSidecar indicates a file (such as a Google Takeout JSON sidecar named *(1).pdf) should be silently skipped.
var ErrSkipNonBookSidecar = errors.New("skip non-book metadata sidecar file")

// IsSupportedBookExtension returns true if filePath has a supported ebook extension (.epub, .pdf, .mobi, .azw, .azw3).
func IsSupportedBookExtension(filePath string) bool {
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".epub", ".pdf", ".mobi", ".azw", ".azw3":
		return true
	default:
		return false
	}
}

// ValidateBookHeader verifies the file size, detects the book format (epub, pdf, mobi), and returns (fileHash, fileSize, format, error).
func ValidateBookHeader(filePath string) (string, int64, string, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return "", 0, "", err
	}
	if info.IsDir() {
		return "", 0, "", errors.New("path is a directory, not a book file")
	}
	if info.Size() < 8 {
		return "", 0, "", errors.New("file is too small to be a valid book")
	}
	if info.Size() > MaxEPUBFileSize {
		return "", 0, "", fmt.Errorf("file exceeds maximum allowed size of %d MB", MaxEPUBFileSize/(1024*1024))
	}

	f, err := os.Open(filePath)
	if err != nil {
		return "", 0, "", err
	}
	defer f.Close()

	head := make([]byte, 1024)
	n, err := f.Read(head)
	if err != nil && err != io.EOF {
		return "", 0, "", err
	}
	head = head[:n]

	trimmed := bytes.TrimSpace(head)
	if len(trimmed) > 0 && trimmed[0] == '{' && info.Size() < 128*1024 {
		return "", 0, "", ErrSkipNonBookSidecar
	}

	ext := strings.ToLower(filepath.Ext(filePath))
	format := ""
	switch {
	case len(head) >= 4 && bytes.Equal(head[:4], []byte{'P', 'K', 0x03, 0x04}):
		format = "epub"
	case bytes.Contains(head, []byte("%PDF-")):
		format = "pdf"
	case len(head) >= 68 && (string(head[60:68]) == "BOOKMOBI" || string(head[60:68]) == "TEXtREAd"):
		format = "mobi"
	case ext == ".mobi" || ext == ".azw" || ext == ".azw3":
		return "", 0, "", errors.New("invalid MOBI/AZW header signature")
	case ext == ".pdf":
		return "", 0, "", errors.New("invalid PDF header signature")
	default:
		return "", 0, "", errors.New("unsupported or invalid book archive signature")
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", 0, "", err
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, io.LimitReader(f, MaxEPUBFileSize)); err != nil {
		return "", 0, "", err
	}
	fileHash := hex.EncodeToString(hasher.Sum(nil))
	return fileHash, info.Size(), format, nil
}

// ParseBookFile dispatches parsing to the EPUB, PDF, or MOBI parser based on verified file format.
func ParseBookFile(filePath string, parentSize, childSize, childOverlap int) (*ParsedBook, error) {
	fileHash, fileSize, format, err := ValidateBookHeader(filePath)
	if err != nil {
		return nil, err
	}

	var book *ParsedBook
	switch format {
	case "epub":
		book, err = ParseFile(filePath, parentSize, childSize, childOverlap)
	case "pdf":
		book, err = parsePDFFile(filePath, fileHash, fileSize, parentSize, childSize, childOverlap)
	case "mobi":
		book, err = parseMOBIFile(filePath, fileHash, fileSize, parentSize, childSize, childOverlap)
	default:
		return nil, fmt.Errorf("unsupported book format: %s", format)
	}
	if err != nil {
		return nil, err
	}
	book.Format = format
	applySidecarMetadataIfMissing(filePath, book)
	inferAuthorTitleFromFilename(filePath, book)
	return book, nil
}

// --- MOBI / PalmDOC Parser ---

func parseMOBIFile(filePath, fileHash string, fileSize int64, parentSize, childSize, childOverlap int) (*ParsedBook, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	_ = fileSize
	if len(data) < 78 {
		return nil, errors.New("MOBI file too short")
	}

	numRecords := int(binary.BigEndian.Uint16(data[76:78]))
	if numRecords < 2 || 78+numRecords*8 > len(data) {
		return nil, errors.New("invalid MOBI PDB record table")
	}

	offsets := make([]uint32, numRecords)
	for i := 0; i < numRecords; i++ {
		pos := 78 + i*8
		offsets[i] = binary.BigEndian.Uint32(data[pos : pos+4])
	}

	recOffset := func(idx int) (int, int, bool) {
		if idx < 0 || idx >= numRecords {
			return 0, 0, false
		}
		start := int(offsets[idx])
		end := len(data)
		if idx+1 < numRecords {
			end = int(offsets[idx+1])
		}
		if start < 0 || end > len(data) || start >= end {
			return 0, 0, false
		}
		return start, end, true
	}

	r0Start, r0End, ok := recOffset(0)
	if !ok || r0End-r0Start < 16 {
		return nil, errors.New("invalid MOBI record 0 header")
	}
	rec0 := data[r0Start:r0End]

	compression := binary.BigEndian.Uint16(rec0[0:2])
	textRecordCount := int(binary.BigEndian.Uint16(rec0[8:10]))
	if textRecordCount <= 0 || textRecordCount >= numRecords {
		textRecordCount = numRecords - 1
	}

	pdbName := strings.TrimRight(string(data[:32]), "\x00 ")
	title := pdbName
	author := "Unknown Author"
	publisher := ""
	description := ""
	isbn := ""
	publishedYear := ""
	var subjects []string

	// Parse MOBI & EXTH headers if present
	if len(rec0) >= 132 && string(rec0[16:20]) == "MOBI" {
		mobiHeaderLen := int(binary.BigEndian.Uint32(rec0[20:24]))
		fullNameOffset := int(binary.BigEndian.Uint32(rec0[84:88]))
		fullNameLen := int(binary.BigEndian.Uint32(rec0[88:92]))
		if fullNameOffset > 0 && fullNameLen > 0 && fullNameOffset+fullNameLen <= len(rec0) {
			if t := strings.TrimSpace(string(rec0[fullNameOffset : fullNameOffset+fullNameLen])); t != "" {
				title = t
			}
		}

		exthFlags := binary.BigEndian.Uint32(rec0[128:132])
		exthStart := 16 + mobiHeaderLen
		if (exthFlags&0x40) != 0 && exthStart+12 <= len(rec0) && string(rec0[exthStart:exthStart+4]) == "EXTH" {
			exthCount := int(binary.BigEndian.Uint32(rec0[exthStart+8 : exthStart+12]))
			pos := exthStart + 12
			var authors []string
			for i := 0; i < exthCount && pos+8 <= len(rec0); i++ {
				recType := binary.BigEndian.Uint32(rec0[pos : pos+4])
				recLen := int(binary.BigEndian.Uint32(rec0[pos+4 : pos+8]))
				if recLen < 8 || pos+recLen > len(rec0) {
					break
				}
				val := strings.TrimSpace(string(rec0[pos+8 : pos+recLen]))
				pos += recLen
				if val == "" {
					continue
				}
				switch recType {
				case 100:
					authors = append(authors, val)
				case 101:
					if publisher == "" {
						publisher = val
					}
				case 103:
					if description == "" {
						description = stripHTMLTags(val)
					}
				case 104:
					if isbn == "" {
						isbn = extractISBN([]string{val})
					}
				case 105:
					subjects = append(subjects, val)
				case 106:
					if publishedYear == "" {
						publishedYear = extractYear([]string{val})
					}
				case 503:
					title = val
				}
			}
			if len(authors) > 0 {
				author = strings.Join(authors, ", ")
			}
		}
	}

	var rawBuf bytes.Buffer
	if compression == 1 || compression == 2 {
		for idx := 1; idx <= textRecordCount && idx < numRecords; idx++ {
			st, en, ok := recOffset(idx)
			if !ok {
				continue
			}
			chunk := data[st:en]
			if compression == 1 {
				rawBuf.Write(chunk)
			} else {
				rawBuf.Write(decompressPalmDOC(chunk))
			}
		}
	}

	htmlContent := rawBuf.String()
	if strings.TrimSpace(htmlContent) == "" {
		// Try external ebook-convert if HUFF/CDIC compressed
		if converted, convErr := convertWithCalibreToEPUB(filePath, parentSize, childSize, childOverlap); convErr == nil {
			converted.ID = fileHash[:16]
			converted.FileHash = fileHash
			converted.FileName = filepath.Base(filePath)
			converted.Format = "mobi"
			return converted, nil
		}
		return nil, errors.New("could not extract readable text from MOBI archive")
	}

	return buildParsedBookFromHTMLOrChapters(
		fileHash,
		filePath,
		"mobi",
		title,
		author,
		publisher,
		publishedYear,
		isbn,
		description,
		subjects,
		htmlContent,
		parentSize,
		childSize,
		childOverlap,
	)
}

func decompressPalmDOC(in []byte) []byte {
	out := make([]byte, 0, len(in)*2)
	i := 0
	for i < len(in) {
		b := in[i]
		i++
		switch {
		case b == 0x00 || (b >= 0x09 && b <= 0x7f):
			out = append(out, b)
		case b >= 0x01 && b <= 0x08:
			count := int(b)
			if i+count > len(in) {
				count = len(in) - i
			}
			out = append(out, in[i:i+count]...)
			i += count
		case b >= 0x80 && b <= 0xbf:
			if i >= len(in) {
				break
			}
			next := in[i]
			i++
			pair := (uint16(b) << 8) | uint16(next)
			dist := int((pair >> 3) & 0x07ff)
			length := int(pair&0x07) + 3
			if dist <= 0 || dist > len(out) {
				continue
			}
			for j := 0; j < length; j++ {
				out = append(out, out[len(out)-dist])
			}
		case b >= 0xc0:
			out = append(out, ' ', b^0x80)
		}
	}
	return out
}

func buildParsedBookFromHTMLOrChapters(
	fileHash, filePath, format, title, author, publisher, publishedYear, isbn, description string,
	subjects []string,
	htmlContent string,
	parentSize, childSize, childOverlap int,
) (*ParsedBook, error) {
	bookID := fileHash[:16]
	if strings.TrimSpace(title) == "" {
		title = strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))
	}
	if strings.TrimSpace(author) == "" {
		author = "Unknown Author"
	}

	pageBreaks := regexp.MustCompile(`(?i)<mbp:pagebreak\s*/?>`).Split(htmlContent, -1)
	if len(pageBreaks) <= 1 {
		pageBreaks = []string{htmlContent}
	}

	var chapters []ParsedChapter
	var allParents []ParentSection
	var allChunks []ParsedChunk
	globalParentIdx := 0
	globalChunkIdx := 0

	for _, rawPart := range pageBreaks {
		sections, fullText, inferredTitle := parseHTMLIntoSections(rawPart)
		if strings.TrimSpace(fullText) == "" {
			continue
		}
		chapIdx := len(chapters) + 1
		chapTitle := inferredTitle
		if chapTitle == "" {
			chapTitle = fmt.Sprintf("Chapter %d", chapIdx)
		}
		chapID := fmt.Sprintf("%s_ch%03d", bookID, chapIdx)
		chapters = append(chapters, ParsedChapter{
			ID:           chapID,
			BookID:       bookID,
			ChapterIndex: chapIdx,
			Title:        chapTitle,
			Href:         fmt.Sprintf("part_%d", chapIdx),
			Content:      fullText,
		})

		parents, children := buildHierarchy(
			bookID, chapID, title, author, chapTitle,
			sections, parentSize, childSize, childOverlap,
			&globalParentIdx, &globalChunkIdx,
		)
		allParents = append(allParents, parents...)
		allChunks = append(allChunks, children...)
	}

	if isbn == "" {
		for i := 0; i < len(chapters) && i < 3; i++ {
			if m := isbnTextRe.FindStringSubmatch(chapters[i].Content); len(m) >= 2 {
				cand := strings.NewReplacer("-", "", " ", "").Replace(m[1])
				if len(cand) == 10 || len(cand) == 13 {
					isbn = strings.ToUpper(cand)
					break
				}
			}
		}
	}

	return &ParsedBook{
		ID:            bookID,
		FileHash:      fileHash,
		FileName:      filepath.Base(filePath),
		Format:        format,
		Title:         title,
		Author:        author,
		Publisher:     publisher,
		PublishedYear: publishedYear,
		ISBN:          isbn,
		Language:      "en",
		Description:   description,
		Subjects:      subjects,
		Chapters:      chapters,
		Parents:       allParents,
		Chunks:        allChunks,
	}, nil
}

// --- PDF Parser (Poppler pdftotext + Pure-Go PDF Stream Extractor) ---

var (
	pdfInfoFieldRe = regexp.MustCompile(`(?i)/(Title|Author|Subject|Creator|CreationDate)\s*\(([^\)\\]*(?:\\.[^\)\\]*)*)\)`)
	pdfStreamRe    = regexp.MustCompile(`(?s)stream\r?\n(.*?)\r?\nendstream`)
	pdfTextParenRe = regexp.MustCompile(`\(([^\)\\]*(?:\\.[^\)\\]*)*)\)\s*(?:Tj|'|")?`)
	pdfTJArrayRe   = regexp.MustCompile(`(?s)\[(.*?)\]\s*TJ`)
)

func parsePDFFile(filePath, fileHash string, fileSize int64, parentSize, childSize, childOverlap int) (*ParsedBook, error) {
	_ = fileSize
	bookID := fileHash[:16]
	title := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))
	author := "Unknown Author"
	publishedYear := ""
	isbn := ""
	pageCount := 0

	// 1. Try pdfinfo if available for fast metadata & page count
	if infoBin := findBinary("pdfinfo", `C:\Program Files\Calibre2\app\bin\pdfinfo.exe`); infoBin != "" {
		if out, err := exec.Command(infoBin, filePath).Output(); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) != 2 {
					continue
				}
				k := strings.ToLower(strings.TrimSpace(parts[0]))
				v := strings.TrimSpace(parts[1])
				if v == "" {
					continue
				}
				switch k {
				case "title":
					if !strings.HasSuffix(strings.ToLower(v), ".pdf") && !strings.HasSuffix(strings.ToLower(v), ".doc") {
						title = v
					}
				case "author":
					author = strings.TrimRight(v, ";, ")
				case "pages":
					if n, nErr := strconv.Atoi(v); nErr == nil && n > 0 {
						pageCount = n
					}
				case "creationdate":
					if m := yearTextRe.FindString(v); m != "" {
						publishedYear = m
					}
				}
			}
		}
	}

	// 2. Extract page texts via pdftotext (if available) or pure-Go stream extractor
	var pages []string
	if pdfToTextBin := findBinary("pdftotext", `C:\Program Files\Calibre2\app\bin\pdftotext.exe`); pdfToTextBin != "" {
		if out, err := exec.Command(pdfToTextBin, "-enc", "UTF-8", "-q", filePath, "-").Output(); err == nil && len(bytes.TrimSpace(out)) > 0 {
			rawPages := strings.Split(string(out), "\f")
			for _, p := range rawPages {
				cleaned := cleanPDFPageText(p)
				if cleaned != "" {
					pages = append(pages, cleaned)
				}
			}
		}
	}

	if len(pages) == 0 {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, err
		}
		metaTitle, metaAuthor, metaYear, extractedPages := extractPDFPureGo(data)
		if metaTitle != "" && title == strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath)) {
			title = metaTitle
		}
		if metaAuthor != "" && author == "Unknown Author" {
			author = metaAuthor
		}
		if metaYear != "" && publishedYear == "" {
			publishedYear = metaYear
		}
		pages = extractedPages
	}

	if len(pages) == 0 {
		return nil, errors.New("no readable text found in PDF")
	}
	if pageCount == 0 {
		pageCount = len(pages)
	}

	// Group pages into logical chapters (either by detected Chapter heading or 10-page blocks)
	const pagesPerChapter = 10
	var chapters []ParsedChapter
	var allParents []ParentSection
	var allChunks []ParsedChunk
	globalParentIdx := 0
	globalChunkIdx := 0

	for start := 0; start < len(pages); start += pagesPerChapter {
		end := start + pagesPerChapter
		if end > len(pages) {
			end = len(pages)
		}
		chunkPages := pages[start:end]
		combinedText := strings.Join(chunkPages, "\n\n")
		if strings.TrimSpace(combinedText) == "" {
			continue
		}

		chapIdx := len(chapters) + 1
		chapTitle := detectPDFSectionHeading(chunkPages[0])
		if chapTitle == "" {
			if len(pages) == 1 {
				chapTitle = "Chapter 1"
			} else {
				chapTitle = fmt.Sprintf("Pages %d–%d", start+1, end)
			}
		}

		chapID := fmt.Sprintf("%s_ch%03d", bookID, chapIdx)
		chapters = append(chapters, ParsedChapter{
			ID:           chapID,
			BookID:       bookID,
			ChapterIndex: chapIdx,
			Title:        chapTitle,
			Href:         fmt.Sprintf("pages_%d_%d", start+1, end),
			Content:      combinedText,
		})

		secs := []sectionBlock{{heading: chapTitle, text: combinedText}}
		parents, children := buildHierarchy(
			bookID, chapID, title, author, chapTitle,
			secs, parentSize, childSize, childOverlap,
			&globalParentIdx, &globalChunkIdx,
		)
		allParents = append(allParents, parents...)
		allChunks = append(allChunks, children...)
	}

	if isbn == "" {
		for i := 0; i < len(chapters) && i < 3; i++ {
			if m := isbnTextRe.FindStringSubmatch(chapters[i].Content); len(m) >= 2 {
				cand := strings.NewReplacer("-", "", " ", "").Replace(m[1])
				if len(cand) == 10 || len(cand) == 13 {
					isbn = strings.ToUpper(cand)
					break
				}
			}
		}
	}

	return &ParsedBook{
		ID:            bookID,
		FileHash:      fileHash,
		FileName:      filepath.Base(filePath),
		Format:        "pdf",
		Title:         title,
		Author:        author,
		PublishedYear: publishedYear,
		ISBN:          isbn,
		PageCount:     pageCount,
		Language:      "en",
		Chapters:      chapters,
		Parents:       allParents,
		Chunks:        allChunks,
	}, nil
}

func extractPDFPureGo(data []byte) (title, author, year string, pages []string) {
	headerLimit := len(data)
	if headerLimit > 512*1024 {
		headerLimit = 512 * 1024
	}
	for _, m := range pdfInfoFieldRe.FindAllSubmatch(data[:headerLimit], -1) {
		key := strings.ToLower(string(m[1]))
		val := strings.TrimSpace(unescapePDFString(string(m[2])))
		if val == "" {
			continue
		}
		switch key {
		case "title":
			if title == "" {
				title = val
			}
		case "author":
			if author == "" {
				author = val
			}
		case "creationdate":
			if year == "" {
				year = extractYear([]string{val})
			}
		}
	}

	matches := pdfStreamRe.FindAllSubmatch(data, -1)
	var currentPage strings.Builder
	streamCount := 0

	for _, m := range matches {
		rawStream := m[1]
		decoded := rawStream
		if zr, err := zlib.NewReader(bytes.NewReader(rawStream)); err == nil {
			if b, readErr := io.ReadAll(io.LimitReader(zr, MaxEntryFileSize)); readErr == nil && len(b) > 0 {
				decoded = b
			}
			zr.Close()
		}

		text := extractTextFromPDFContentStream(decoded)
		if strings.TrimSpace(text) == "" {
			continue
		}
		if currentPage.Len() > 0 {
			currentPage.WriteString("\n\n")
		}
		currentPage.WriteString(text)
		streamCount++
		if streamCount%2 == 0 || currentPage.Len() >= 2500 {
			pages = append(pages, strings.TrimSpace(currentPage.String()))
			currentPage.Reset()
		}
	}
	if strings.TrimSpace(currentPage.String()) != "" {
		pages = append(pages, strings.TrimSpace(currentPage.String()))
	}
	return title, author, year, pages
}

func extractTextFromPDFContentStream(stream []byte) string {
	s := string(stream)
	if !strings.Contains(s, "BT") && !strings.Contains(s, "Tj") && !strings.Contains(s, "TJ") {
		return ""
	}

	var out strings.Builder
	// Extract TJ arrays first
	s = pdfTJArrayRe.ReplaceAllStringFunc(s, func(arr string) string {
		var line strings.Builder
		for _, pm := range pdfTextParenRe.FindAllStringSubmatch(arr, -1) {
			line.WriteString(unescapePDFString(pm[1]))
		}
		if line.Len() > 0 {
			out.WriteString(line.String())
			out.WriteByte(' ')
		}
		return ""
	})

	for _, pm := range pdfTextParenRe.FindAllStringSubmatch(s, -1) {
		val := unescapePDFString(pm[1])
		if strings.TrimSpace(val) != "" {
			out.WriteString(val)
			out.WriteByte(' ')
		}
	}

	return cleanPDFPageText(out.String())
}

func unescapePDFString(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n':
				out.WriteByte('\n')
			case 'r':
				out.WriteByte('\r')
			case 't':
				out.WriteByte('\t')
			case '(', ')', '\\':
				out.WriteByte(s[i])
			default:
				out.WriteByte(s[i])
			}
			continue
		}
		if s[i] >= 32 || s[i] == '\n' || s[i] == '\t' {
			out.WriteByte(s[i])
		}
	}
	return out.String()
}

func cleanPDFPageText(raw string) string {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	var cleaned []string
	for _, line := range lines {
		t := strings.TrimSpace(multiSpaceRe.ReplaceAllString(line, " "))
		if t != "" {
			cleaned = append(cleaned, t)
		} else if len(cleaned) > 0 && cleaned[len(cleaned)-1] != "" {
			cleaned = append(cleaned, "")
		}
	}
	return strings.TrimSpace(strings.Join(cleaned, "\n"))
}

func detectPDFSectionHeading(pageText string) string {
	for _, line := range strings.Split(pageText, "\n") {
		line = strings.TrimSpace(line)
		if len(line) >= 4 && len(line) <= 90 {
			lower := strings.ToLower(line)
			if strings.HasPrefix(lower, "chapter ") || strings.HasPrefix(lower, "part ") || strings.HasPrefix(lower, "appendix ") {
				return line
			}
		}
	}
	return ""
}

func findBinary(name string, winFallbacks ...string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	if runtime.GOOS == "windows" {
		for _, fb := range winFallbacks {
			if _, err := os.Stat(fb); err == nil {
				return fb
			}
		}
	} else {
		for _, dir := range []string{"/usr/bin", "/usr/local/bin", "/bin"} {
			cand := filepath.Join(dir, name)
			if _, err := os.Stat(cand); err == nil {
				return cand
			}
		}
	}
	return ""
}

func convertWithCalibreToEPUB(filePath string, parentSize, childSize, childOverlap int) (*ParsedBook, error) {
	convBin := findBinary("ebook-convert", `C:\Program Files\Calibre2\ebook-convert.exe`)
	if convBin == "" {
		return nil, errors.New("ebook-convert not available")
	}
	tmpDir, err := os.MkdirTemp("", "library-mcp-mobi-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	outEPUB := filepath.Join(tmpDir, "converted.epub")
	if err := exec.Command(convBin, filePath, outEPUB).Run(); err != nil {
		return nil, err
	}
	return ParseFile(outEPUB, parentSize, childSize, childOverlap)
}

func applySidecarMetadataIfMissing(filePath string, book *ParsedBook) {
	dir := filepath.Dir(filePath)
	base := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))
	candidates := []string{
		filepath.Join(dir, filepath.Base(filePath)+".json"),
		filepath.Join(dir, base+".json"),
		filepath.Join(dir, base+"(1).pdf"),
	}
	for _, cand := range candidates {
		b, err := os.ReadFile(cand)
		if err != nil || len(b) > 256*1024 {
			continue
		}
		var sidecar struct {
			VolumeInfo struct {
				Title       string   `json:"title"`
				Author      []string `json:"author"`
				Publisher   string   `json:"publisher"`
				Description string   `json:"description"`
			} `json:"volume_info"`
		}
		if json.Unmarshal(b, &sidecar) != nil {
			continue
		}
		if len(sidecar.VolumeInfo.Author) > 0 && (book.Author == "" || book.Author == "Unknown Author") {
			joined := strings.Join(sidecar.VolumeInfo.Author, ", ")
			if !strings.EqualFold(joined, "Nieznany") && !strings.EqualFold(joined, "Unknown") {
				book.Author = joined
			}
		}
		if sidecar.VolumeInfo.Publisher != "" && book.Publisher == "" {
			book.Publisher = sidecar.VolumeInfo.Publisher
		}
		if sidecar.VolumeInfo.Description != "" && book.Description == "" {
			book.Description = sidecar.VolumeInfo.Description
		}
		break
	}
}

var authorDashTitleRe = regexp.MustCompile(`^([A-Z][a-zA-Z .,'-]+?)\s+-\s+(.+)$`)

func inferAuthorTitleFromFilename(filePath string, book *ParsedBook) {
	if book.Author != "" && book.Author != "Unknown Author" && !strings.EqualFold(book.Author, "Nieznany") {
		return
	}
	base := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))
	if m := authorDashTitleRe.FindStringSubmatch(base); len(m) == 3 {
		candAuthor := strings.TrimSpace(m[1])
		candTitle := strings.TrimSpace(m[2])
		if idx := strings.LastIndex(candTitle, "-"); idx > 4 {
			candTitle = strings.TrimSpace(candTitle[:idx])
		}
		if len(candAuthor) >= 4 && len(candAuthor) <= 60 && !strings.Contains(strings.ToLower(candAuthor), "edition") {
			book.Author = candAuthor
			if book.Title == "" || book.Title == base {
				book.Title = strings.ReplaceAll(candTitle, "_", ":")
			}
		}
	}
}
