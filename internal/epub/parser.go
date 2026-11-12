package epub

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	MaxEPUBFileSize  = 250 * 1024 * 1024 // 250 MB max EPUB archive size
	MaxEntryFileSize = 25 * 1024 * 1024  // 25 MB max single uncompressed entry
)

// ParsedBook represents an extracted EPUB book with chapters, parent sections, and child search chunks.
type ParsedBook struct {
	ID          string          `json:"id"`
	FileHash    string          `json:"file_hash"`
	FileName    string          `json:"file_name"`
	Title       string          `json:"title"`
	Author      string          `json:"author"`
	Publisher   string          `json:"publisher"`
	Language    string          `json:"language"`
	Description string          `json:"description"`
	Subjects    []string        `json:"subjects"`
	Chapters    []ParsedChapter `json:"chapters"`
	Parents     []ParentSection `json:"parents"`
	Chunks      []ParsedChunk   `json:"chunks"`
}

// ParsedChapter represents a full chapter in the book spine.
type ParsedChapter struct {
	ID           string `json:"id"`
	BookID       string `json:"book_id"`
	ChapterIndex int    `json:"chapter_index"`
	Title        string `json:"title"`
	Href         string `json:"href"`
	Content      string `json:"content"`
}

// ParentSection represents a complete structural section (~2000-4000 chars) returned to the LLM during Parent-Child RAG.
type ParentSection struct {
	ID           string `json:"id"`
	BookID       string `json:"book_id"`
	ChapterID    string `json:"chapter_id"`
	BookTitle    string `json:"book_title"`
	Author       string `json:"author"`
	ChapterTitle string `json:"chapter_title"`
	SectionTitle string `json:"section_title"`
	ParentIndex  int    `json:"parent_index"`
	HasCode      bool   `json:"has_code"`
	Content      string `json:"content"`
}

// ParsedChunk represents a fine-grained child chunk (~650 chars) indexed in Qdrant and linked to its ParentSection.
type ParsedChunk struct {
	ID           string `json:"id"`
	PointUUID    string `json:"point_uuid"`
	ParentID     string `json:"parent_id"`
	BookID       string `json:"book_id"`
	ChapterID    string `json:"chapter_id"`
	BookTitle    string `json:"book_title"`
	Author       string `json:"author"`
	ChapterTitle string `json:"chapter_title"`
	SectionTitle string `json:"section_title"`
	ChunkIndex   int    `json:"chunk_index"`
	HasCode      bool   `json:"has_code"`
	Text         string `json:"text"`
}

type containerXML struct {
	Rootfiles []struct {
		FullPath string `xml:"full-path,attr"`
	} `xml:"rootfiles>rootfile"`
}

type opfPackage struct {
	Metadata struct {
		Title       []string `xml:"title"`
		Creator     []string `xml:"creator"`
		Publisher   []string `xml:"publisher"`
		Language    []string `xml:"language"`
		Description []string `xml:"description"`
		Subject     []string `xml:"subject"`
	} `xml:"metadata"`
	Manifest struct {
		Items []opfManifestItem `xml:"item"`
	} `xml:"manifest"`
	Spine struct {
		Toc      string         `xml:"toc,attr"`
		Itemrefs []opfSpineItem `xml:"itemref"`
	} `xml:"spine"`
}

type opfManifestItem struct {
	ID         string `xml:"id,attr"`
	Href       string `xml:"href,attr"`
	MediaType  string `xml:"media-type,attr"`
	Properties string `xml:"properties,attr"`
}

type opfSpineItem struct {
	IDRef  string `xml:"idref,attr"`
	Linear string `xml:"linear,attr"`
}

type ncxDoc struct {
	NavPoints []ncxNavPoint `xml:"navMap>navPoint"`
}

type ncxNavPoint struct {
	Labels  []string `xml:"navLabel>text"`
	Content struct {
		Src string `xml:"src,attr"`
	} `xml:"content"`
	Children []ncxNavPoint `xml:"navPoint"`
}

var (
	scriptStyleRe = regexp.MustCompile(`(?is)<(script|style|head|svg)[^>]*>.*?</(script|style|head|svg)>`)
	preBlockRe    = regexp.MustCompile(`(?is)<pre[^>]*>(.*?)</pre>`)
	tableBlockRe  = regexp.MustCompile(`(?is)<table[^>]*>(.*?)</table>`)
	trBlockRe     = regexp.MustCompile(`(?is)<tr[^>]*>(.*?)</tr>`)
	tdBlockRe     = regexp.MustCompile(`(?is)<t[dh][^>]*>(.*?)</t[dh]>`)
	headingRe     = regexp.MustCompile(`(?is)<h([1-4])[^>]*>(.*?)</h[1-4]>`)
	navLinkRe     = regexp.MustCompile(`(?is)<a[^>]+href=["']([^"']+)["'][^>]*>(.*?)</a>`)
	tagStripRe    = regexp.MustCompile(`(?s)<[^>]+>`)
	blockBreakRe  = regexp.MustCompile(`(?i)</?(p|div|li|ul|ol|blockquote|section|article|br|hr|tr)[^>]*>`)
	multiSpaceRe  = regexp.MustCompile(`[ \t]+`)
	multiNewline  = regexp.MustCompile(`\n{3,}`)
	xxeEntityRe   = regexp.MustCompile(`(?is)<!ENTITY\b`)
)

// DeterministicUUID generates an RFC-4122 v5-like UUID string from an arbitrary string ID for Qdrant point IDs.
func DeterministicUUID(id string) string {
	sum := sha256.Sum256([]byte("smart-library:" + id))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50 // version 5
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC4122
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ValidateEPUBHeader verifies the file size and ZIP magic signature (PK\x03\x04).
func ValidateEPUBHeader(filePath string) (string, int64, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return "", 0, err
	}
	if info.IsDir() {
		return "", 0, errors.New("path is a directory, not an EPUB file")
	}
	if info.Size() < 4 {
		return "", 0, errors.New("file is too small to be a valid EPUB")
	}
	if info.Size() > MaxEPUBFileSize {
		return "", 0, fmt.Errorf("file exceeds maximum allowed size of %d MB", MaxEPUBFileSize/(1024*1024))
	}

	f, err := os.Open(filePath)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil {
		return "", 0, err
	}
	if !bytes.Equal(magic, []byte{'P', 'K', 0x03, 0x04}) {
		return "", 0, errors.New("invalid file signature: not a valid ZIP/EPUB archive")
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, io.LimitReader(f, MaxEPUBFileSize)); err != nil {
		return "", 0, err
	}
	fileHash := hex.EncodeToString(hasher.Sum(nil))
	return fileHash, info.Size(), nil
}

// ParseFile parses an EPUB archive into chapters, parent sections, and child chunks for hierarchical RAG.
func ParseFile(filePath string, parentSize, childSize, childOverlap int) (*ParsedBook, error) {
	fileHash, fileSize, err := ValidateEPUBHeader(filePath)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	zr, err := zip.NewReader(f, fileSize)
	if err != nil {
		return nil, fmt.Errorf("failed to open epub zip: %w", err)
	}

	filesByPath := make(map[string]*zip.File, len(zr.File))
	for _, zf := range zr.File {
		cleanName, ok := sanitizeZipEntryPath(zf.Name)
		if !ok {
			continue
		}
		filesByPath[cleanName] = zf
	}

	opfPath := "content.opf"
	if containerBytes, err := readZipEntry(filesByPath, "META-INF/container.xml"); err == nil {
		var c containerXML
		if err := safeXMLUnmarshal(containerBytes, &c); err == nil && len(c.Rootfiles) > 0 {
			if cleanOPF, ok := sanitizeZipEntryPath(c.Rootfiles[0].FullPath); ok {
				opfPath = cleanOPF
			}
		}
	}

	opfBytes, err := readZipEntry(filesByPath, opfPath)
	if err != nil {
		for name := range filesByPath {
			if strings.HasSuffix(strings.ToLower(name), ".opf") {
				opfPath = name
				opfBytes, err = readZipEntry(filesByPath, opfPath)
				break
			}
		}
		if err != nil {
			return nil, errors.New("could not find OPF package document in EPUB")
		}
	}

	var pkg opfPackage
	if err := safeXMLUnmarshal(opfBytes, &pkg); err != nil {
		return nil, fmt.Errorf("failed to parse OPF XML: %w", err)
	}

	opfDir := path.Dir(opfPath)
	if opfDir == "." {
		opfDir = ""
	}

	bookID := fileHash[:16]
	title := firstNonEmpty(pkg.Metadata.Title, strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath)))
	author := strings.Join(cleanSlice(pkg.Metadata.Creator), ", ")
	if author == "" {
		author = "Unknown Author"
	}
	publisher := firstNonEmpty(pkg.Metadata.Publisher, "")
	language := firstNonEmpty(pkg.Metadata.Language, "en")
	description := stripHTMLTags(firstNonEmpty(pkg.Metadata.Description, ""))
	subjects := cleanSlice(pkg.Metadata.Subject)

	manifestByID := make(map[string]opfManifestItem, len(pkg.Manifest.Items))
	var navHref, ncxHref string
	for _, item := range pkg.Manifest.Items {
		manifestByID[item.ID] = item
		if strings.Contains(item.Properties, "nav") {
			navHref = resolveHref(opfDir, item.Href)
		}
		if item.ID == pkg.Spine.Toc || strings.Contains(item.MediaType, "dtbncx") || strings.HasSuffix(strings.ToLower(item.Href), ".ncx") {
			ncxHref = resolveHref(opfDir, item.Href)
		}
	}

	tocTitles := parseTOC(filesByPath, opfDir, ncxHref, navHref)

	var chapters []ParsedChapter
	var allParents []ParentSection
	var allChunks []ParsedChunk
	globalParentIdx := 0
	globalChunkIdx := 0

	for _, spineRef := range pkg.Spine.Itemrefs {
		if strings.EqualFold(spineRef.Linear, "no") {
			continue
		}
		item, ok := manifestByID[spineRef.IDRef]
		if !ok {
			continue
		}
		lowerMedia := strings.ToLower(item.MediaType)
		if !strings.Contains(lowerMedia, "html") && !strings.Contains(lowerMedia, "xml") {
			continue
		}

		entryPath := resolveHref(opfDir, item.Href)
		rawHTML, err := readZipEntry(filesByPath, entryPath)
		if err != nil {
			continue
		}

		chapTitle := lookupChapterTitle(tocTitles, entryPath, item.Href)
		sections, fullChapterText, inferredTitle := parseHTMLIntoSections(string(rawHTML))
		if strings.TrimSpace(fullChapterText) == "" {
			continue
		}
		if chapTitle == "" {
			if inferredTitle != "" {
				chapTitle = inferredTitle
			} else {
				chapTitle = fmt.Sprintf("Chapter %d", len(chapters)+1)
			}
		}

		chapIdx := len(chapters) + 1
		chapID := fmt.Sprintf("%s_ch%03d", bookID, chapIdx)
		chapters = append(chapters, ParsedChapter{
			ID:           chapID,
			BookID:       bookID,
			ChapterIndex: chapIdx,
			Title:        chapTitle,
			Href:         entryPath,
			Content:      fullChapterText,
		})

		parents, children := buildHierarchy(
			bookID, chapID, title, author, chapTitle,
			sections, parentSize, childSize, childOverlap,
			&globalParentIdx, &globalChunkIdx,
		)
		allParents = append(allParents, parents...)
		allChunks = append(allChunks, children...)
	}

	return &ParsedBook{
		ID:          bookID,
		FileHash:    fileHash,
		FileName:    filepath.Base(filePath),
		Title:       title,
		Author:      author,
		Publisher:   publisher,
		Language:    language,
		Description: description,
		Subjects:    subjects,
		Chapters:    chapters,
		Parents:     allParents,
		Chunks:      allChunks,
	}, nil
}

type sectionBlock struct {
	heading string
	text    string
}

func parseHTMLIntoSections(rawHTML string) ([]sectionBlock, string, string) {
	cleaned := scriptStyleRe.ReplaceAllString(rawHTML, "")

	var codeBlocks []string
	cleaned = preBlockRe.ReplaceAllStringFunc(cleaned, func(match string) string {
		inner := tagStripRe.ReplaceAllString(match, "")
		inner = html.UnescapeString(inner)
		inner = strings.TrimSpace(inner)
		if inner == "" {
			return ""
		}
		token := fmt.Sprintf("\n__CODE_BLOCK_%d__\n", len(codeBlocks))
		codeBlocks = append(codeBlocks, "```\n"+inner+"\n```")
		return token
	})

	var tableBlocks []string
	cleaned = tableBlockRe.ReplaceAllStringFunc(cleaned, func(match string) string {
		mdTable := htmlTableToMarkdown(match)
		if mdTable == "" {
			return ""
		}
		token := fmt.Sprintf("\n__TABLE_BLOCK_%d__\n", len(tableBlocks))
		tableBlocks = append(tableBlocks, mdTable)
		return token
	})

	var firstHeading string
	cleaned = headingRe.ReplaceAllStringFunc(cleaned, func(match string) string {
		sub := headingRe.FindStringSubmatch(match)
		if len(sub) < 3 {
			return match
		}
		hText := cleanInlineText(sub[2])
		if hText == "" {
			return ""
		}
		if firstHeading == "" {
			firstHeading = hText
		}
		return fmt.Sprintf("\n__SECTION_HEADING__%s__END_HEADING__\n", hText)
	})

	cleaned = blockBreakRe.ReplaceAllString(cleaned, "\n")
	cleaned = tagStripRe.ReplaceAllString(cleaned, " ")
	cleaned = html.UnescapeString(cleaned)

	lines := strings.Split(cleaned, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(multiSpaceRe.ReplaceAllString(line, " "))
	}
	cleaned = strings.Join(lines, "\n")
	cleaned = multiNewline.ReplaceAllString(cleaned, "\n\n")

	for i, cb := range codeBlocks {
		token := fmt.Sprintf("__CODE_BLOCK_%d__", i)
		cleaned = strings.ReplaceAll(cleaned, token, "\n"+cb+"\n")
	}
	for i, tb := range tableBlocks {
		token := fmt.Sprintf("__TABLE_BLOCK_%d__", i)
		cleaned = strings.ReplaceAll(cleaned, token, "\n"+tb+"\n")
	}

	parts := strings.Split(cleaned, "__SECTION_HEADING__")
	var sections []sectionBlock
	var fullBuilder strings.Builder

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		heading := "Overview"
		body := part
		if idx := strings.Index(part, "__END_HEADING__"); idx != -1 {
			heading = strings.TrimSpace(part[:idx])
			body = strings.TrimSpace(part[idx+len("__END_HEADING__"):])
		}
		if body == "" {
			continue
		}
		sections = append(sections, sectionBlock{
			heading: heading,
			text:    body,
		})
		if fullBuilder.Len() > 0 {
			fullBuilder.WriteString("\n\n")
		}
		if heading != "" && heading != "Overview" {
			fullBuilder.WriteString("## ")
			fullBuilder.WriteString(heading)
			fullBuilder.WriteString("\n\n")
		}
		fullBuilder.WriteString(body)
	}

	return sections, strings.TrimSpace(fullBuilder.String()), firstHeading
}

func htmlTableToMarkdown(tableHTML string) string {
	rows := trBlockRe.FindAllStringSubmatch(tableHTML, -1)
	if len(rows) == 0 {
		return ""
	}
	var out strings.Builder
	headerWritten := false
	for _, r := range rows {
		cells := tdBlockRe.FindAllStringSubmatch(r[1], -1)
		if len(cells) == 0 {
			continue
		}
		var vals []string
		for _, c := range cells {
			val := cleanInlineText(c[1])
			val = strings.ReplaceAll(val, "|", `\|`)
			vals = append(vals, val)
		}
		out.WriteString("| ")
		out.WriteString(strings.Join(vals, " | "))
		out.WriteString(" |\n")
		if !headerWritten {
			headerWritten = true
			out.WriteString("|")
			for range vals {
				out.WriteString(" --- |")
			}
			out.WriteString("\n")
		}
	}
	return strings.TrimSpace(out.String())
}

// buildHierarchy creates ParentSections (~parentSize chars) and splits each parent into Child Chunks (~childSize chars).
func buildHierarchy(
	bookID, chapID, bookTitle, author, chapTitle string,
	sections []sectionBlock,
	parentSize, childSize, childOverlap int,
	globalParentIdx, globalChunkIdx *int,
) ([]ParentSection, []ParsedChunk) {
	var parents []ParentSection
	var children []ParsedChunk

	for _, sec := range sections {
		breadcrumb := fmt.Sprintf("[Book: %s | Author: %s | Chapter: %s | Section: %s]\n", bookTitle, author, chapTitle, sec.heading)
		parentBlocks := splitIntoWindows(sec.text, parentSize, 0)
		for _, pBody := range parentBlocks {
			pBody = strings.TrimSpace(pBody)
			if pBody == "" {
				continue
			}
			*globalParentIdx++
			parentID := fmt.Sprintf("%s_sec%04d", bookID, *globalParentIdx)
			parentText := breadcrumb + pBody
			hasCode := strings.Contains(pBody, "```")

			parents = append(parents, ParentSection{
				ID:           parentID,
				BookID:       bookID,
				ChapterID:    chapID,
				BookTitle:    bookTitle,
				Author:       author,
				ChapterTitle: chapTitle,
				SectionTitle: sec.heading,
				ParentIndex:  *globalParentIdx,
				HasCode:      hasCode,
				Content:      parentText,
			})

			childWindows := splitIntoWindows(pBody, childSize, childOverlap)
			for _, cBody := range childWindows {
				cBody = strings.TrimSpace(cBody)
				if cBody == "" {
					continue
				}
				*globalChunkIdx++
				chunkID := fmt.Sprintf("%s_ck%05d", bookID, *globalChunkIdx)
				children = append(children, ParsedChunk{
					ID:           chunkID,
					PointUUID:    DeterministicUUID(chunkID),
					ParentID:     parentID,
					BookID:       bookID,
					ChapterID:    chapID,
					BookTitle:    bookTitle,
					Author:       author,
					ChapterTitle: chapTitle,
					SectionTitle: sec.heading,
					ChunkIndex:   *globalChunkIdx,
					HasCode:      strings.Contains(cBody, "```"),
					Text:         breadcrumb + cBody,
				})
			}
		}
	}

	return parents, children
}

func splitIntoWindows(text string, maxChars, overlap int) []string {
	paragraphs := splitKeepingCodeBlocks(text)
	var windows []string
	var current strings.Builder

	for _, para := range paragraphs {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}

		if current.Len() > 0 && current.Len()+len(para)+2 > maxChars {
			windows = append(windows, strings.TrimSpace(current.String()))
			prev := current.String()
			current.Reset()
			if overlap > 0 && len(prev) > overlap {
				tail := prev[len(prev)-overlap:]
				if sp := strings.Index(tail, " "); sp != -1 && sp+1 < len(tail) {
					tail = tail[sp+1:]
				}
				current.WriteString(strings.TrimSpace(tail))
				current.WriteString("\n\n")
			}
		}

		if len(para) > maxChars*2 {
			step := maxChars - overlap
			if step < 100 {
				step = maxChars
			}
			for start := 0; start < len(para); start += step {
				end := start + maxChars
				if end > len(para) {
					end = len(para)
				}
				windows = append(windows, strings.TrimSpace(para[start:end]))
				if end == len(para) {
					break
				}
			}
			continue
		}

		if current.Len() > 0 {
			current.WriteString("\n\n")
		}
		current.WriteString(para)
	}

	if strings.TrimSpace(current.String()) != "" {
		windows = append(windows, strings.TrimSpace(current.String()))
	}
	return windows
}

func splitKeepingCodeBlocks(text string) []string {
	var blocks []string
	var buf strings.Builder
	inCode := false

	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if !inCode {
				if strings.TrimSpace(buf.String()) != "" {
					blocks = append(blocks, strings.Split(buf.String(), "\n\n")...)
					buf.Reset()
				}
				inCode = true
				buf.WriteString(line)
				buf.WriteByte('\n')
			} else {
				buf.WriteString(line)
				blocks = append(blocks, buf.String())
				buf.Reset()
				inCode = false
			}
			continue
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
	}

	if strings.TrimSpace(buf.String()) != "" {
		blocks = append(blocks, strings.Split(buf.String(), "\n\n")...)
	}
	return blocks
}

func parseTOC(files map[string]*zip.File, opfDir, ncxHref, navHref string) map[string]string {
	toc := make(map[string]string)

	if ncxHref != "" {
		if b, err := readZipEntry(files, ncxHref); err == nil {
			var doc ncxDoc
			if err := safeXMLUnmarshal(b, &doc); err == nil {
				ncxDir := path.Dir(ncxHref)
				if ncxDir == "." {
					ncxDir = ""
				}
				var walk func(pts []ncxNavPoint)
				walk = func(pts []ncxNavPoint) {
					for _, pt := range pts {
						label := firstNonEmpty(pt.Labels, "")
						if label != "" && pt.Content.Src != "" {
							cleanSrc := stripFragment(resolveHref(ncxDir, pt.Content.Src))
							if _, exists := toc[cleanSrc]; !exists {
								toc[cleanSrc] = label
							}
						}
						walk(pt.Children)
					}
				}
				walk(doc.NavPoints)
			}
		}
	}

	if navHref != "" {
		if b, err := readZipEntry(files, navHref); err == nil {
			navDir := path.Dir(navHref)
			if navDir == "." {
				navDir = ""
			}
			matches := navLinkRe.FindAllStringSubmatch(string(b), -1)
			for _, m := range matches {
				if len(m) < 3 {
					continue
				}
				href := stripFragment(resolveHref(navDir, m[1]))
				label := cleanInlineText(m[2])
				if href != "" && label != "" {
					if _, exists := toc[href]; !exists {
						toc[href] = label
					}
				}
			}
		}
	}

	_ = opfDir
	return toc
}

func lookupChapterTitle(toc map[string]string, fullPath, rawHref string) string {
	cleanFull := stripFragment(fullPath)
	if t, ok := toc[cleanFull]; ok {
		return t
	}
	cleanRaw := stripFragment(rawHref)
	if t, ok := toc[cleanRaw]; ok {
		return t
	}
	base := path.Base(cleanFull)
	for k, v := range toc {
		if path.Base(k) == base {
			return v
		}
	}
	return ""
}

func sanitizeZipEntryPath(entryName string) (string, bool) {
	normalized := strings.ReplaceAll(entryName, `\`, "/")
	normalized = strings.TrimLeft(normalized, "/")
	cleaned := path.Clean(normalized)
	if cleaned == "." || cleaned == "" || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
		return "", false
	}
	return cleaned, true
}

func readZipEntry(files map[string]*zip.File, entryPath string) ([]byte, error) {
	cleanPath, ok := sanitizeZipEntryPath(entryPath)
	if !ok {
		return nil, errors.New("invalid zip entry path")
	}
	zf, exists := files[cleanPath]
	if !exists {
		lower := strings.ToLower(cleanPath)
		for k, v := range files {
			if strings.ToLower(k) == lower {
				zf = v
				exists = true
				break
			}
		}
	}
	if !exists {
		return nil, fmt.Errorf("entry not found: %s", cleanPath)
	}
	if zf.UncompressedSize64 > MaxEntryFileSize {
		return nil, fmt.Errorf("zip entry %s exceeds maximum allowed uncompressed size", cleanPath)
	}
	rc, err := zf.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	return io.ReadAll(io.LimitReader(rc, MaxEntryFileSize))
}

func safeXMLUnmarshal(data []byte, v any) error {
	if xxeEntityRe.Match(data) {
		return errors.New("security violation: XML ENTITY declarations are forbidden")
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.Strict = false
	decoder.Entity = xml.HTMLEntity
	return decoder.Decode(v)
}

func resolveHref(baseDir, href string) string {
	href = stripFragment(href)
	if baseDir == "" || baseDir == "." {
		return path.Clean(href)
	}
	return path.Clean(path.Join(baseDir, href))
}

func stripFragment(href string) string {
	if idx := strings.IndexByte(href, '#'); idx != -1 {
		return href[:idx]
	}
	return href
}

func cleanInlineText(s string) string {
	s = tagStripRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = multiSpaceRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func stripHTMLTags(s string) string {
	return cleanInlineText(s)
}

func firstNonEmpty(items []string, fallback string) string {
	for _, item := range items {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			return trimmed
		}
	}
	return fallback
}

func cleanSlice(items []string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, item := range items {
		t := strings.TrimSpace(item)
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}
