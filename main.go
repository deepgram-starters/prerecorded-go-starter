/**
 * Go Transcription Starter - Backend Server
 *
 * This is a simple HTTP server that provides a transcription API endpoint
 * powered by Deepgram's Speech-to-Text service. It's designed to be easily
 * modified and extended for your own projects.
 *
 * Key Features:
 * - Single API endpoint: POST /api/transcription
 * - Accepts file uploads (multipart/form-data)
 * - CORS enabled for frontend communication
 * - JWT session auth
 * - Pure API server (frontend served separately)
 */

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/golang-jwt/jwt/v5"
	"github.com/joho/godotenv"

	listenrest "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/rest"
	dginterfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"
	listen "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/listen"
)

// ============================================================================
// SECTION 1: CONFIGURATION - Customize these values for your needs
// ============================================================================

/**
 * Default transcription model to use when none is specified.
 * Options: "nova-3", "nova-2", "nova", "enhanced", "base"
 * See: https://developers.deepgram.com/docs/models-languages-overview
 */
const defaultModel = "nova-3"

// config holds server configuration, overridable via environment variables.
type config struct {
	Port string
	Host string
}

// loadConfig reads PORT and HOST from the environment with sensible defaults.
func loadConfig() config {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}
	host := os.Getenv("HOST")
	if host == "" {
		host = "0.0.0.0"
	}
	return config{Port: port, Host: host}
}

// ============================================================================
// SECTION 2: SESSION AUTH - JWT tokens for production security
// ============================================================================

// sessionSecret is used to sign JWTs. Auto-generated if not set via env.
var sessionSecret string

// jwtExpiry controls how long issued tokens remain valid.
const jwtExpiry = 1 * time.Hour

// initSessionSecret sets the session secret from env or generates one.
func initSessionSecret() {
	sessionSecret = os.Getenv("SESSION_SECRET")
	if sessionSecret == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			log.Fatalf("Failed to generate session secret: %v", err)
		}
		sessionSecret = hex.EncodeToString(b)
	}
}

// requireSession is HTTP middleware that validates a JWT Bearer token.
// Returns 401 JSON error if the token is missing or invalid.
func requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			writeJSON(w, http.StatusUnauthorized, map[string]interface{}{
				"error": map[string]interface{}{
					"type":    "AuthenticationError",
					"code":    "MISSING_TOKEN",
					"message": "Authorization header with Bearer token is required",
				},
			})
			return
		}

		tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
		_, err := jwt.Parse(tokenStr, func(token *jwt.Token) (interface{}, error) {
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
			}
			return []byte(sessionSecret), nil
		})
		if err != nil {
			msg := "Invalid session token"
			if strings.Contains(err.Error(), "expired") {
				msg = "Session expired, please refresh the page"
			}
			writeJSON(w, http.StatusUnauthorized, map[string]interface{}{
				"error": map[string]interface{}{
					"type":    "AuthenticationError",
					"code":    "INVALID_TOKEN",
					"message": msg,
				},
			})
			return
		}

		next(w, r)
	}
}

// ============================================================================
// SECTION 3: API KEY LOADING - Load Deepgram API key from .env
// ============================================================================

// apiKey holds the Deepgram API key loaded at startup.
var apiKey string

// loadAPIKey reads the Deepgram API key from the environment.
// Exits with a helpful error message if not found.
func loadAPIKey() string {
	key := os.Getenv("DEEPGRAM_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "\n  ERROR: Deepgram API key not found!\n")
		fmt.Fprintln(os.Stderr, "Please set your API key using one of these methods:\n")
		fmt.Fprintln(os.Stderr, "1. Create a .env file (recommended):")
		fmt.Fprintln(os.Stderr, "   DEEPGRAM_API_KEY=your_api_key_here\n")
		fmt.Fprintln(os.Stderr, "2. Environment variable:")
		fmt.Fprintln(os.Stderr, "   export DEEPGRAM_API_KEY=your_api_key_here\n")
		fmt.Fprintln(os.Stderr, "Get your API key at: https://console.deepgram.com\n")
		os.Exit(1)
	}
	return key
}

// ============================================================================
// SECTION 4: SETUP - Initialize configuration and middleware
// ============================================================================

// corsMiddleware adds CORS headers to every response. Wildcard origin is safe
// because same-origin is enforced via Vite proxy / Caddy in production.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// ============================================================================
// SECTION 5: HELPER FUNCTIONS - JSON response utilities
// ============================================================================

// writeJSON marshals data to JSON and writes it to the response with the
// given HTTP status code.
func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("Error encoding JSON response: %v", err)
	}
}

// formatErrorResponse builds a structured error envelope suitable for the
// frontend to display.
func formatErrorResponse(errMsg string, statusCode int, code string) map[string]interface{} {
	errType := "TranscriptionError"
	if statusCode == 400 {
		errType = "ValidationError"
	}
	if code == "" {
		if statusCode == 400 {
			code = "MISSING_INPUT"
		} else {
			code = "TRANSCRIPTION_FAILED"
		}
	}
	return map[string]interface{}{
		"error": map[string]interface{}{
			"type":    errType,
			"code":    code,
			"message": errMsg,
			"details": map[string]interface{}{
				"originalError": errMsg,
			},
		},
	}
}

// ============================================================================
// SECTION 6: DEEPGRAM API CLIENT - Official Deepgram Go SDK (prerecorded REST)
// ============================================================================

// dgContextFromParams converts the request params into SDK custom query
// parameters, so every parameter forwarded by the frontend is passed through to
// Deepgram exactly as before.
func dgContextFromParams(params map[string]string) context.Context {
	custom := make(map[string][]string, len(params))
	for k, v := range params {
		if v != "" {
			custom[k] = []string{v}
		}
	}
	return dginterfaces.WithCustomParameters(context.Background(), custom)
}

// newListenClient builds a prerecorded (REST) Deepgram client using the loaded
// API key.
func newListenClient() *listenrest.Client {
	c := listen.NewREST(apiKey, &dginterfaces.ClientOptions{})
	return listenrest.New(c)
}

// dgResponseToMap marshals the SDK's typed response back into a generic map so
// the existing formatTranscriptionResponse logic (and the frontend contract) is
// preserved unchanged.
func dgResponseToMap(res interface{}) (map[string]interface{}, error) {
	b, err := json.Marshal(res)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal Deepgram response: %w", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("failed to parse Deepgram response: %w", err)
	}
	return m, nil
}

// callDeepgramTranscription sends audio bytes to the Deepgram /v1/listen
// endpoint via the SDK and returns the parsed JSON response.
func callDeepgramTranscription(audioData []byte, params map[string]string) (map[string]interface{}, error) {
	ctx := dgContextFromParams(params)
	res, err := newListenClient().FromStream(ctx, bytes.NewReader(audioData), &dginterfaces.PreRecordedTranscriptionOptions{})
	if err != nil {
		return nil, fmt.Errorf("Deepgram transcription failed: %w", err)
	}
	return dgResponseToMap(res)
}

// callDeepgramTranscriptionURL sends a URL to Deepgram for remote transcription
// via the SDK.
func callDeepgramTranscriptionURL(audioURL string, params map[string]string) (map[string]interface{}, error) {
	ctx := dgContextFromParams(params)
	res, err := newListenClient().FromURL(ctx, audioURL, &dginterfaces.PreRecordedTranscriptionOptions{})
	if err != nil {
		return nil, fmt.Errorf("Deepgram transcription failed: %w", err)
	}
	return dgResponseToMap(res)
}

// ============================================================================
// SECTION 7: RESPONSE FORMATTING - Shape Deepgram responses for the frontend
// ============================================================================

// formatTranscriptionResponse extracts the relevant fields from the raw
// Deepgram API response and returns a simplified structure the frontend
// expects.
func formatTranscriptionResponse(dgResponse map[string]interface{}, modelName string) (map[string]interface{}, error) {
	// Navigate: results -> channels[0] -> alternatives[0]
	results, ok := dgResponse["results"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("no transcription results returned from Deepgram")
	}
	channels, ok := results["channels"].([]interface{})
	if !ok || len(channels) == 0 {
		return nil, fmt.Errorf("no transcription results returned from Deepgram")
	}
	channel, ok := channels[0].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("no transcription results returned from Deepgram")
	}
	alternatives, ok := channel["alternatives"].([]interface{})
	if !ok || len(alternatives) == 0 {
		return nil, fmt.Errorf("no transcription results returned from Deepgram")
	}
	alt, ok := alternatives[0].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("no transcription results returned from Deepgram")
	}

	// Build metadata from top-level metadata field
	metadata := map[string]interface{}{
		"model_name": modelName,
	}
	if meta, ok := dgResponse["metadata"].(map[string]interface{}); ok {
		if v, ok := meta["model_uuid"]; ok {
			metadata["model_uuid"] = v
		}
		if v, ok := meta["request_id"]; ok {
			metadata["request_id"] = v
		}
	}

	response := map[string]interface{}{
		"transcript": alt["transcript"],
		"words":      alt["words"],
		"metadata":   metadata,
	}

	// Add optional duration if present
	if meta, ok := dgResponse["metadata"].(map[string]interface{}); ok {
		if dur, ok := meta["duration"]; ok {
			response["duration"] = dur
		}
	}

	return response, nil
}

// ============================================================================
// SECTION 8: SESSION ROUTES - Auth endpoints (unprotected)
// ============================================================================

// handleSession issues a signed JWT for session authentication.
// GET /api/session
func handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	now := time.Now()
	claims := jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(jwtExpiry)),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenStr, err := token.SignedString([]byte(sessionSecret))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"error": "Failed to generate session token",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"token": tokenStr,
	})
}

// ============================================================================
// SECTION 9: API ROUTES - Define your API endpoints here
// ============================================================================

// handleTranscription processes audio file uploads and sends them to the
// Deepgram API for prerecorded transcription.
//
// POST /api/transcription
//
// Accepts multipart/form-data with a "file" field.
// Query params: model, language, smart_format, diarize, punctuate,
//               paragraphs, utterances, filler_words
//
// Protected by JWT session auth (requireSession middleware).
func handleTranscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse multipart form (32 MB max memory)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, formatErrorResponse(
			"Failed to parse multipart form: "+err.Error(), 400, "MISSING_INPUT",
		))
		return
	}

	// Build query parameters from form values and query string
	model := r.URL.Query().Get("model")
	if model == "" {
		model = r.FormValue("model")
	}
	if model == "" {
		model = defaultModel
	}

	params := map[string]string{
		"model":        model,
		"language":     firstNonEmpty(r.URL.Query().Get("language"), r.FormValue("language"), "en"),
		"smart_format": firstNonEmpty(r.URL.Query().Get("smart_format"), r.FormValue("smart_format"), "true"),
	}

	// Optional boolean feature flags
	for _, key := range []string{"diarize", "punctuate", "paragraphs", "utterances", "filler_words"} {
		val := r.URL.Query().Get(key)
		if val == "" {
			val = r.FormValue(key)
		}
		if val != "" {
			params[key] = val
		}
	}

	// Check for URL-based transcription first, then fall back to file upload
	var dgResponse map[string]interface{}
	audioURL := r.FormValue("url")
	if audioURL != "" {
		var err error
		dgResponse, err = callDeepgramTranscriptionURL(audioURL, params)
		if err != nil {
			log.Printf("URL transcription error: %v", err)
			writeJSON(w, http.StatusInternalServerError, formatErrorResponse(
				"An error occurred during transcription", 500, "TRANSCRIPTION_FAILED",
			))
			return
		}
	} else {
		file, _, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, formatErrorResponse(
				"Either file or url must be provided", 400, "MISSING_INPUT",
			))
			return
		}
		defer file.Close()

		audioData, err := io.ReadAll(file)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, formatErrorResponse(
				"Failed to read uploaded file: "+err.Error(), 500, "TRANSCRIPTION_FAILED",
			))
			return
		}

		dgResponse, err = callDeepgramTranscription(audioData, params)
		if err != nil {
			log.Printf("Transcription error: %v", err)
			writeJSON(w, http.StatusInternalServerError, formatErrorResponse(
				"An error occurred during transcription", 500, "TRANSCRIPTION_FAILED",
			))
			return
		}
	}

	// Format and return response
	response, err := formatTranscriptionResponse(dgResponse, model)
	if err != nil {
		log.Printf("Response formatting error: %v", err)
		writeJSON(w, http.StatusInternalServerError, formatErrorResponse(
			err.Error(), 500, "TRANSCRIPTION_FAILED",
		))
		return
	}

	writeJSON(w, http.StatusOK, response)
}

// handleMetadata reads and returns the [meta] section from deepgram.toml.
// GET /api/metadata
func handleMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var tomlData struct {
		Meta map[string]interface{} `toml:"meta"`
	}
	if _, err := toml.DecodeFile("deepgram.toml", &tomlData); err != nil {
		log.Printf("Error reading deepgram.toml: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"error":   "INTERNAL_SERVER_ERROR",
			"message": "Failed to read metadata from deepgram.toml",
		})
		return
	}

	if tomlData.Meta == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"error":   "INTERNAL_SERVER_ERROR",
			"message": "Missing [meta] section in deepgram.toml",
		})
		return
	}

	writeJSON(w, http.StatusOK, tomlData.Meta)
}

// handleHealth is a simple health-check endpoint.
// GET /health
func handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "ok",
	})
}

// firstNonEmpty returns the first non-empty string from its arguments.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ============================================================================
// SECTION 10: SERVER START
// ============================================================================

func main() {
	// Load .env file (ignore error if not present)
	_ = godotenv.Load()

	// Initialize components
	cfg := loadConfig()
	initSessionSecret()
	apiKey = loadAPIKey()

	// Register routes
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session", handleSession)
	mux.HandleFunc("/api/transcription", requireSession(handleTranscription))
	mux.HandleFunc("/api/metadata", handleMetadata)
	mux.HandleFunc("/health", handleHealth)

	// Wrap with CORS middleware
	handler := corsMiddleware(mux)

	addr := cfg.Host + ":" + cfg.Port
	separator := strings.Repeat("=", 70)
	fmt.Printf("\n%s\n", separator)
	fmt.Printf("  Backend API running at http://localhost:%s\n", cfg.Port)
	fmt.Printf("  GET  /api/session\n")
	fmt.Printf("  POST /api/transcription (auth required)\n")
	fmt.Printf("  GET  /api/metadata\n")
	fmt.Printf("  GET  /health\n")
	fmt.Printf("%s\n\n", separator)

	server := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  120 * time.Second,
		WriteTimeout: 120 * time.Second,
	}

	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
