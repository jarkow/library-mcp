package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

var windowsDriveRe = regexp.MustCompile(`^([a-zA-Z]):[\\/](.*)$`)

// Config holds the runtime configuration for Smart Library (Go + Qdrant + Ollama).
type Config struct {
	BooksDir         string `json:"books_dir"`
	DataDir          string `json:"data_dir"`
	QdrantURL        string `json:"qdrant_url"`
	QdrantCollection string `json:"qdrant_collection"`
	OllamaURL        string `json:"ollama_url"`
	EmbeddingModel   string `json:"embedding_model"`
	ChatModel        string `json:"chat_model"`
	ParentChunkSize  int    `json:"parent_chunk_size"`
	ChildChunkSize   int    `json:"child_chunk_size"`
	ChildOverlap     int    `json:"child_overlap"`
	TopK             int    `json:"top_k"`
	Host             string `json:"host"`
	Port             int    `json:"port"`
}

// Default returns a Config populated with safe defaults.
func Default() *Config {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	return &Config{
		BooksDir:         `H:\Books`,
		DataDir:          filepath.Join(cwd, "data"),
		QdrantURL:        "http://127.0.0.1:6333",
		QdrantCollection: "smart_library_chunks",
		OllamaURL:        "http://127.0.0.1:11434",
		EmbeddingModel:   "bge-m3",
		ChatModel:        "qwen2.5-coder:14b",
		ParentChunkSize:  3200,
		ChildChunkSize:   650,
		ChildOverlap:     120,
		TopK:             6,
		Host:             "127.0.0.1",
		Port:             8765,
	}
}

// TranslatePath converts a user path (including Windows paths like H:\Books on WSL) into a clean OS path.
func TranslatePath(raw string) string {
	cleaned := strings.TrimSpace(raw)
	cleaned = strings.Trim(cleaned, `"'`)
	if cleaned == "" {
		return "books"
	}

	if runtime.GOOS != "windows" {
		if matches := windowsDriveRe.FindStringSubmatch(cleaned); len(matches) == 3 {
			drive := strings.ToLower(matches[1])
			rest := strings.ReplaceAll(matches[2], `\`, "/")
			rest = strings.TrimLeft(rest, "/")
			if rest == "" {
				return filepath.Clean("/mnt/" + drive)
			}
			return filepath.Clean(filepath.Join("/mnt", drive, rest))
		}
	}

	if strings.HasPrefix(cleaned, "~/") || cleaned == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			cleaned = filepath.Join(home, strings.TrimPrefix(cleaned, "~"))
		}
	}

	abs, err := filepath.Abs(cleaned)
	if err != nil {
		return filepath.Clean(cleaned)
	}
	return filepath.Clean(abs)
}

// IsSafeSubpath strictly verifies that targetPath is inside baseDir with a trailing separator check.
func IsSafeSubpath(targetPath, baseDir string) bool {
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return false
	}
	absTarget, err := filepath.Abs(targetPath)
	if err != nil {
		return false
	}

	realBase := absBase
	if resolved, err := filepath.EvalSymlinks(absBase); err == nil {
		realBase = resolved
	}
	realTarget := absTarget
	if resolved, err := filepath.EvalSymlinks(absTarget); err == nil {
		realTarget = resolved
	}

	realBase = filepath.Clean(realBase)
	realTarget = filepath.Clean(realTarget)
	if realTarget == realBase {
		return true
	}
	prefix := realBase + string(filepath.Separator)
	return strings.HasPrefix(realTarget, prefix)
}

// ValidateEndpointURL checks that an HTTP endpoint uses http or https and has a valid host.
func ValidateEndpointURL(rawURL string) (string, error) {
	trimmed := strings.TrimSpace(rawURL)
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("invalid endpoint url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("endpoint url must use http or https scheme")
	}
	if u.Hostname() == "" {
		return "", errors.New("endpoint url must include a hostname")
	}
	return strings.TrimRight(trimmed, "/"), nil
}

// GetSessionSecret resolves the session secret from env -> file -> cryptographic random fallback.
func GetSessionSecret(dataDir string) string {
	if envSecret := strings.TrimSpace(os.Getenv("SMART_LIBRARY_SECRET_KEY")); len(envSecret) >= 32 {
		return envSecret
	}

	secretFile := filepath.Join(dataDir, ".session_secret")
	if b, err := os.ReadFile(secretFile); err == nil {
		val := strings.TrimSpace(string(b))
		if len(val) >= 32 {
			return val
		}
	}

	log.Println("WARNING: Generating ephemeral session secret. Instance-isolated!")
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(buf)
}

// Load reads Config from disk and applies SMART_LIBRARY_* environment overrides.
func Load(configPath string) (*Config, error) {
	cfg := Default()
	if configPath == "" {
		if envPath := os.Getenv("SMART_LIBRARY_CONFIG"); envPath != "" {
			configPath = envPath
		} else {
			configPath = "config.json"
		}
	}

	if b, err := os.ReadFile(configPath); err == nil {
		if jsonErr := json.Unmarshal(b, cfg); jsonErr != nil {
			log.Printf("Warning: could not parse %s: %v", configPath, jsonErr)
		}
	}

	if v := strings.TrimSpace(os.Getenv("SMART_LIBRARY_BOOKS_DIR")); v != "" {
		cfg.BooksDir = v
	}
	if v := strings.TrimSpace(os.Getenv("SMART_LIBRARY_DATA_DIR")); v != "" {
		cfg.DataDir = v
	}
	if v := strings.TrimSpace(os.Getenv("SMART_LIBRARY_QDRANT_URL")); v != "" {
		cfg.QdrantURL = v
	}
	if v := strings.TrimSpace(os.Getenv("SMART_LIBRARY_QDRANT_COLLECTION")); v != "" {
		cfg.QdrantCollection = v
	}
	if v := strings.TrimSpace(os.Getenv("SMART_LIBRARY_OLLAMA_URL")); v != "" {
		cfg.OllamaURL = v
	}
	if v := strings.TrimSpace(os.Getenv("SMART_LIBRARY_EMBED_MODEL")); v != "" {
		cfg.EmbeddingModel = v
	}
	if v := strings.TrimSpace(os.Getenv("SMART_LIBRARY_CHAT_MODEL")); v != "" {
		cfg.ChatModel = v
	}
	if v := strings.TrimSpace(os.Getenv("SMART_LIBRARY_PARENT_SIZE")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.ParentChunkSize = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("SMART_LIBRARY_CHILD_SIZE")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.ChildChunkSize = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("SMART_LIBRARY_TOP_K")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.TopK = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("SMART_LIBRARY_PORT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Port = n
		}
	}

	cfg.Sanitize()
	if err := cfg.EnsureDirs(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Sanitize clamps numeric parameters and enforces localhost binding.
func (c *Config) Sanitize() {
	if c.ParentChunkSize < 1000 {
		c.ParentChunkSize = 1000
	} else if c.ParentChunkSize > 12000 {
		c.ParentChunkSize = 12000
	}
	if c.ChildChunkSize < 250 {
		c.ChildChunkSize = 250
	} else if c.ChildChunkSize > 2500 {
		c.ChildChunkSize = 2500
	}
	if c.ChildOverlap < 0 {
		c.ChildOverlap = 0
	} else if c.ChildOverlap >= c.ChildChunkSize/2 {
		c.ChildOverlap = c.ChildChunkSize / 4
	}
	if c.TopK < 1 {
		c.TopK = 1
	} else if c.TopK > 30 {
		c.TopK = 30
	}
	if c.Port < 1024 || c.Port > 65535 {
		c.Port = 8765
	}
	if validated, err := ValidateEndpointURL(c.QdrantURL); err == nil {
		c.QdrantURL = validated
	} else {
		c.QdrantURL = "http://127.0.0.1:6333"
	}
	if validated, err := ValidateEndpointURL(c.OllamaURL); err == nil {
		c.OllamaURL = validated
	} else {
		c.OllamaURL = "http://127.0.0.1:11434"
	}
	if strings.TrimSpace(c.QdrantCollection) == "" {
		c.QdrantCollection = "smart_library_chunks"
	}
	// Strictly bind to localhost
	c.Host = "127.0.0.1"
}

// ResolvedBooksDir returns the OS-resolved books directory path.
func (c *Config) ResolvedBooksDir() string {
	return TranslatePath(c.BooksDir)
}

// ResolvedDataDir returns the OS-resolved data directory path.
func (c *Config) ResolvedDataDir() string {
	return TranslatePath(c.DataDir)
}

// UploadsDir returns the secure directory for uploaded EPUB files outside the web root.
func (c *Config) UploadsDir() string {
	return filepath.Join(c.ResolvedDataDir(), "uploads")
}

// EnsureDirs creates the data and uploads directories with restricted permissions.
func (c *Config) EnsureDirs() error {
	if err := os.MkdirAll(c.ResolvedDataDir(), 0o700); err != nil {
		return err
	}
	return os.MkdirAll(c.UploadsDir(), 0o700)
}

// Save writes the configuration to configPath with canonical indentation.
func (c *Config) Save(configPath string) error {
	c.Sanitize()
	if configPath == "" {
		configPath = "config.json"
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, append(b, '\n'), 0o600)
}
