package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// BookMetadata holds enriched bibliographic metadata, category, and tags for a book.
type BookMetadata struct {
	Title          string   `json:"title"`
	Subtitle       string   `json:"subtitle,omitempty"`
	Author         string   `json:"author"`
	Publisher      string   `json:"publisher,omitempty"`
	PublishedYear  string   `json:"published_year,omitempty"`
	ISBN           string   `json:"isbn,omitempty"`
	PageCount      int      `json:"page_count,omitempty"`
	Language       string   `json:"language,omitempty"`
	Description    string   `json:"description,omitempty"`
	Subjects       []string `json:"subjects,omitempty"`
	Category       string   `json:"category"`
	Tags           []string `json:"tags"`
	CoverURL       string   `json:"cover_url,omitempty"`
	WebURL         string   `json:"web_url,omitempty"`
	MetadataSource string   `json:"metadata_source"`
}

// Enricher fetches book metadata from Open Library and Google Books and classifies books into categories and tags.
type Enricher struct {
	HTTPClient    *http.Client
	GoogleBaseURL string
	OpenLibURL    string
}

// NewEnricher creates a web metadata Enricher with a bounded timeout.
func NewEnricher() *Enricher {
	return &Enricher{
		HTTPClient: &http.Client{
			Timeout: 8 * time.Second,
		},
		GoogleBaseURL: "https://www.googleapis.com/books/v1/volumes",
		OpenLibURL:    "https://openlibrary.org/search.json",
	}
}

var (
	noiseParenRe = regexp.MustCompile(`(?i)\s*[\(\[](?:z-lib(?:rary)?|libgen|epub|pdf|mobi|azw3|[^\)\]]*edition[^\)\]]*)[\)\]]`)
	yearExtract  = regexp.MustCompile(`\b(19\d{2}|20\d{2})\b`)
	isbnCleanRe  = regexp.MustCompile(`[^0-9Xx]`)
	nonSlugRe    = regexp.MustCompile(`[^a-z0-9+#.-]+`)
)

// CleanQueryTitle strips file artifacts like "(Z-Library)" or trailing underscores from a title before web lookup.
func CleanQueryTitle(raw string) string {
	s := strings.TrimSpace(raw)
	s = noiseParenRe.ReplaceAllString(s, "")
	if idx := strings.Index(s, "_ -"); idx != -1 {
		s = s[:idx]
	}
	s = strings.TrimSuffix(s, "_")
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimSpace(s)
}

// NormalizeISBN cleans an ISBN-10 or ISBN-13 string.
func NormalizeISBN(raw string) string {
	cleaned := strings.ToUpper(isbnCleanRe.ReplaceAllString(raw, ""))
	if len(cleaned) == 10 || len(cleaned) == 13 {
		return cleaned
	}
	return ""
}

// Enrich combines existing EPUB metadata with live web metadata from Google Books & Open Library, then categorizes and tags the book.
func (e *Enricher) Enrich(ctx context.Context, seed BookMetadata, chapterTitles []string) BookMetadata {
	out := seed
	out.Title = CleanQueryTitle(out.Title)
	out.ISBN = NormalizeISBN(out.ISBN)
	if out.MetadataSource == "" {
		out.MetadataSource = "epub"
	}

	var sources []string
	if out.Title != "" || out.ISBN != "" {
		gbMeta, gbOK := e.fetchGoogleBooks(ctx, out.Title, out.Author, out.ISBN)
		if gbOK {
			out = mergeMetadata(out, gbMeta)
			sources = append(sources, "google_books")
		}

		olMeta, olOK := e.fetchOpenLibrary(ctx, out.Title, out.Author, out.ISBN)
		if olOK {
			out = mergeMetadata(out, olMeta)
			sources = append(sources, "openlibrary")
		}
	}

	if len(sources) > 0 {
		out.MetadataSource = strings.Join(sources, "+")
	}

	category, tags := ClassifyBook(out, chapterTitles)
	out.Category = category
	out.Tags = tags
	return out
}

func mergeMetadata(base, web BookMetadata) BookMetadata {
	if (base.Title == "" || strings.EqualFold(base.Title, "unknown")) && web.Title != "" {
		base.Title = web.Title
	}
	if base.Subtitle == "" && web.Subtitle != "" {
		base.Subtitle = web.Subtitle
	}
	if (base.Author == "" || strings.EqualFold(base.Author, "Unknown Author")) && web.Author != "" {
		base.Author = web.Author
	}
	if base.Publisher == "" && web.Publisher != "" {
		base.Publisher = web.Publisher
	}
	if base.PublishedYear == "" && web.PublishedYear != "" {
		base.PublishedYear = web.PublishedYear
	}
	if base.ISBN == "" && web.ISBN != "" {
		base.ISBN = web.ISBN
	}
	if base.PageCount == 0 && web.PageCount > 0 {
		base.PageCount = web.PageCount
	}
	if len(web.Description) > len(base.Description) {
		base.Description = web.Description
	}
	if base.CoverURL == "" && web.CoverURL != "" {
		base.CoverURL = web.CoverURL
	}
	if base.WebURL == "" && web.WebURL != "" {
		base.WebURL = web.WebURL
	}
	if len(web.Subjects) > 0 {
		seen := make(map[string]bool)
		for _, s := range base.Subjects {
			seen[strings.ToLower(strings.TrimSpace(s))] = true
		}
		for _, s := range web.Subjects {
			clean := strings.TrimSpace(s)
			if clean != "" && !seen[strings.ToLower(clean)] {
				seen[strings.ToLower(clean)] = true
				base.Subjects = append(base.Subjects, clean)
			}
		}
	}
	return base
}

func (e *Enricher) fetchGoogleBooks(ctx context.Context, title, author, isbn string) (BookMetadata, bool) {
	if e.GoogleBaseURL == "" {
		return BookMetadata{}, false
	}

	var query string
	if isbn != "" {
		query = "isbn:" + isbn
	} else {
		shortTitle := title
		if idx := strings.Index(shortTitle, ":"); idx > 3 {
			shortTitle = strings.TrimSpace(shortTitle[:idx])
		}
		query = fmt.Sprintf("intitle:%q", shortTitle)
		if author != "" && !strings.EqualFold(author, "Unknown Author") {
			firstAuthor := strings.Split(author, ",")[0]
			parts := strings.Fields(firstAuthor)
			if len(parts) > 0 {
				query += fmt.Sprintf(" inauthor:%q", parts[len(parts)-1])
			}
		}
	}

	reqURL := fmt.Sprintf("%s?q=%s&maxResults=3", e.GoogleBaseURL, url.QueryEscape(query))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return BookMetadata{}, false
	}
	req.Header.Set("User-Agent", "library-mcp/1.0 (https://github.com/jarkow/library-mcp)")

	resp, err := e.HTTPClient.Do(req)
	if err != nil {
		return BookMetadata{}, false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return BookMetadata{}, false
	}

	var payload struct {
		Items []struct {
			VolumeInfo struct {
				Title               string   `json:"title"`
				Subtitle            string   `json:"subtitle"`
				Authors             []string `json:"authors"`
				Publisher           string   `json:"publisher"`
				PublishedDate       string   `json:"publishedDate"`
				Description         string   `json:"description"`
				PageCount           int      `json:"pageCount"`
				Categories          []string `json:"categories"`
				Language            string   `json:"language"`
				InfoLink            string   `json:"infoLink"`
				IndustryIdentifiers []struct {
					Type       string `json:"type"`
					Identifier string `json:"identifier"`
				} `json:"industryIdentifiers"`
				ImageLinks struct {
					Thumbnail string `json:"thumbnail"`
				} `json:"imageLinks"`
			} `json:"volumeInfo"`
		} `json:"items"`
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, 2*1024*1024)).Decode(&payload); err != nil || len(payload.Items) == 0 {
		return BookMetadata{}, false
	}

	vi := payload.Items[0].VolumeInfo
	var foundISBN string
	for _, id := range vi.IndustryIdentifiers {
		if id.Type == "ISBN_13" {
			foundISBN = NormalizeISBN(id.Identifier)
			break
		}
		if id.Type == "ISBN_10" && foundISBN == "" {
			foundISBN = NormalizeISBN(id.Identifier)
		}
	}

	pubYear := ""
	if m := yearExtract.FindString(vi.PublishedDate); m != "" {
		pubYear = m
	}

	return BookMetadata{
		Title:         strings.TrimSpace(vi.Title),
		Subtitle:      strings.TrimSpace(vi.Subtitle),
		Author:        strings.Join(vi.Authors, ", "),
		Publisher:     strings.TrimSpace(vi.Publisher),
		PublishedYear: pubYear,
		ISBN:          foundISBN,
		PageCount:     vi.PageCount,
		Language:      strings.TrimSpace(vi.Language),
		Description:   strings.TrimSpace(vi.Description),
		Subjects:      vi.Categories,
		CoverURL:      strings.Replace(vi.ImageLinks.Thumbnail, "http://", "https://", 1),
		WebURL:        vi.InfoLink,
	}, true
}

func (e *Enricher) fetchOpenLibrary(ctx context.Context, title, author, isbn string) (BookMetadata, bool) {
	if e.OpenLibURL == "" {
		return BookMetadata{}, false
	}

	params := url.Values{}
	if isbn != "" {
		params.Set("isbn", isbn)
	} else {
		shortTitle := title
		if idx := strings.Index(shortTitle, ":"); idx > 3 {
			shortTitle = strings.TrimSpace(shortTitle[:idx])
		}
		params.Set("title", shortTitle)
		if author != "" && !strings.EqualFold(author, "Unknown Author") {
			firstAuthor := strings.TrimSpace(strings.Split(author, ",")[0])
			params.Set("author", firstAuthor)
		}
	}
	params.Set("limit", "3")
	params.Set("fields", "key,title,subtitle,author_name,first_publish_year,isbn,publisher,number_of_pages_median,subject,cover_i")

	reqURL := e.OpenLibURL + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return BookMetadata{}, false
	}
	req.Header.Set("User-Agent", "library-mcp/1.0 (https://github.com/jarkow/library-mcp)")

	resp, err := e.HTTPClient.Do(req)
	if err != nil {
		return BookMetadata{}, false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return BookMetadata{}, false
	}

	var payload struct {
		Docs []struct {
			Key              string   `json:"key"`
			Title            string   `json:"title"`
			Subtitle         string   `json:"subtitle"`
			AuthorName       []string `json:"author_name"`
			FirstPublishYear int      `json:"first_publish_year"`
			ISBN             []string `json:"isbn"`
			Publisher        []string `json:"publisher"`
			Pages            int      `json:"number_of_pages_median"`
			Subject          []string `json:"subject"`
			CoverID          int      `json:"cover_i"`
		} `json:"docs"`
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, 2*1024*1024)).Decode(&payload); err != nil || len(payload.Docs) == 0 {
		return BookMetadata{}, false
	}

	doc := payload.Docs[0]
	var foundISBN string
	for _, cand := range doc.ISBN {
		norm := NormalizeISBN(cand)
		if len(norm) == 13 {
			foundISBN = norm
			break
		}
		if len(norm) == 10 && foundISBN == "" {
			foundISBN = norm
		}
	}

	var pub string
	if len(doc.Publisher) > 0 {
		pub = strings.TrimSpace(doc.Publisher[0])
	}
	var year string
	if doc.FirstPublishYear > 0 {
		year = strconv.Itoa(doc.FirstPublishYear)
	}
	var coverURL string
	if doc.CoverID > 0 {
		coverURL = fmt.Sprintf("https://covers.openlibrary.org/b/id/%d-M.jpg", doc.CoverID)
	}
	var webURL string
	if doc.Key != "" {
		webURL = "https://openlibrary.org" + doc.Key
	}

	maxSubj := 12
	if len(doc.Subject) < maxSubj {
		maxSubj = len(doc.Subject)
	}

	return BookMetadata{
		Title:         strings.TrimSpace(doc.Title),
		Subtitle:      strings.TrimSpace(doc.Subtitle),
		Author:        strings.Join(doc.AuthorName, ", "),
		Publisher:     pub,
		PublishedYear: year,
		ISBN:          foundISBN,
		PageCount:     doc.Pages,
		Subjects:      doc.Subject[:maxSubj],
		CoverURL:      coverURL,
		WebURL:        webURL,
	}, true
}

type categoryRule struct {
	Category string
	Keywords []struct {
		Term   string
		Weight int
		Tag    string
	}
}

var taxonomyRules = []categoryRule{
	{
		Category: "AI & Machine Learning",
		Keywords: []struct {
			Term   string
			Weight int
			Tag    string
		}{
			{"ai engineering", 12, "ai-engineering"},
			{"foundation model", 10, "foundation-models"},
			{"large language model", 10, "llm"},
			{"llm", 8, "llm"},
			{"retrieval-augmented", 9, "rag"},
			{"rag", 6, "rag"},
			{"prompt engineering", 9, "prompt-engineering"},
			{"finetuning", 8, "fine-tuning"},
			{"fine-tuning", 8, "fine-tuning"},
			{"machine learning", 9, "machine-learning"},
			{"deep learning", 9, "deep-learning"},
			{"neural network", 8, "neural-networks"},
			{"transformer", 6, "transformers"},
			{"reinforcement learning", 8, "reinforcement-learning"},
			{"mlops", 8, "mlops"},
			{"inference optimization", 8, "inference"},
			{"guardrails", 6, "ai-safety"},
			{"artificial intelligence", 8, "artificial-intelligence"},
			{"vector database", 7, "vector-search"},
			{"embeddings", 6, "embeddings"},
		},
	},
	{
		Category: "Security & Cryptography",
		Keywords: []struct {
			Term   string
			Weight int
			Tag    string
		}{
			{"web application security", 14, "web-security"},
			{"application security", 10, "appsec"},
			{"cross-site scripting", 10, "xss"},
			{"xss", 8, "xss"},
			{"cross-site request forgery", 10, "csrf"},
			{"csrf", 8, "csrf"},
			{"sql injection", 9, "sql-injection"},
			{"owasp", 9, "owasp"},
			{"penetration testing", 9, "penetration-testing"},
			{"reconnaissance", 6, "reconnaissance"},
			{"cryptography", 9, "cryptography"},
			{"encryption", 6, "encryption"},
			{"vulnerability", 7, "vulnerabilities"},
			{"vulnerabilities", 7, "vulnerabilities"},
			{"exploit", 7, "exploitation"},
			{"authentication", 5, "authentication"},
			{"authorization", 5, "authorization"},
			{"cybersecurity", 9, "cybersecurity"},
			{"threat modeling", 8, "threat-modeling"},
			{"fuzzing", 7, "fuzzing"},
		},
	},
	{
		Category: "Software Architecture & Design",
		Keywords: []struct {
			Term   string
			Weight int
			Tag    string
		}{
			{"domain-driven design", 14, "domain-driven-design"},
			{"domain driven design", 14, "domain-driven-design"},
			{"bounded context", 10, "bounded-context"},
			{"ubiquitous language", 10, "ubiquitous-language"},
			{"context map", 9, "context-mapping"},
			{"shared kernel", 8, "shared-kernel"},
			{"anticorruption layer", 9, "anticorruption-layer"},
			{"supple design", 8, "domain-modeling"},
			{"aggregates", 7, "aggregates"},
			{"value objects", 7, "value-objects"},
			{"repositories", 5, "repositories"},
			{"microservices", 8, "microservices"},
			{"software architecture", 10, "software-architecture"},
			{"design patterns", 9, "design-patterns"},
			{"clean architecture", 10, "clean-architecture"},
			{"hexagonal", 8, "hexagonal-architecture"},
			{"event-driven", 8, "event-driven"},
			{"refactoring", 6, "refactoring"},
		},
	},
	{
		Category: "Software Engineering & DevOps",
		Keywords: []struct {
			Term   string
			Weight int
			Tag    string
		}{
			{"software engineering at google", 14, "google-engineering"},
			{"software engineering", 9, "software-engineering"},
			{"code review", 8, "code-review"},
			{"unit testing", 7, "unit-testing"},
			{"test doubles", 7, "testing"},
			{"larger testing", 7, "integration-testing"},
			{"continuous integration", 8, "ci-cd"},
			{"continuous delivery", 8, "ci-cd"},
			{"deprecation", 7, "deprecation"},
			{"version control", 7, "version-control"},
			{"monorepo", 8, "monorepo"},
			{"static analysis", 8, "static-analysis"},
			{"dependency management", 8, "dependency-management"},
			{"technical debt", 7, "technical-debt"},
			{"site reliability", 9, "sre"},
			{"devops", 8, "devops"},
			{"observability", 7, "observability"},
		},
	},
	{
		Category: "Data Engineering & Databases",
		Keywords: []struct {
			Term   string
			Weight int
			Tag    string
		}{
			{"data-intensive", 11, "data-intensive"},
			{"lsm-tree", 9, "storage-engines"},
			{"b-tree", 8, "storage-engines"},
			{"sstable", 9, "storage-engines"},
			{"replication", 7, "replication"},
			{"partitioning", 7, "sharding"},
			{"sharding", 7, "sharding"},
			{"stream processing", 8, "stream-processing"},
			{"batch processing", 8, "batch-processing"},
			{"postgresql", 8, "postgresql"},
			{"database internals", 10, "databases"},
			{"olap", 7, "analytics"},
			{"data warehouse", 8, "data-warehouse"},
		},
	},
	{
		Category: "Systems & Networking",
		Keywords: []struct {
			Term   string
			Weight int
			Tag    string
		}{
			{"systems programming", 11, "systems-programming"},
			{"operating system", 9, "operating-systems"},
			{"linux kernel", 10, "linux-kernel"},
			{"ebpf", 9, "ebpf"},
			{"memory management", 7, "memory-management"},
			{"concurrency", 7, "concurrency"},
			{"tcp/ip", 8, "networking"},
			{"distributed systems", 9, "distributed-systems"},
			{"consensus", 7, "consensus"},
			{"raft", 7, "raft"},
		},
	},
}

var programmingLangTags = map[string]string{
	"golang":     "golang",
	" in go":     "golang",
	"python":     "python",
	"rust":       "rust",
	"javascript": "javascript",
	"typescript": "typescript",
	"java ":      "java",
	"c++":        "cpp",
	"kubernetes": "kubernetes",
	"docker":     "docker",
}

// ClassifyBook assigns a primary Category and normalized technical Tags based on book metadata and chapter titles.
func ClassifyBook(meta BookMetadata, chapterTitles []string) (string, []string) {
	titleCorpus := strings.ToLower(meta.Title + " " + meta.Subtitle)
	subjectCorpus := strings.ToLower(strings.Join(meta.Subjects, " "))
	descCorpus := strings.ToLower(meta.Description)
	chapCorpus := strings.ToLower(strings.Join(chapterTitles, " "))
	fullCorpus := titleCorpus + "\n" + subjectCorpus + "\n" + descCorpus + "\n" + chapCorpus

	catScores := make(map[string]int)
	tagScores := make(map[string]int)

	for _, rule := range taxonomyRules {
		for _, kw := range rule.Keywords {
			matched := false
			weight := 0
			if strings.Contains(titleCorpus, kw.Term) {
				weight += kw.Weight * 3
				matched = true
			}
			if strings.Contains(subjectCorpus, kw.Term) {
				weight += kw.Weight * 2
				matched = true
			}
			if strings.Contains(chapCorpus, kw.Term) {
				weight += kw.Weight * 2
				matched = true
			}
			if strings.Contains(descCorpus, kw.Term) {
				weight += kw.Weight
				matched = true
			}
			if matched {
				catScores[rule.Category] += weight
				if kw.Tag != "" {
					tagScores[kw.Tag] += weight
				}
			}
		}
	}

	for needle, tag := range programmingLangTags {
		if strings.Contains(titleCorpus, needle) || strings.Contains(subjectCorpus, needle) {
			tagScores[tag] += 15
		} else if strings.Contains(fullCorpus, needle) {
			tagScores[tag] += 5
		}
	}

	// Also incorporate clean subject tags from EPUB / Web metadata
	for _, subj := range meta.Subjects {
		slug := normalizeTagSlug(subj)
		if slug != "" && len(slug) >= 3 && len(slug) <= 32 {
			if _, exists := tagScores[slug]; !exists {
				tagScores[slug] = 4
			}
		}
	}

	bestCategory := "Software Engineering & Computer Science"
	bestScore := 0
	for cat, sc := range catScores {
		if sc > bestScore || (sc == bestScore && cat < bestCategory) {
			bestScore = sc
			bestCategory = cat
		}
	}

	type scoredTag struct {
		tag   string
		score int
	}
	var rankedTags []scoredTag
	for t, sc := range tagScores {
		rankedTags = append(rankedTags, scoredTag{tag: t, score: sc})
	}
	sort.Slice(rankedTags, func(i, j int) bool {
		if rankedTags[i].score == rankedTags[j].score {
			return rankedTags[i].tag < rankedTags[j].tag
		}
		return rankedTags[i].score > rankedTags[j].score
	})

	maxTags := 8
	if len(rankedTags) < maxTags {
		maxTags = len(rankedTags)
	}
	tags := make([]string, 0, maxTags)
	for i := 0; i < maxTags; i++ {
		tags = append(tags, rankedTags[i].tag)
	}
	if len(tags) == 0 {
		tags = []string{"technical-book", "reference"}
	}

	return bestCategory, tags
}

var ignoredSubjectSlugs = map[string]bool{
	"computers": true, "general": true, "nonfiction": true,
	"electronic-books": true, "books": true, "science": true,
	"technology-engineering": true,
}

func normalizeTagSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if idx := strings.IndexAny(s, "/,;"); idx != -1 {
		s = strings.TrimSpace(s[:idx])
	}
	slug := strings.Trim(nonSlugRe.ReplaceAllString(s, "-"), "-")
	if ignoredSubjectSlugs[slug] {
		return ""
	}
	return slug
}
