package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq"
)

// Global state
var (
	db        *sql.DB
	startTime = time.Now()
	
	// Simple in-memory cache for the most recent URLs (optional)
	recentCache = make(map[string]string, 1000)
	cacheMutex  sync.RWMutex
)

// Models
type CreateURLRequest struct {
	OriginalURL string `json:"original_url"`
	CustomCode  string `json:"custom_code,omitempty"`
	ExpiresAt   string `json:"expires_at,omitempty"`
}

type CreateURLResponse struct {
	ShortCode   string     `json:"short_code"`
	ShortURL    string     `json:"short_url"`
	OriginalURL string     `json:"original_url"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

type StatsResponse struct {
	ShortCode   string    `json:"short_code"`
	OriginalURL string    `json:"original_url"`
	ClickCount  int64     `json:"click_count"`
	CreatedAt   time.Time `json:"created_at"`
}

// Simple base62 encoding for fallback (if needed)
const base62Chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func generateShortCode() string {
	bytes := make([]byte, 6)
	rand.Read(bytes)
	
	result := make([]byte, 6)
	for i := 0; i < 6; i++ {
		result[i] = base62Chars[bytes[i]%62]
	}
	
	return string(result)
}

// Get next sequential number for short code
func getNextSequentialCode() (string, error) {
	var nextId int64
	
	// Get the next available ID from the sequence
	query := `SELECT nextval('urls_id_seq')`
	err := db.QueryRow(query).Scan(&nextId)
	if err != nil {
		return "", err
	}
	
	return strconv.FormatInt(nextId, 10), nil
}

// Database initialization - simpler config
func initDB() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://ihdas:your-password@localhost/ihdas?sslmode=disable"
	}
	
	var err error
	db, err = sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatal("Database connection failed:", err)
	}
	
	// Reasonable connection pool for portfolio project
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)
	
	// Create table with good indexing
	createTable := `
	CREATE TABLE IF NOT EXISTS urls (
		id BIGSERIAL PRIMARY KEY,
		short_code VARCHAR(10) UNIQUE NOT NULL,
		original_url TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT NOW(),
		expires_at TIMESTAMP,
		click_count BIGINT DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_short_code ON urls(short_code);
	CREATE INDEX IF NOT EXISTS idx_expires_at ON urls(expires_at) WHERE expires_at IS NOT NULL;
	
	-- Optimize sequence for better performance (cache 50 at a time)
	ALTER SEQUENCE urls_id_seq CACHE 50;
	`
	
	if _, err := db.Exec(createTable); err != nil {
		log.Fatal("Table creation failed:", err)
	}
	
	log.Println("✅ PostgreSQL connected")
}

// Optional simple cache (just for demo purposes)
func getCachedURL(shortCode string) (string, bool) {
	cacheMutex.RLock()
	url, exists := recentCache[shortCode]
	cacheMutex.RUnlock()
	return url, exists
}

func setCachedURL(shortCode, originalURL string) {
	cacheMutex.Lock()
	// Keep only last 1000 URLs to prevent memory issues
	if len(recentCache) >= 1000 {
		// Remove a random entry
		for k := range recentCache {
			delete(recentCache, k)
			break
		}
	}
	recentCache[shortCode] = originalURL
	cacheMutex.Unlock()
}

// Simple click counting (synchronous for simplicity)
func incrementClickCount(shortCode string) {
	db.Exec("UPDATE urls SET click_count = click_count + 1 WHERE short_code = $1", shortCode)
}

// Utility functions
func getClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if ips := strings.Split(xff, ","); len(ips) > 0 {
			return strings.TrimSpace(ips[0])
		}
	}
	
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}
	
	if ip := strings.Split(r.RemoteAddr, ":"); len(ip) > 0 {
		return ip[0]
	}
	
	return r.RemoteAddr
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// Handlers
func createURLHandler(w http.ResponseWriter, r *http.Request) {
	// Parse JSON body
	var req CreateURLRequest
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	
	// Validate URL
	if _, err := url.ParseRequestURI(req.OriginalURL); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid URL")
		return
	}
	
	// Generate or validate custom code
	var shortCode string
	if req.CustomCode != "" {
		shortCode = req.CustomCode
	} else {
		// Generate sequential number
		sequentialCode, err := getNextSequentialCode()
		if err != nil {
			log.Printf("Sequential code generation error: %v", err)
			writeError(w, http.StatusInternalServerError, "Code generation error")
			return
		}
		shortCode = sequentialCode
	}
	
	// Parse expiration if provided
	var expiresAt *time.Time
	if req.ExpiresAt != "" {
		parsed, err := time.Parse(time.RFC3339, req.ExpiresAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, "Invalid expiration date")
			return
		}
		expiresAt = &parsed
	}
	
	// Insert into database
	var id int64
	var createdAt time.Time
	
	query := `INSERT INTO urls (short_code, original_url, expires_at) 
			  VALUES ($1, $2, $3) 
			  RETURNING id, created_at`
	
	err = db.QueryRow(query, shortCode, req.OriginalURL, expiresAt).Scan(&id, &createdAt)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			writeError(w, http.StatusConflict, "Short code already exists")
			return
		}
		log.Printf("Database error: %v", err)
		writeError(w, http.StatusInternalServerError, "Database error")
		return
	}
	
	// Cache the new URL
	setCachedURL(shortCode, req.OriginalURL)
	
	// Build response
	baseURL := fmt.Sprintf("https://%s", r.Host)
	response := CreateURLResponse{
		ShortCode:   shortCode,
		ShortURL:    fmt.Sprintf("%s/%s", baseURL, shortCode),
		OriginalURL: req.OriginalURL,
		CreatedAt:   createdAt,
		ExpiresAt:   expiresAt,
	}
	
	writeJSON(w, http.StatusCreated, response)
}

func redirectHandler(w http.ResponseWriter, r *http.Request) {
	// Extract short code from path
	shortCode := strings.TrimPrefix(r.URL.Path, "/")
	if shortCode == "" || shortCode == "favicon.ico" {
		http.ServeFile(w, r, "static/index.html")
		return
	}
	
	// Try cache first (optional optimization)
	if originalURL, exists := getCachedURL(shortCode); exists {
		incrementClickCount(shortCode)
		http.Redirect(w, r, originalURL, http.StatusMovedPermanently)
		return
	}
	
	// Query database
	var originalURL string
	var expiresAt *time.Time
	query := `SELECT original_url, expires_at FROM urls WHERE short_code = $1`
	err := db.QueryRow(query, shortCode).Scan(&originalURL, &expiresAt)
	
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	} else if err != nil {
		log.Printf("Database error: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	
	// Check expiration
	if expiresAt != nil && time.Now().After(*expiresAt) {
		http.Error(w, "Link expired", http.StatusGone)
		return
	}
	
	// Cache for next time and redirect
	setCachedURL(shortCode, originalURL)
	incrementClickCount(shortCode)
	http.Redirect(w, r, originalURL, http.StatusMovedPermanently)
}

func statsHandler(w http.ResponseWriter, r *http.Request) {
	// Extract short code from path
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 4 {
		writeError(w, http.StatusBadRequest, "Invalid path")
		return
	}
	
	shortCode := parts[len(parts)-1]
	
	var stats StatsResponse
	query := `SELECT short_code, original_url, click_count, created_at 
			  FROM urls WHERE short_code = $1`
	
	err := db.QueryRow(query, shortCode).Scan(
		&stats.ShortCode, &stats.OriginalURL, &stats.ClickCount, &stats.CreatedAt)
	
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "Short URL not found")
		return
	} else if err != nil {
		log.Printf("Database error: %v", err)
		writeError(w, http.StatusInternalServerError, "Database error")
		return
	}
	
	writeJSON(w, http.StatusOK, stats)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	// Check database
	dbStatus := "up"
	if err := db.Ping(); err != nil {
		dbStatus = "down"
	}
	
	cacheSize := 0
	cacheMutex.RLock()
	cacheSize = len(recentCache)
	cacheMutex.RUnlock()
	
	// Get total URL count
	var totalUrls int64
	db.QueryRow("SELECT COUNT(*) FROM urls").Scan(&totalUrls)
	
	status := map[string]interface{}{
		"status":      "healthy",
		"database":    dbStatus,
		"cache_size":  cacheSize,
		"uptime":      time.Since(startTime).String(),
		"version":     "simple-go-postgresql-sequential",
		"total_urls":  totalUrls,
		"timestamp":   time.Now().Unix(),
	}
	
	if dbStatus == "down" {
		status["status"] = "unhealthy"
		writeJSON(w, http.StatusServiceUnavailable, status)
		return
	}
	
	writeJSON(w, http.StatusOK, status)
}

// Health dashboard handler
func healthDashboardHandler(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "static/health.html")
}

// Simple router
func router(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	method := r.Method
	
	// Security headers
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-XSS-Protection", "1; mode=block")
	
	// CORS headers
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	
	if method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	
	switch {
	case path == "/health":
		healthHandler(w, r)
	case path == "/dashboard" && method == "GET":
		healthDashboardHandler(w, r)
	case path == "/api/v1/shorten" && method == "POST":
		createURLHandler(w, r)
	case strings.HasPrefix(path, "/api/v1/stats/") && method == "GET":
		statsHandler(w, r)
	case path == "/" && method == "GET":
		http.ServeFile(w, r, "static/index.html")
	case strings.HasPrefix(path, "/static/"):
		http.StripPrefix("/static/", http.FileServer(http.Dir("static"))).ServeHTTP(w, r)
	default:
		// Everything else is treated as a potential short code
		redirectHandler(w, r)
	}
}

func main() {
	// Initialize
	initDB()
	
	// Create static directory but don't auto-generate index.html
	os.MkdirAll("static", 0755)
	
	// Simple server configuration
	server := &http.Server{
		Addr:         ":" + getPort(),
		Handler:      http.HandlerFunc(router),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	
	log.Printf("🚀 ihdas server starting on port %s", getPort())
	log.Printf("📊 Simple architecture: Go + PostgreSQL")
	log.Printf("📊 Health check: http://localhost:%s/health", getPort())
	log.Printf("🔍 Health dashboard: http://localhost:%s/dashboard", getPort())
	log.Printf("🎯 Sequential numbering enabled!")
	
	log.Fatal(server.ListenAndServe())
}

func getPort() string {
	if port := os.Getenv("PORT"); port != "" {
		return port
	}
	return "8080"
}