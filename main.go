package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	qrcode "github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var phoneInputPattern = regexp.MustCompile(`^\+?[0-9 ()-]{7,24}$`)

const phonePairingReadyTimeout = 20 * time.Second

type config struct {
	token, addr, dsn, webhook, webhookSecret, webhookToken string
	dbDialect                                              string
	maxMedia                                               int64
	panelAutoToken                                         bool
	webhookAllowPrivate                                    bool
}

func loadConfig() (config, error) {
	addr := strings.TrimSpace(os.Getenv("LISTEN_ADDR"))
	if addr == "" {
		port := env("PORT", "8080")
		host := "127.0.0.1"
		if strings.TrimSpace(os.Getenv("PORT")) != "" {
			host = "0.0.0.0"
		}
		addr = net.JoinHostPort(host, port)
	}
	dsn := strings.TrimSpace(os.Getenv("DATABASE_DSN"))
	if dsn == "" {
		return config{}, errors.New("DATABASE_DSN must point to the Supabase Postgres database")
	}
	pgConfig, err := pgx.ParseConfig(dsn)
	if err != nil || pgConfig.Host == "" {
		return config{}, errors.New("DATABASE_DSN must be a valid PostgreSQL connection string")
	}
	pgConfig.RuntimeParams["search_path"] = "whatsapp,public"
	c := config{token: os.Getenv("API_TOKEN"), addr: addr, dsn: dsn, dbDialect: "postgres", webhook: os.Getenv("WEBHOOK_URL"), webhookSecret: os.Getenv("WEBHOOK_SECRET"), maxMedia: 16 << 20}
	if v := os.Getenv("MAX_MEDIA_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 || n > 64<<20 {
			return c, errors.New("MAX_MEDIA_BYTES must be between 1 and 67108864")
		}
		c.maxMedia = n
	}
	if _, _, err := net.SplitHostPort(c.addr); err != nil {
		return c, errors.New("LISTEN_ADDR must be host:port")
	}
	if len(c.token) < 32 {
		return c, errors.New("API_TOKEN must contain at least 32 characters")
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PANEL_AUTO_TOKEN"))) {
	case "1", "true", "yes", "on":
		c.panelAutoToken = true
	}
	c.webhookToken = strings.TrimSpace(os.Getenv("WEBHOOK_TOKEN"))
	if strings.ToLower(strings.TrimSpace(os.Getenv("WEBHOOK_ALLOW_PRIVATE"))) == "1" {
		c.webhookAllowPrivate = true
	}
	if c.webhook != "" {
		u, err := url.Parse(c.webhook)
		if err != nil || u.Host == "" || (u.Scheme != "https" && !(c.webhookAllowPrivate && u.Scheme == "http")) {
			return c, errors.New("WEBHOOK_URL must be an HTTPS URL (or HTTP when WEBHOOK_ALLOW_PRIVATE=1)")
		}
		if c.webhookSecret == "" {
			return c, errors.New("WEBHOOK_SECRET is required when WEBHOOK_URL is set")
		}
	}
	return c, nil
}
func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

type event struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	SessionID string    `json:"session_id"`
	Timestamp time.Time `json:"timestamp"`
	Data      any       `json:"data"`
}
type session struct {
	ID                 string                   `json:"id"`
	Status             string                   `json:"status"`
	Account            string                   `json:"account,omitempty"`
	Client             *whatsmeow.Client        `json:"-"`
	Pairing            string                   `json:"-"`
	PairingMethod      string                   `json:"-"`
	PasskeyPublicKey   *types.WebAuthnPublicKey `json:"-"`
	PasskeyConfirmCode string                   `json:"-"`
	PasskeySkipHandoff bool                     `json:"-"`
	opMu               sync.RWMutex
	mu                 sync.RWMutex
	qrReady            chan struct{}
	qrOnce             sync.Once
}
type app struct {
	cfg        config
	store      *sqlstore.Container
	db         *sql.DB
	logger     *slog.Logger
	mu         sync.RWMutex
	sessions   map[string]*session
	events     chan event
	httpClient *http.Client
	rateMu     sync.Mutex
	rate       map[string]bucket
	mediaJobs  chan mediaJob
}
type bucket struct {
	tokens  float64
	updated time.Time
}
type mediaJob struct {
	sessionID, messageID string
	client               *whatsmeow.Client
	attachment           whatsmeow.DownloadableMessage
	kind                 string
}

func ensureMetadataSchema(db *sql.DB) error {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS api_sessions (id TEXT PRIMARY KEY, device_id TEXT NOT NULL UNIQUE, created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS webhook_events (id TEXT PRIMARY KEY, payload BYTEA NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, available_at BIGINT NOT NULL DEFAULT 0, created_at BIGINT NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS api_messages (id BIGSERIAL PRIMARY KEY, session_id TEXT NOT NULL, chat_jid TEXT NOT NULL, message_id TEXT NOT NULL, sender TEXT NOT NULL DEFAULT '', from_me BOOLEAN NOT NULL DEFAULT false, text TEXT NOT NULL DEFAULT '', media_type TEXT NOT NULL DEFAULT '', timestamp BIGINT NOT NULL, UNIQUE(session_id, chat_jid, message_id))`,
		`CREATE INDEX IF NOT EXISTS idx_api_messages_chat ON api_messages(session_id, chat_jid, timestamp)`,
		`CREATE TABLE IF NOT EXISTS api_products (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, sku TEXT NOT NULL DEFAULT '', name TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', price TEXT NOT NULL DEFAULT '', currency TEXT NOT NULL DEFAULT 'BRL', image_url TEXT NOT NULL DEFAULT '', created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS api_media (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, payload BYTEA NOT NULL, content_type TEXT NOT NULL DEFAULT 'application/octet-stream', filename TEXT NOT NULL DEFAULT '', created_at BIGINT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_api_media_session ON api_media(session_id, created_at)`,
		`CREATE TABLE IF NOT EXISTS app_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) bindSQL(query string) string {
	if a.cfg.dbDialect != "postgres" {
		return query
	}
	query = strings.Replace(query, "INSERT OR IGNORE INTO", "INSERT INTO", 1)
	ignored := strings.HasPrefix(query, "INSERT INTO") && !strings.Contains(query, "ON CONFLICT")
	var out strings.Builder
	index := 1
	for _, char := range query {
		if char == '?' {
			fmt.Fprintf(&out, "$%d", index)
			index++
		} else {
			out.WriteRune(char)
		}
	}
	if ignored {
		out.WriteString(" ON CONFLICT DO NOTHING")
	}
	return out.String()
}

func (a *app) exec(query string, args ...any) (sql.Result, error) {
	return a.db.Exec(a.bindSQL(query), args...)
}
func (a *app) execContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return a.db.ExecContext(ctx, a.bindSQL(query), args...)
}
func (a *app) query(query string, args ...any) (*sql.Rows, error) {
	return a.db.Query(a.bindSQL(query), args...)
}
func (a *app) queryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return a.db.QueryContext(ctx, a.bindSQL(query), args...)
}
func (a *app) queryRow(query string, args ...any) *sql.Row {
	return a.db.QueryRow(a.bindSQL(query), args...)
}
func (a *app) queryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return a.db.QueryRowContext(ctx, a.bindSQL(query), args...)
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	pgConfig, err := pgx.ParseConfig(cfg.dsn)
	if err != nil {
		logger.Error("parse Supabase PostgreSQL connection", "error", err)
		os.Exit(1)
	}
	pgConfig.RuntimeParams["search_path"] = "whatsapp,public"
	db := stdlib.OpenDB(*pgConfig)
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err = db.Ping(); err != nil {
		logger.Error("connect to Supabase PostgreSQL", "error", err)
		os.Exit(1)
	}
	var schemaReady bool
	if err = db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name='whatsapp')`).Scan(&schemaReady); err != nil || !schemaReady {
		logger.Error("WhatsApp schema missing; apply the Supabase migration before starting the service", "error", err)
		os.Exit(1)
	}
	container := sqlstore.NewWithDB(db, "postgres", nil)
	if err = container.Upgrade(context.Background()); err != nil {
		logger.Error("upgrade WhatsApp session tables in Supabase", "error", err)
		os.Exit(1)
	}
	if err = ensureMetadataSchema(db); err != nil {
		logger.Error("initialize WhatsApp metadata in Supabase", "error", err)
		os.Exit(1)
	}
	if err := db.Ping(); err != nil {
		logger.Error("ping metadata database", "error", err)
		os.Exit(1)
	}
	allowPrivate := cfg.webhookAllowPrivate
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return webhookDial(ctx, allowPrivate, network, address)
	}, TLSHandshakeTimeout: 4 * time.Second, ResponseHeaderTimeout: 5 * time.Second}
	client := &http.Client{Timeout: 8 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	a := &app{cfg: cfg, store: container, db: db, logger: logger, sessions: make(map[string]*session), events: make(chan event, 256), httpClient: client, rate: make(map[string]bucket), mediaJobs: make(chan mediaJob, 128)}
	if err := a.restore(context.Background()); err != nil {
		logger.Error("restore sessions", "error", err)
		os.Exit(1)
	}
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	go a.deliverEvents(workerCtx)
	for i := 0; i < 2; i++ {
		go a.processMedia(workerCtx)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /readyz", a.ready)
	mux.HandleFunc("GET /{$}", a.ui)
	mux.Handle("/v1/", a.rateLimit(a.auth(http.HandlerFunc(a.route))))
	srv := &http.Server{Addr: cfg.addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		logger.Info("API listening", "address", cfg.addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP server", "error", err)
			stop()
		}
	}()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	a.close()
	cancelWorkers()
	_ = db.Close()
	_ = container.Close()
}
func (a *app) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		got := ""
		if strings.HasPrefix(header, "Bearer ") {
			got = strings.TrimPrefix(header, "Bearer ")
		}
		if !hmac.Equal([]byte(got), []byte(a.cfg.token)) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "valid bearer token required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *app) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}
		key := ip + ":" + r.URL.Path
		capacity := 60.0
		rate := 1.0
		if strings.HasSuffix(r.URL.Path, "/connect") || strings.HasSuffix(r.URL.Path, "/pair") {
			capacity = 5
			rate = 1.0 / 12
		}
		now := time.Now()
		a.rateMu.Lock()
		if len(a.rate) > 10000 {
			for k, v := range a.rate {
				if now.Sub(v.updated) > time.Hour {
					delete(a.rate, k)
				}
			}
		}
		b, ok := a.rate[key]
		if !ok {
			b = bucket{tokens: capacity, updated: now}
		} else {
			b.tokens += now.Sub(b.updated).Seconds() * rate
			if b.tokens > capacity {
				b.tokens = capacity
			}
			b.updated = now
		}
		if b.tokens < 1 {
			a.rate[key] = b
			a.rateMu.Unlock()
			w.Header().Set("Retry-After", "1")
			writeError(w, 429, "rate_limited", "too many requests")
			return
		}
		b.tokens--
		a.rate[key] = b
		a.rateMu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func safeWebhookDialer() *net.Dialer {
	return &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
}

func isPublicIP(ip net.IP) bool {
	return ip != nil && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast() && !ip.IsUnspecified()
}

func webhookDial(ctx context.Context, allowPrivate bool, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if !allowPrivate {
		for _, entry := range ips {
			if !isPublicIP(entry.IP) {
				return nil, errors.New("webhook destination resolves to a non-public address")
			}
		}
	}
	var last error
	d := safeWebhookDialer()
	for _, entry := range ips {
		conn, dialErr := d.DialContext(ctx, network, net.JoinHostPort(entry.IP.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		last = dialErr
	}
	return nil, last
}
func (a *app) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok"})
}
func (a *app) ready(w http.ResponseWriter, _ *http.Request) {
	if err := a.db.Ping(); err != nil {
		writeError(w, 503, "not_ready", "database unavailable")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ready"})
}
func (a *app) route(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "v1" || (parts[1] != "sessions" && parts[1] != "settings") {
		if len(parts) == 3 && parts[0] == "v1" && parts[1] == "media" && r.Method == http.MethodGet {
			a.serveMedia(w, r, parts[2])
			return
		}
		writeError(w, 404, "not_found", "route not found")
		return
	}
	if len(parts) == 2 {
		if r.Method == http.MethodPost {
			a.create(w, r)
			return
		}
		if r.Method == http.MethodGet {
			a.list(w, r)
			return
		}
		writeError(w, 405, "method_not_allowed", "method not allowed")
		return
	}
	if len(parts) < 3 {
		writeError(w, 404, "not_found", "route not found")
		return
	}
	if parts[2] == "" {
		writeError(w, 400, "invalid_session", "session id required")
		return
	}
	if len(parts) == 3 && parts[1] == "settings" {
		if parts[2] != "webhook" {
			writeError(w, 404, "not_found", "route not found")
			return
		}
		switch r.Method {
		case http.MethodGet:
			a.getWebhookSettings(w, r)
		case http.MethodPost:
			a.saveWebhookSettings(w, r)
		default:
			writeError(w, 405, "method_not_allowed", "method not allowed")
		}
		return
	}
	if len(parts) == 3 {
		switch r.Method {
		case http.MethodGet:
			a.get(w, parts[2])
		case http.MethodDelete:
			a.remove(w, r, parts[2])
		default:
			writeError(w, 405, "method_not_allowed", "method not allowed")
		}
		return
	}
	s := a.find(parts[2])
	if s == nil {
		writeError(w, 404, "not_found", "session not found")
		return
	}
	// Keep a session alive for the duration of ordinary operations. Lifecycle
	// operations take the exclusive lock so removal cannot race with a send.
	if parts[3] == "connect" || parts[3] == "disconnect" || parts[3] == "pair" {
		// These handlers manage the lock themselves.
	} else if parts[3] == "passkey" {
		// Passkey handlers manage the lock according to the operation.
	} else {
		s.opMu.RLock()
		defer s.opMu.RUnlock()
	}
	if len(parts) == 4 {
		switch {
		case parts[3] == "connect" && r.Method == http.MethodPost:
			a.connect(w, r, s)
		case parts[3] == "pair" && r.Method == http.MethodPost:
			a.pairPhone(w, r, s)
		case parts[3] == "pairing" && r.Method == http.MethodGet:
			w.Header().Set("Cache-Control", "no-store, max-age=0")
			w.Header().Set("Pragma", "no-cache")
			s.mu.RLock()
			code, method := s.Pairing, s.PairingMethod
			s.mu.RUnlock()
			if code == "" {
				writeError(w, 404, "pairing_unavailable", "no current pairing code")
				return
			}
			if r.URL.Query().Get("format") == "png" {
				if method != "qr" {
					writeError(w, http.StatusConflict, "qr_unavailable", "the current pairing uses a phone code, not a QR")
					return
				}
				png, err := qrcode.Encode(code, qrcode.Medium, 512)
				if err != nil {
					writeError(w, 500, "qr_generation_failed", "could not render pairing QR")
					return
				}
				w.Header().Set("Content-Type", "image/png")
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("X-Content-Type-Options", "nosniff")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(png)
				return
			}
			writeJSON(w, 200, map[string]string{"code": code, "method": method})
		case parts[3] == "passkey":
			a.passkey(w, r, s)
		case parts[3] == "disconnect" && r.Method == http.MethodPost:
			s.opMu.Lock()
			defer s.opMu.Unlock()
			s.Client.Disconnect()
			s.mu.Lock()
			s.Status = "disconnected"
			s.mu.Unlock()
			a.emit(s.ID, "session.disconnected", map[string]any{})
			writeJSON(w, 200, map[string]string{"status": "disconnected"})
		case parts[3] == "messages" && r.Method == http.MethodPost:
			a.send(w, r, s)
		case parts[3] == "presence" && r.Method == http.MethodPost:
			a.presence(w, r, s)
		case parts[3] == "contacts" && r.Method == http.MethodGet:
			a.contacts(w, r, s)
		case parts[3] == "groups" && r.Method == http.MethodGet:
			a.groups(w, r, s)
		case parts[3] == "chats" && r.Method == http.MethodGet:
			a.listChats(w, r, s)
		case parts[3] == "products" && r.Method == http.MethodGet:
			a.listProducts(w, r, s)
		case parts[3] == "products" && r.Method == http.MethodPost:
			a.createProduct(w, r, s)
		default:
			writeError(w, 404, "not_found", "route not found")
		}
		return
	}
	if len(parts) == 5 && parts[3] == "passkey" && parts[4] == "confirmation" {
		a.passkeyConfirmation(w, r, s)
		return
	}
	if len(parts) == 5 && parts[3] == "chats" && parts[4] != "" && r.Method == http.MethodGet {
		a.chatHistory(w, r, s, parts[4])
		return
	}
	if len(parts) == 5 && parts[3] == "products" && parts[4] != "" {
		switch r.Method {
		case http.MethodPatch:
			a.updateProduct(w, r, s, parts[4])
		case http.MethodDelete:
			a.deleteProduct(w, r, s, parts[4])
		default:
			writeError(w, 405, "method_not_allowed", "method not allowed")
		}
		return
	}
	if len(parts) == 6 && parts[3] == "contacts" && parts[4] != "" && parts[5] == "picture" && r.Method == http.MethodGet {
		a.contactPicture(w, r, s, parts[4])
		return
	}
	if len(parts) == 6 && parts[3] == "messages" && parts[4] != "" && parts[5] == "read" && r.Method == http.MethodPost {
		a.readMessage(w, r, s, parts[4])
		return
	}
	if len(parts) == 6 && parts[3] == "chats" && parts[4] != "" && parts[5] == "settings" {
		if r.Method == http.MethodGet {
			a.getChatSettings(w, r, s, parts[4])
			return
		}
		if r.Method == http.MethodPatch {
			a.updateChatSettings(w, r, s, parts[4])
			return
		}
	}
	writeError(w, 404, "not_found", "route not found")
}

func (a *app) create(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := decode(w, r, &in, 4096); err != nil {
			writeError(w, 400, "invalid_request", err.Error())
			return
		}
	}
	if in.ID == "" {
		in.ID = uuid.NewString()
	}
	if !sessionIDPattern.MatchString(in.ID) {
		writeError(w, 400, "invalid_request", "id must contain 1-64 letters, digits, underscores or hyphens")
		return
	}
	device := a.store.NewDevice()
	id := uuid.NewString()
	if _, err := a.execContext(r.Context(), `INSERT INTO api_sessions(id,device_id) VALUES(?,?)`, in.ID, id); err != nil {
		writeError(w, 409, "session_exists", "session id already exists")
		return
	}
	s := a.makeSession(device, in.ID)
	a.mu.Lock()
	a.sessions[in.ID] = s
	a.mu.Unlock()
	writeJSON(w, 201, map[string]string{"id": in.ID, "status": s.Status})
}
func (a *app) makeSession(device *store.Device, id string) *session {
	c := whatsmeow.NewClient(device, nil)
	c.InitialAutoReconnect = true
	s := &session{ID: id, Status: "disconnected", Client: c, qrReady: make(chan struct{})}
	if device.ID != nil {
		s.Account = device.ID.String()
	}
	c.AddEventHandler(func(raw any) {
		switch e := raw.(type) {
		case *events.Connected:
			if device.ID != nil {
				_, _ = a.exec(`UPDATE api_sessions SET device_id=? WHERE id=?`, device.ID.String(), id)
			}
			s.mu.Lock()
			s.Status = "connected"
			if device.ID != nil {
				s.Account = device.ID.String()
			}
			s.Pairing = ""
			s.PairingMethod = ""
			s.PasskeyPublicKey = nil
			s.PasskeyConfirmCode = ""
			s.PasskeySkipHandoff = false
			s.mu.Unlock()
			a.emit(id, "session.connected", map[string]any{})
		case *events.Disconnected:
			s.mu.Lock()
			reconnecting := s.Client.EnableAutoReconnect && s.Account != ""
			if reconnecting {
				s.Status = "reconnecting"
			} else {
				s.Status = "disconnected"
			}
			s.PasskeyPublicKey = nil
			s.PasskeyConfirmCode = ""
			s.PasskeySkipHandoff = false
			s.Pairing = ""
			s.PairingMethod = ""
			s.mu.Unlock()
			if reconnecting {
				a.emit(id, "session.reconnecting", map[string]any{})
			} else {
				a.emit(id, "session.disconnected", map[string]any{})
			}
		case *events.QR:
			if len(e.Codes) > 0 {
				s.mu.Lock()
				if s.PairingMethod != "phone" {
					s.Pairing = e.Codes[0]
					s.PairingMethod = "qr"
					s.Status = "pairing"
				}
				s.PasskeyPublicKey = nil
				s.PasskeyConfirmCode = ""
				s.PasskeySkipHandoff = false
				s.mu.Unlock()
				a.emit(id, "session.qr", map[string]any{"code": e.Codes[0]})
			}
			s.qrOnce.Do(func() { close(s.qrReady) })
		case *events.Message:
			m := e.Message
			body := m.GetConversation()
			if body == "" && m.GetExtendedTextMessage() != nil {
				body = m.GetExtendedTextMessage().GetText()
			}
			var attachment whatsmeow.DownloadableMessage
			kind := ""
			caption := ""
			if img := m.GetImageMessage(); img != nil {
				attachment, kind, caption = img, "image", img.GetCaption()
			} else if vid := m.GetVideoMessage(); vid != nil {
				attachment, kind, caption = vid, "video", vid.GetCaption()
			} else if aud := m.GetAudioMessage(); aud != nil {
				attachment, kind = aud, "audio"
			} else if doc := m.GetDocumentMessage(); doc != nil {
				attachment, kind, caption = doc, "document", doc.GetCaption()
			} else if st := m.GetStickerMessage(); st != nil {
				attachment, kind = st, "sticker"
			}
			if body == "" {
				body = caption
			}
			pushName := ""
			if ci, ciErr := c.Store.Contacts.GetContact(context.Background(), e.Info.Sender); ciErr == nil {
				pushName = ci.PushName
			}
			data := map[string]any{"id": e.Info.ID, "chat": e.Info.Chat.String(), "sender": e.Info.Sender.String(), "push_name": pushName, "from_me": e.Info.IsFromMe, "text": body, "timestamp": e.Info.Timestamp}
			if attachment != nil {
				queued := true
				select {
				case a.mediaJobs <- mediaJob{sessionID: id, messageID: string(e.Info.ID), client: c, attachment: attachment, kind: kind}:
				default:
					queued = false
				}
				data["media"] = map[string]any{"type": kind, "available": queued, "status": map[bool]string{true: "queued", false: "queue_full"}[queued]}
			}
			a.storeIncomingMessage(id, e.Info, body, kind)
			a.emit(id, "message.received", data)
		case *events.Receipt:
			a.emit(id, "message.receipt", map[string]any{"chat": e.Chat.String(), "sender": e.Sender.String(), "message_ids": e.MessageIDs, "type": e.Type})
		case *events.PairPasskeyRequest:
			s.mu.Lock()
			s.PasskeyPublicKey = e.PublicKey
			s.PasskeyConfirmCode = ""
			s.PasskeySkipHandoff = false
			s.Status = "passkey_required"
			s.mu.Unlock()
			rpID := ""
			if e.PublicKey != nil {
				rpID = e.PublicKey.RelyingPartID
			}
			a.emit(id, "session.passkey_required", map[string]any{"relying_party_id": rpID})
		case *events.PairPasskeyError:
			s.mu.Lock()
			s.Status = "pairing_failed"
			s.PasskeyPublicKey = nil
			s.PasskeyConfirmCode = ""
			s.PasskeySkipHandoff = false
			s.mu.Unlock()
			a.emit(id, "session.passkey_failed", map[string]any{"continuation": e.Continuation})
		case *events.PairPasskeyConfirmation:
			s.mu.Lock()
			s.PasskeyConfirmCode = e.Code
			s.PasskeySkipHandoff = e.SkipHandoffUX
			s.Status = "passkey_confirmation"
			s.mu.Unlock()
			a.emit(id, "session.passkey_confirmation", map[string]any{"confirmation_required": !e.SkipHandoffUX})
		case *events.LoggedOut:
			s.mu.Lock()
			s.Status = "logged_out"
			s.Account = ""
			s.Pairing = ""
			s.PairingMethod = ""
			s.PasskeyPublicKey = nil
			s.PasskeyConfirmCode = ""
			s.PasskeySkipHandoff = false
			s.mu.Unlock()
			a.emit(id, "session.logged_out", map[string]any{"on_connect": e.OnConnect, "reason": e.Reason.String()})
		case *events.TemporaryBan:
			s.mu.Lock()
			s.Status = "temporarily_banned"
			s.Pairing = ""
			s.PairingMethod = ""
			s.PasskeyPublicKey = nil
			s.PasskeyConfirmCode = ""
			s.PasskeySkipHandoff = false
			s.mu.Unlock()
			a.emit(id, "session.temporarily_banned", map[string]any{"code": int(e.Code), "expires_in_seconds": int64(e.Expire.Seconds())})
		case *events.ClientOutdated:
			s.mu.Lock()
			s.Status = "client_outdated"
			s.Pairing = ""
			s.PairingMethod = ""
			s.PasskeyPublicKey = nil
			s.PasskeyConfirmCode = ""
			s.PasskeySkipHandoff = false
			s.mu.Unlock()
			a.emit(id, "session.client_outdated", map[string]any{})
		case *events.StreamReplaced:
			s.mu.Lock()
			s.Status = "stream_replaced"
			s.Pairing = ""
			s.PairingMethod = ""
			s.PasskeyPublicKey = nil
			s.PasskeyConfirmCode = ""
			s.PasskeySkipHandoff = false
			s.mu.Unlock()
			a.emit(id, "session.stream_replaced", map[string]any{})
		case *events.ConnectFailure:
			s.mu.Lock()
			s.Status = "connection_failed"
			s.Pairing = ""
			s.PairingMethod = ""
			s.PasskeyPublicKey = nil
			s.PasskeyConfirmCode = ""
			s.PasskeySkipHandoff = false
			s.mu.Unlock()
			a.emit(id, "session.connect_failed", map[string]any{"reason": e.Reason.String(), "message": e.Message})
		}
	})
	return s
}

// storeIncomingMessage persists one received message for the local chat
// history; failures are non-fatal (history is a convenience mirror).
func (a *app) storeIncomingMessage(id string, info types.MessageInfo, body, kind string) {
	fromMe := 0
	if info.IsFromMe {
		fromMe = 1
	}
	_, _ = a.exec(`INSERT OR IGNORE INTO api_messages(session_id,chat_jid,message_id,sender,from_me,text,media_type,timestamp) VALUES(?,?,?,?,?,?,?,?)`, id, info.Chat.String(), string(info.ID), info.Sender.String(), fromMe, body, kind, info.Timestamp.Unix())
}

func (a *app) passkey(w http.ResponseWriter, r *http.Request, s *session) {
	switch r.Method {
	case http.MethodGet:
		s.opMu.RLock()
		defer s.opMu.RUnlock()
		s.mu.RLock()
		publicKey := s.PasskeyPublicKey
		s.mu.RUnlock()
		if publicKey == nil {
			writeError(w, http.StatusNotFound, "passkey_unavailable", "no passkey challenge is waiting for this session")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{"public_key": publicKey})
	case http.MethodPost:
		s.opMu.Lock()
		defer s.opMu.Unlock()
		s.mu.RLock()
		challengePending := s.PasskeyPublicKey != nil
		s.mu.RUnlock()
		if !challengePending {
			writeError(w, http.StatusConflict, "passkey_unavailable", "no passkey challenge is waiting for this session")
			return
		}
		var response types.WebAuthnResponse
		if err := decodePasskeyResponse(w, r, &response); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if response.ID == "" || response.Type != "public-key" {
			writeError(w, http.StatusBadRequest, "invalid_passkey_response", "a valid WebAuthn public-key assertion is required")
			return
		}
		if err := s.Client.SendPasskeyResponse(r.Context(), &response); err != nil {
			writeError(w, http.StatusBadGateway, "passkey_response_failed", "WhatsApp rejected the passkey assertion")
			return
		}
		s.mu.Lock()
		s.PasskeyPublicKey = nil
		if s.Status == "passkey_required" {
			s.Status = "passkey_processing"
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "passkey_processing"})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func decodePasskeyResponse(w http.ResponseWriter, r *http.Request, dst *types.WebAuthnResponse) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("request must contain one JSON object")
	}
	return nil
}

func (a *app) passkeyConfirmation(w http.ResponseWriter, r *http.Request, s *session) {
	switch r.Method {
	case http.MethodGet:
		s.opMu.RLock()
		defer s.opMu.RUnlock()
		s.mu.RLock()
		code, skip := s.PasskeyConfirmCode, s.PasskeySkipHandoff
		s.mu.RUnlock()
		if code == "" {
			writeError(w, http.StatusNotFound, "passkey_confirmation_unavailable", "no passkey confirmation is waiting")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{"code": code, "confirmation_required": !skip})
	case http.MethodPost:
		s.opMu.Lock()
		defer s.opMu.Unlock()
		var in struct {
			Code    string `json:"code"`
			Confirm bool   `json:"confirm"`
		}
		if err := decode(w, r, &in, 4096); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		s.mu.RLock()
		code, skip := s.PasskeyConfirmCode, s.PasskeySkipHandoff
		s.mu.RUnlock()
		if code == "" || skip {
			writeError(w, http.StatusConflict, "passkey_confirmation_unavailable", "manual confirmation is not pending")
			return
		}
		if !in.Confirm || in.Code != code {
			writeError(w, http.StatusBadRequest, "confirmation_mismatch", "confirm the matching code shown by WhatsApp")
			return
		}
		if err := s.Client.SendPasskeyConfirmation(r.Context()); err != nil {
			writeError(w, http.StatusBadGateway, "passkey_confirmation_failed", "could not confirm passkey pairing")
			return
		}
		s.mu.Lock()
		s.PasskeyConfirmCode = ""
		if s.Status == "passkey_confirmation" {
			s.Status = "connecting"
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "connecting"})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (a *app) processMedia(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-a.mediaJobs:
			media := a.receiveMedia(job.sessionID, job.client, job.attachment, job.kind)
			a.emit(job.sessionID, "message.media", map[string]any{"message_id": job.messageID, "media": media})
		}
	}
}

func (a *app) pairPhone(w http.ResponseWriter, r *http.Request, s *session) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	var in struct {
		Phone string `json:"phone"`
	}
	if err := decode(w, r, &in, 4096); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	digits := strings.Builder{}
	for _, ch := range in.Phone {
		if ch >= '0' && ch <= '9' {
			digits.WriteRune(ch)
		}
	}
	if !phoneInputPattern.MatchString(in.Phone) {
		writeError(w, 400, "invalid_phone", "phone may contain digits, spaces, parentheses, hyphens and a leading plus")
		return
	}
	phone := digits.String()
	if len(phone) < 7 || len(phone) > 15 || strings.HasPrefix(phone, "0") {
		writeError(w, 400, "invalid_phone", "phone must be an international number without a leading zero")
		return
	}
	if err := waitForPhonePairingReady(r.Context(), s.qrReady, phonePairingReadyTimeout); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			writeError(w, 504, "pairing_timeout", "connection was not ready for phone pairing")
			return
		}
		writeError(w, 504, "pairing_timeout", "WhatsApp did not finish preparing phone pairing in time")
		return
	}
	s.mu.RLock()
	status := s.Status
	s.mu.RUnlock()
	if status == "connected" {
		writeError(w, http.StatusConflict, "already_connected", "session already has a paired WhatsApp account")
		return
	}
	if status == "passkey_required" || status == "passkey_processing" || status == "passkey_confirmation" {
		writeError(w, http.StatusConflict, "passkey_pending", "complete the pending passkey step before requesting another phone code")
		return
	}
	if status != "pairing" {
		writeError(w, http.StatusConflict, "pairing_not_ready", "session is not waiting for a pairing code")
		return
	}
	code, err := s.Client.PairPhone(r.Context(), phone, false, whatsmeow.PairClientChrome, "Chrome (Windows)")
	if err != nil {
		writeError(w, 502, "pairing_failed", "could not generate phone pairing code")
		return
	}
	s.mu.Lock()
	s.Pairing = code
	s.PairingMethod = "phone"
	s.Status = "pairing"
	s.mu.Unlock()
	writeJSON(w, 200, map[string]string{"code": code})
}

func waitForPhonePairingReady(ctx context.Context, ready <-chan struct{}, timeout time.Duration) error {
	if ready == nil {
		return errors.New("pairing readiness channel is unavailable")
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errors.New("pairing readiness timed out")
	}
}

func (a *app) connect(w http.ResponseWriter, r *http.Request, s *session) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.RLock()
	status := s.Status
	s.mu.RUnlock()
	if status == "logged_out" {
		writeError(w, http.StatusConflict, "session_logged_out", "remove this session and create it again to pair a new device")
		return
	}
	if s.Client.IsConnected() {
		s.mu.RLock()
		status := s.Status
		s.mu.RUnlock()
		code := http.StatusAccepted
		if status == "connected" {
			code = http.StatusOK
		}
		writeJSON(w, code, map[string]string{"status": status})
		return
	}
	qr, err := s.Client.GetQRChannel(context.WithoutCancel(r.Context()))
	if err != nil && !errors.Is(err, whatsmeow.ErrQRAlreadyConnected) {
		writeError(w, 409, "pairing_unavailable", "could not start pairing")
		return
	}
	if err = s.Client.ConnectContext(context.WithoutCancel(r.Context())); err != nil {
		writeError(w, 502, "upstream_error", "could not connect to WhatsApp")
		return
	}
	if qr != nil {
		go func() {
			for q := range qr {
				if q.Event == "code" {
					s.mu.Lock()
					if s.PairingMethod != "phone" {
						s.Pairing = q.Code
						s.PairingMethod = "qr"
						s.Status = "pairing"
					}
					s.mu.Unlock()
				} else if q.Event == "success" {
					s.mu.Lock()
					s.Pairing = ""
					s.PairingMethod = ""
					s.mu.Unlock()
				} else if q.Event == "timeout" {
					s.mu.Lock()
					s.Pairing = ""
					s.PairingMethod = ""
					s.Status = "disconnected"
					s.mu.Unlock()
				}
			}
		}()
	}
	s.mu.Lock()
	if s.Status == "disconnected" {
		s.Status = "connecting"
	}
	status = s.Status
	s.mu.Unlock()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": status})
}
func (a *app) send(w http.ResponseWriter, r *http.Request, s *session) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		a.sendMedia(w, r, s)
		return
	}
	var in struct {
		To   string `json:"to"`
		Text string `json:"text"`
	}
	if err := decode(w, r, &in, 1<<20); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	if !s.Client.IsConnected() {
		writeError(w, 409, "not_connected", "session is not connected")
		return
	}
	if in.Text == "" || len(in.Text) > 65536 {
		writeError(w, 400, "invalid_request", "text must contain 1 to 65536 bytes")
		return
	}
	jid, err := types.ParseJID(in.To)
	if err != nil || jid.User == "" || (jid.Server != types.DefaultUserServer && jid.Server != types.GroupServer) {
		writeError(w, 400, "invalid_jid", "to must be a user or group JID")
		return
	}
	resp, err := s.Client.SendMessage(r.Context(), jid, &waE2E.Message{Conversation: proto.String(in.Text)}, whatsmeow.SendRequestExtra{Timeout: 12 * time.Second})
	if err != nil {
		writeError(w, 502, "send_failed", "WhatsApp could not send the message")
		return
	}
	_, _ = a.exec(`INSERT OR IGNORE INTO api_messages(session_id,chat_jid,message_id,sender,from_me,text,media_type,timestamp) VALUES(?,?,?,?,?,?,'',?)`, s.ID, jid.String(), string(resp.ID), s.Account, true, in.Text, resp.Timestamp.Unix())
	writeJSON(w, 202, map[string]string{"message_id": string(resp.ID), "timestamp": resp.Timestamp.String()})
}

func (a *app) sendMedia(w http.ResponseWriter, r *http.Request, s *session) {
	r.Body = http.MaxBytesReader(w, r.Body, a.cfg.maxMedia+(1<<20))
	if err := r.ParseMultipartForm(a.cfg.maxMedia); err != nil {
		writeError(w, 400, "invalid_multipart", "multipart form is invalid or too large")
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	to := r.FormValue("to")
	caption := r.FormValue("caption")
	jid, err := validDestination(to)
	if err != nil {
		writeError(w, 400, "invalid_jid", "to must be a valid user or group JID")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, 400, "invalid_request", "file field is required")
		return
	}
	defer file.Close()
	if header.Size < 1 || header.Size > a.cfg.maxMedia {
		writeError(w, 413, "media_too_large", "file exceeds configured media limit")
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, a.cfg.maxMedia+1))
	if err != nil || int64(len(data)) > a.cfg.maxMedia {
		writeError(w, 413, "media_too_large", "file exceeds configured media limit")
		return
	}
	mimeType := header.Header.Get("Content-Type")
	if parsed, _, parseErr := mime.ParseMediaType(mimeType); parseErr == nil {
		mimeType = parsed
	} else {
		mimeType = ""
	}
	if mimeType == "" {
		mimeType = mime.TypeByExtension(filepath.Ext(header.Filename))
	}
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	kind := "document"
	uploadType := whatsmeow.MediaDocument
	if strings.HasPrefix(mimeType, "image/") {
		kind = "image"
		uploadType = whatsmeow.MediaImage
	} else if strings.HasPrefix(mimeType, "video/") {
		kind = "video"
		uploadType = whatsmeow.MediaVideo
	} else if strings.HasPrefix(mimeType, "audio/") {
		kind = "audio"
		uploadType = whatsmeow.MediaAudio
	}
	if !s.Client.IsConnected() {
		writeError(w, 409, "not_connected", "session is not connected")
		return
	}
	resp, err := s.Client.Upload(r.Context(), data, uploadType)
	if err != nil {
		writeError(w, 502, "upload_failed", "WhatsApp media upload failed")
		return
	}
	urlValue, pathValue, length := resp.URL, resp.DirectPath, resp.FileLength
	msg := &waE2E.Message{}
	switch kind {
	case "image":
		msg.ImageMessage = &waE2E.ImageMessage{URL: proto.String(urlValue), DirectPath: proto.String(pathValue), Mimetype: proto.String(mimeType), Caption: proto.String(caption), MediaKey: resp.MediaKey, FileSHA256: resp.FileSHA256, FileEncSHA256: resp.FileEncSHA256, FileLength: proto.Uint64(length)}
	case "video":
		msg.VideoMessage = &waE2E.VideoMessage{URL: proto.String(urlValue), DirectPath: proto.String(pathValue), Mimetype: proto.String(mimeType), Caption: proto.String(caption), MediaKey: resp.MediaKey, FileSHA256: resp.FileSHA256, FileEncSHA256: resp.FileEncSHA256, FileLength: proto.Uint64(length)}
	case "audio":
		msg.AudioMessage = &waE2E.AudioMessage{URL: proto.String(urlValue), DirectPath: proto.String(pathValue), Mimetype: proto.String(mimeType), MediaKey: resp.MediaKey, FileSHA256: resp.FileSHA256, FileEncSHA256: resp.FileEncSHA256, FileLength: proto.Uint64(length)}
	default:
		filename := filepath.Base(header.Filename)
		if filename == "." || filename == string(filepath.Separator) {
			filename = "attachment"
		}
		msg.DocumentMessage = &waE2E.DocumentMessage{URL: proto.String(urlValue), DirectPath: proto.String(pathValue), Mimetype: proto.String(mimeType), Title: proto.String(filename), FileName: proto.String(filename), Caption: proto.String(caption), MediaKey: resp.MediaKey, FileSHA256: resp.FileSHA256, FileEncSHA256: resp.FileEncSHA256, FileLength: proto.Uint64(length)}
	}
	sent, err := s.Client.SendMessage(r.Context(), jid, msg, whatsmeow.SendRequestExtra{Timeout: 12 * time.Second})
	if err != nil {
		writeError(w, 502, "send_failed", "WhatsApp could not send the media")
		return
	}
	_, _ = a.exec(`INSERT OR IGNORE INTO api_messages(session_id,chat_jid,message_id,sender,from_me,text,media_type,timestamp) VALUES(?,?,?,?,?,?,?,?)`, s.ID, jid.String(), string(sent.ID), s.Account, true, caption, kind, sent.Timestamp.Unix())
	writeJSON(w, 202, map[string]string{"message_id": string(sent.ID), "media_type": kind, "timestamp": sent.Timestamp.String()})
}

func validDestination(raw string) (types.JID, error) {
	jid, err := types.ParseJID(raw)
	if err != nil || jid.User == "" || (jid.Server != types.DefaultUserServer && jid.Server != types.GroupServer) {
		return types.JID{}, errors.New("invalid destination")
	}
	return jid, nil
}

func (a *app) receiveMedia(sessionID string, client *whatsmeow.Client, attachment whatsmeow.DownloadableMessage, kind string) map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	data, err := client.Download(ctx, attachment)
	if err != nil {
		a.logger.Warn("incoming media download failed", "media_type", kind, "error", err)
		return map[string]any{"type": kind, "available": false}
	}
	if int64(len(data)) > a.cfg.maxMedia {
		a.logger.Warn("incoming media exceeded configured limit", "media_type", kind)
		return map[string]any{"type": kind, "available": false}
	}
	mimeType := "application/octet-stream"
	var filename string
	switch m := attachment.(type) {
	case *waE2E.ImageMessage:
		mimeType = m.GetMimetype()
	case *waE2E.VideoMessage:
		mimeType = m.GetMimetype()
	case *waE2E.AudioMessage:
		mimeType = m.GetMimetype()
	case *waE2E.StickerMessage:
		mimeType = m.GetMimetype()
	case *waE2E.DocumentMessage:
		mimeType = m.GetMimetype()
		filename = m.GetFileName()
	}
	if filename == "" {
		filename = "media"
	}
	ext := filepath.Ext(filepath.Base(filename))
	if filename == "media" {
		if exts, err := mime.ExtensionsByType(mimeType); err == nil && len(exts) > 0 {
			ext = exts[0]
		}
	}
	if len(ext) > 10 || strings.ContainsAny(ext, "/\\") {
		ext = ""
	}
	name := uuid.NewString() + ext
	if _, err := a.execContext(ctx, `INSERT INTO api_media(id,session_id,payload,content_type,filename,created_at) VALUES(?,?,?,?,?,?)`, strings.TrimSuffix(name, ext), sessionID, data, mimeType, filepath.Base(filename), time.Now().Unix()); err != nil {
		a.logger.Error("store incoming media in Supabase", "session_id", sessionID, "error", err)
		return map[string]any{"type": kind, "available": false}
	}
	return map[string]any{"type": kind, "available": true, "mime_type": mimeType, "filename": filepath.Base(filename), "size": len(data), "url": "/v1/media/" + name}
}

var mediaNamePattern = regexp.MustCompile(`^[a-f0-9-]{36}(\.[A-Za-z0-9]{1,8})?$`)

func (a *app) serveMedia(w http.ResponseWriter, r *http.Request, name string) {
	if !mediaNamePattern.MatchString(name) {
		writeError(w, 404, "not_found", "media not found")
		return
	}
	id := strings.TrimSuffix(name, filepath.Ext(name))
	var data []byte
	var contentType, filename string
	var createdAt int64
	err := a.queryRowContext(r.Context(), `SELECT payload,content_type,filename,created_at FROM api_media WHERE id=?`, id).Scan(&data, &contentType, &filename, &createdAt)
	if err != nil || int64(len(data)) > a.cfg.maxMedia {
		writeError(w, 404, "not_found", "media not found")
		return
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if filename == "" {
		filename = name
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(filepath.Base(filename), "\"", "_")+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, name, time.Unix(createdAt, 0), bytes.NewReader(data))
}
func (a *app) presence(w http.ResponseWriter, r *http.Request, s *session) {
	var in struct {
		State string `json:"state"`
		Chat  string `json:"chat,omitempty"`
	}
	if err := decode(w, r, &in, 4096); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	if !s.Client.IsConnected() {
		writeError(w, 409, "not_connected", "session is not connected")
		return
	}
	var state types.ChatPresence
	media := types.ChatPresenceMediaText
	if in.State == "typing" {
		state = types.ChatPresenceComposing
	} else if in.State == "recording" {
		state = types.ChatPresenceComposing
		media = types.ChatPresenceMediaAudio
	} else if in.State == "paused" {
		state = types.ChatPresencePaused
	} else {
		writeError(w, 400, "invalid_request", "state must be typing, recording or paused")
		return
	}
	var jid types.JID
	if in.Chat != "" {
		v, err := types.ParseJID(in.Chat)
		if err != nil {
			writeError(w, 400, "invalid_jid", "invalid chat JID")
			return
		}
		jid = v
	}
	if err := s.Client.SendChatPresence(r.Context(), jid, state, media); err != nil {
		writeError(w, 502, "presence_failed", "could not update presence")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) contacts(w http.ResponseWriter, r *http.Request, s *session) {
	contacts, err := s.Client.Store.Contacts.GetAllContacts(r.Context())
	if err != nil {
		writeError(w, 502, "contacts_unavailable", "could not read cached contacts")
		return
	}
	out := make([]map[string]string, 0, len(contacts))
	for jid, info := range contacts {
		out = append(out, map[string]string{"jid": jid.String(), "first_name": info.FirstName, "full_name": info.FullName, "push_name": info.PushName, "business_name": info.BusinessName})
	}
	writeJSON(w, 200, out)
}
func (a *app) groups(w http.ResponseWriter, r *http.Request, s *session) {
	if !s.Client.IsConnected() {
		writeError(w, 409, "not_connected", "session is not connected")
		return
	}
	groups, err := s.Client.GetJoinedGroups(r.Context())
	if err != nil {
		writeError(w, 502, "groups_unavailable", "could not query joined groups")
		return
	}
	out := make([]map[string]any, 0, len(groups))
	for _, group := range groups {
		out = append(out, map[string]any{"jid": group.JID.String(), "name": group.Name, "topic": group.Topic, "participant_count": group.ParticipantCount})
	}
	writeJSON(w, 200, out)
}

func (a *app) contactPicture(w http.ResponseWriter, r *http.Request, s *session, raw string) {
	jid, err := types.ParseJID(raw)
	if err != nil || jid.User == "" {
		writeError(w, 400, "invalid_jid", "invalid contact JID")
		return
	}
	info, err := s.Client.GetProfilePictureInfo(r.Context(), jid, &whatsmeow.GetProfilePictureParams{})
	if err != nil {
		writeError(w, 502, "picture_unavailable", "could not fetch profile picture")
		return
	}
	writeJSON(w, 200, map[string]any{"profile_picture_url": info.URL, "picture_id": info.ID})
}

func (a *app) listChats(w http.ResponseWriter, r *http.Request, s *session) {
	rows, err := a.queryContext(r.Context(), `SELECT m.chat_jid, m.text, m.media_type, m.timestamp, (SELECT COUNT(*) FROM api_messages c WHERE c.session_id=m.session_id AND c.chat_jid=m.chat_jid) FROM api_messages m JOIN (SELECT chat_jid, MAX(timestamp) AS mt FROM api_messages WHERE session_id=? GROUP BY chat_jid) x ON m.chat_jid=x.chat_jid AND m.timestamp=x.mt WHERE m.session_id=? ORDER BY m.timestamp DESC LIMIT 200`, s.ID, s.ID)
	if err != nil {
		writeError(w, 500, "storage_error", "could not read chats")
		return
	}
	defer rows.Close()
	out := make([]map[string]any, 0, 16)
	for rows.Next() {
		var chat, text, mediaType string
		var ts, total int64
		if err := rows.Scan(&chat, &text, &mediaType, &ts, &total); err != nil {
			writeError(w, 500, "storage_error", "could not read chats")
			return
		}
		out = append(out, map[string]any{"chat": chat, "last_text": text, "last_media_type": mediaType, "last_timestamp": ts, "message_count": total})
	}
	writeJSON(w, 200, out)
}

func (a *app) chatHistory(w http.ResponseWriter, r *http.Request, s *session, raw string) {
	chat, err := parseChatJID(raw)
	if err != nil {
		writeError(w, 400, "invalid_jid", "invalid chat JID")
		return
	}
	rows, err := a.queryContext(r.Context(), `SELECT message_id, sender, from_me, text, media_type, timestamp FROM api_messages WHERE session_id=? AND chat_jid=? ORDER BY timestamp ASC, id ASC LIMIT 500`, s.ID, chat.String())
	if err != nil {
		writeError(w, 500, "storage_error", "could not read messages")
		return
	}
	defer rows.Close()
	out := make([]map[string]any, 0, 32)
	for rows.Next() {
		var messageID, sender, text, mediaType string
		var fromMe, ts int64
		if err := rows.Scan(&messageID, &sender, &fromMe, &text, &mediaType, &ts); err != nil {
			writeError(w, 500, "storage_error", "could not read messages")
			return
		}
		out = append(out, map[string]any{"id": messageID, "sender": sender, "from_me": fromMe, "text": text, "media_type": mediaType, "timestamp": ts})
	}
	writeJSON(w, 200, out)
}

var productPricePattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]{1,2})?$`)
var productCurrencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

type productInput struct {
	Name        *string `json:"name,omitempty"`
	SKU         *string `json:"sku,omitempty"`
	Description *string `json:"description,omitempty"`
	Price       *string `json:"price,omitempty"`
	Currency    *string `json:"currency,omitempty"`
	ImageURL    *string `json:"image_url,omitempty"`
}

func validateProductFields(in *productInput) error {
	if in.Name != nil {
		*in.Name = strings.TrimSpace(*in.Name)
		if *in.Name == "" || len(*in.Name) > 200 {
			return errors.New("name must contain 1 to 200 characters")
		}
	}
	if in.SKU != nil {
		*in.SKU = strings.TrimSpace(*in.SKU)
		if len(*in.SKU) > 100 {
			return errors.New("sku must contain at most 100 characters")
		}
	}
	if in.Description != nil && len(*in.Description) > 4000 {
		return errors.New("description must contain at most 4000 characters")
	}
	if in.Price != nil {
		*in.Price = strings.TrimSpace(*in.Price)
		if *in.Price != "" && !productPricePattern.MatchString(*in.Price) {
			return errors.New("price must be a plain value like 79.90 without currency symbol")
		}
	}
	if in.Currency != nil {
		*in.Currency = strings.ToUpper(strings.TrimSpace(*in.Currency))
		if !productCurrencyPattern.MatchString(*in.Currency) {
			return errors.New("currency must be a 3-letter ISO code like BRL")
		}
	}
	if in.ImageURL != nil && *in.ImageURL != "" {
		u, err := url.Parse(*in.ImageURL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return errors.New("image_url must be an HTTPS URL")
		}
	}
	return nil
}

func (a *app) listProducts(w http.ResponseWriter, r *http.Request, s *session) {
	rows, err := a.queryContext(r.Context(), `SELECT id, sku, name, description, price, currency, image_url, created_at, updated_at FROM api_products WHERE session_id=? ORDER BY created_at DESC`, s.ID)
	if err != nil {
		writeError(w, 500, "storage_error", "could not read products")
		return
	}
	defer rows.Close()
	out := make([]map[string]any, 0, 16)
	for rows.Next() {
		var id, sku, name, description, price, currency, imageURL string
		var createdAt, updatedAt int64
		if err := rows.Scan(&id, &sku, &name, &description, &price, &currency, &imageURL, &createdAt, &updatedAt); err != nil {
			writeError(w, 500, "storage_error", "could not read products")
			return
		}
		out = append(out, map[string]any{"id": id, "sku": sku, "name": name, "description": description, "price": price, "currency": currency, "image_url": imageURL, "created_at": createdAt, "updated_at": updatedAt})
	}
	writeJSON(w, 200, out)
}

func (a *app) createProduct(w http.ResponseWriter, r *http.Request, s *session) {
	var in productInput
	if err := decode(w, r, &in, 64<<10); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	if in.Name == nil {
		writeError(w, 400, "invalid_product", "name is required")
		return
	}
	if in.Price == nil {
		in.Price = new(string)
	}
	if in.Currency == nil || *in.Currency == "" {
		in.Currency = new(string)
		*in.Currency = "BRL"
	}
	if err := validateProductFields(&in); err != nil {
		writeError(w, 400, "invalid_product", err.Error())
		return
	}
	id := uuid.NewString()
	now := time.Now().Unix()
	sku, description, price, currency, imageURL := "", "", "", *in.Currency, ""
	if in.SKU != nil {
		sku = *in.SKU
	}
	if in.Description != nil {
		description = *in.Description
	}
	if in.Price != nil {
		price = *in.Price
	}
	if in.ImageURL != nil {
		imageURL = *in.ImageURL
	}
	if _, err := a.execContext(r.Context(), `INSERT INTO api_products(id,session_id,sku,name,description,price,currency,image_url,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, id, s.ID, sku, *in.Name, description, price, currency, imageURL, now, now); err != nil {
		writeError(w, 500, "storage_error", "could not create product")
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "sku": sku, "name": *in.Name, "description": description, "price": price, "currency": currency, "image_url": imageURL, "created_at": now, "updated_at": now})
}

func (a *app) updateProduct(w http.ResponseWriter, r *http.Request, s *session, id string) {
	var in productInput
	if err := decode(w, r, &in, 64<<10); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	if in.Name == nil && in.SKU == nil && in.Description == nil && in.Price == nil && in.Currency == nil && in.ImageURL == nil {
		writeError(w, 400, "invalid_product", "provide at least one product field to update")
		return
	}
	if err := validateProductFields(&in); err != nil {
		writeError(w, 400, "invalid_product", err.Error())
		return
	}
	sets, args := []string{"updated_at=?"}, []any{time.Now().Unix()}
	for field, value := range map[string]*string{"sku": in.SKU, "name": in.Name, "description": in.Description, "price": in.Price, "currency": in.Currency, "image_url": in.ImageURL} {
		if value != nil {
			sets = append(sets, field+"=?")
			args = append(args, *value)
		}
	}
	args = append(args, s.ID, id)
	res, err := a.execContext(r.Context(), `UPDATE api_products SET `+strings.Join(sets, ", ")+` WHERE session_id=? AND id=?`, args...)
	if err != nil {
		writeError(w, 500, "storage_error", "could not update product")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, 404, "not_found", "product not found")
		return
	}
	var idOut, sku, name, description, price, currency, imageURL string
	var createdAt, updatedAt int64
	err = a.queryRowContext(r.Context(), `SELECT id, sku, name, description, price, currency, image_url, created_at, updated_at FROM api_products WHERE session_id=? AND id=?`, s.ID, id).Scan(&idOut, &sku, &name, &description, &price, &currency, &imageURL, &createdAt, &updatedAt)
	if err != nil {
		writeError(w, 500, "storage_error", "could not read product")
		return
	}
	writeJSON(w, 200, map[string]any{"id": idOut, "sku": sku, "name": name, "description": description, "price": price, "currency": currency, "image_url": imageURL, "created_at": createdAt, "updated_at": updatedAt})
}

func (a *app) deleteProduct(w http.ResponseWriter, r *http.Request, s *session, id string) {
	res, err := a.execContext(r.Context(), `DELETE FROM api_products WHERE session_id=? AND id=?`, s.ID, id)
	if err != nil {
		writeError(w, 500, "storage_error", "could not delete product")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, 404, "not_found", "product not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func parseChatJID(raw string) (types.JID, error) {
	jid, err := types.ParseJID(raw)
	if err != nil || jid.User == "" || (jid.Server != types.DefaultUserServer && jid.Server != types.GroupServer) {
		return types.JID{}, errors.New("invalid chat JID")
	}
	return jid, nil
}
func (a *app) getChatSettings(w http.ResponseWriter, r *http.Request, s *session, raw string) {
	chat, err := parseChatJID(raw)
	if err != nil {
		writeError(w, 400, "invalid_jid", "chat must be a valid user or group JID")
		return
	}
	settings, err := s.Client.Store.ChatSettings.GetChatSettings(r.Context(), chat)
	if err != nil {
		writeError(w, 502, "chat_settings_unavailable", "could not read local chat settings")
		return
	}
	writeJSON(w, 200, map[string]any{"chat": chat.String(), "found": settings.Found, "archived": settings.Archived, "pinned": settings.Pinned, "muted_until": settings.MutedUntil})
}
func (a *app) updateChatSettings(w http.ResponseWriter, r *http.Request, s *session, raw string) {
	chat, err := parseChatJID(raw)
	if err != nil {
		writeError(w, 400, "invalid_jid", "chat must be a valid user or group JID")
		return
	}
	var in struct {
		Archived   *bool   `json:"archived,omitempty"`
		Pinned     *bool   `json:"pinned,omitempty"`
		MutedUntil *string `json:"muted_until,omitempty"`
	}
	if err = decode(w, r, &in, 4096); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	if in.Archived == nil && in.Pinned == nil && in.MutedUntil == nil {
		writeError(w, 400, "invalid_request", "provide at least one setting to update")
		return
	}
	var muted time.Time
	if in.MutedUntil != nil && *in.MutedUntil != "" {
		muted, err = time.Parse(time.RFC3339, *in.MutedUntil)
		if err != nil {
			writeError(w, 400, "invalid_request", "muted_until must be RFC3339 or empty to unmute")
			return
		}
	}
	settings := s.Client.Store.ChatSettings
	if in.Archived != nil {
		if err = settings.PutArchived(r.Context(), chat, *in.Archived); err != nil {
			writeError(w, 502, "chat_settings_failed", "could not update archived state")
			return
		}
	}
	if in.Pinned != nil {
		if err = settings.PutPinned(r.Context(), chat, *in.Pinned); err != nil {
			writeError(w, 502, "chat_settings_failed", "could not update pinned state")
			return
		}
	}
	if in.MutedUntil != nil {
		if err = settings.PutMutedUntil(r.Context(), chat, muted); err != nil {
			writeError(w, 502, "chat_settings_failed", "could not update muted state")
			return
		}
	}
	a.getChatSettings(w, r, s, raw)
}
func (a *app) readMessage(w http.ResponseWriter, r *http.Request, s *session, id string) {
	var in struct {
		Chat   string `json:"chat"`
		Sender string `json:"sender,omitempty"`
	}
	if err := decode(w, r, &in, 4096); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	chat, err := types.ParseJID(in.Chat)
	if err != nil {
		writeError(w, 400, "invalid_jid", "invalid chat JID")
		return
	}
	sender := chat
	if chat.Server == types.GroupServer && in.Sender == "" {
		writeError(w, 400, "invalid_request", "sender is required for group message receipts")
		return
	}
	if in.Sender != "" {
		sender, err = types.ParseJID(in.Sender)
		if err != nil {
			writeError(w, 400, "invalid_jid", "invalid sender JID")
			return
		}
	}
	if err = s.Client.MarkRead(r.Context(), []types.MessageID{types.MessageID(id)}, time.Now(), chat, sender); err != nil {
		writeError(w, 502, "read_failed", "could not mark message read")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (a *app) list(w http.ResponseWriter, _ *http.Request) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]map[string]string, 0, len(a.sessions))
	for _, s := range a.sessions {
		s.mu.RLock()
		out = append(out, map[string]string{"id": s.ID, "status": s.Status, "account": s.Account, "pairing_method": s.PairingMethod})
		s.mu.RUnlock()
	}
	writeJSON(w, 200, out)
}
func (a *app) getWebhookSettings(w http.ResponseWriter, _ *http.Request) {
	url, secret, token := a.webhookSettings()
	writeJSON(w, 200, map[string]any{"webhook_url": url, "webhook_secret_set": secret != "", "webhook_token_set": token != "", "stored_in_db": a.setting("webhook_url") != ""})
}

func (a *app) saveWebhookSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		WebhookURL    *string `json:"webhook_url,omitempty"`
		WebhookSecret *string `json:"webhook_secret,omitempty"`
		WebhookToken  *string `json:"webhook_token,omitempty"`
	}
	if err := decode(w, r, &in, 8192); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	if in.WebhookURL != nil {
		trimmed := strings.TrimSpace(*in.WebhookURL)
		if trimmed != "" {
			u, err := url.Parse(trimmed)
			if err != nil || u.Host == "" || (u.Scheme != "https" && !(a.cfg.webhookAllowPrivate && u.Scheme == "http")) {
				writeError(w, 400, "invalid_webhook_url", "a URL deve ser HTTPS (ou HTTP quando WEBHOOK_ALLOW_PRIVATE=1)")
				return
			}
			a.setSetting("webhook_url", trimmed)
		} else {
			a.clearSetting("webhook_url")
		}
	}
	if in.WebhookSecret != nil {
		if *in.WebhookSecret == "" {
			a.clearSetting("webhook_secret")
		} else {
			a.setSetting("webhook_secret", *in.WebhookSecret)
		}
	}
	if in.WebhookToken != nil {
		if *in.WebhookToken == "" {
			a.clearSetting("webhook_token")
		} else {
			a.setSetting("webhook_token", *in.WebhookToken)
		}
	}
	a.getWebhookSettings(w, r)
}

func (a *app) get(w http.ResponseWriter, id string) {
	s := a.find(id)
	if s == nil {
		writeError(w, 404, "not_found", "session not found")
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	writeJSON(w, 200, map[string]string{"id": s.ID, "status": s.Status, "account": s.Account, "pairing_method": s.PairingMethod})
}
func (a *app) find(id string) *session { a.mu.RLock(); defer a.mu.RUnlock(); return a.sessions[id] }

// Configuração efetiva de webhook: valor salvo no banco (via painel) tem
// precedência; o ambiente serve apenas como fallback de bootstrap.
func (a *app) setting(key string) string {
	var v string
	_ = a.queryRow(`SELECT value FROM app_settings WHERE key=?`, key).Scan(&v)
	return v
}
func (a *app) setSetting(key, value string) {
	_, _ = a.exec(`INSERT INTO app_settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
}
func (a *app) clearSetting(key string) { _, _ = a.exec(`DELETE FROM app_settings WHERE key=?`, key) }
func (a *app) webhookSettings() (url, secret, token string) {
	url = a.setting("webhook_url")
	if url == "" {
		url = a.cfg.webhook
	}
	secret = a.setting("webhook_secret")
	if secret == "" {
		secret = a.cfg.webhookSecret
	}
	token = a.setting("webhook_token")
	if token == "" {
		token = a.cfg.webhookToken
	}
	return url, secret, token
}
func (a *app) remove(w http.ResponseWriter, r *http.Request, id string) {
	s := a.find(id)
	if s == nil {
		writeError(w, 404, "not_found", "session not found")
		return
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	// Confirm the same session is still registered after waiting for an in-flight
	// connect/disconnect request.
	if a.find(id) != s {
		writeError(w, 404, "not_found", "session not found")
		return
	}
	if s.Client.IsConnected() {
		s.mu.RLock()
		loggedOut := s.Status == "logged_out"
		s.mu.RUnlock()
		if !loggedOut {
			if err := s.Client.Logout(r.Context()); err != nil {
				// An unpaired session (still pairing or connecting) has nothing
				// registered on WhatsApp to log out; discard local credentials.
				s.mu.RLock()
				unpaired := s.Account == ""
				s.mu.RUnlock()
				if !unpaired {
					writeError(w, 502, "logout_failed", "could not log out WhatsApp device")
					return
				}
				s.Client.Disconnect()
			}
		} else {
			s.Client.Disconnect()
		}
	} else {
		s.Client.Disconnect()
	}
	// A device that never paired was never persisted, so there is nothing to
	// delete; DeleteDevice would fail with ErrDeviceIDMustBeSet.
	if s.Client.Store.ID != nil {
		if err := s.Client.Store.Delete(r.Context()); err != nil {
			writeError(w, 500, "storage_error", "could not delete session credentials")
			return
		}
	}
	a.mu.Lock()
	delete(a.sessions, id)
	a.mu.Unlock()
	_, err := a.execContext(r.Context(), `DELETE FROM api_sessions WHERE id=?`, id)
	if err != nil {
		writeError(w, 500, "storage_error", "could not remove session metadata")
		return
	}
	if _, err := a.execContext(r.Context(), `DELETE FROM api_media WHERE session_id=?`, id); err != nil {
		a.logger.Warn("remove session media", "session_id", id, "error", err)
	}
	if _, err := a.execContext(r.Context(), `DELETE FROM api_messages WHERE session_id=?`, id); err != nil {
		a.logger.Warn("remove session chat history", "session_id", id, "error", err)
	}
	if _, err := a.execContext(r.Context(), `DELETE FROM api_products WHERE session_id=?`, id); err != nil {
		a.logger.Warn("remove session products", "session_id", id, "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}
func (a *app) restore(ctx context.Context) error {
	rows, err := a.queryContext(ctx, `SELECT id,device_id FROM api_sessions`)
	if err != nil {
		return err
	}
	// Close the result set before reconnecting clients. Reconnection events can
	// update api_sessions, and keeping a SQLite cursor open can block those writes.
	type persistedSession struct{ id, deviceID string }
	var persisted []persistedSession
	for rows.Next() {
		var id, deviceID string
		if err = rows.Scan(&id, &deviceID); err != nil {
			return err
		}
		persisted = append(persisted, persistedSession{id: id, deviceID: deviceID})
	}
	rowsErr := rows.Err()
	if closeErr := rows.Close(); rowsErr == nil {
		rowsErr = closeErr
	}
	if rowsErr != nil {
		return rowsErr
	}
	for _, saved := range persisted {
		id, deviceID := saved.id, saved.deviceID
		deviceJID, parseErr := types.ParseJID(deviceID)
		if parseErr != nil {
			device := a.store.NewDevice()
			a.sessions[id] = a.makeSession(device, id)
			continue
		}
		device, err := a.store.GetDevice(ctx, deviceJID)
		if err != nil {
			a.logger.Error("load stored device", "session_id", id, "error", err)
			// Keep this API session visible and independently recoverable even if
			// one device record is corrupt or temporarily unreadable.
			device = a.store.NewDevice()
			if _, updateErr := a.execContext(ctx, `UPDATE api_sessions SET device_id=? WHERE id=?`, uuid.NewString(), id); updateErr != nil {
				a.logger.Error("reset unavailable device mapping", "session_id", id, "error", updateErr)
			}
		}
		if device == nil {
			device = a.store.NewDevice()
			if _, updateErr := a.execContext(ctx, `UPDATE api_sessions SET device_id=? WHERE id=?`, uuid.NewString(), id); updateErr != nil {
				a.logger.Error("reset missing device mapping", "session_id", id, "error", updateErr)
			}
		}
		s := a.makeSession(device, id)
		a.sessions[id] = s
		if device.ID != nil {
			s.mu.Lock()
			s.Status = "connecting"
			s.mu.Unlock()
			if err = s.Client.Connect(); err != nil {
				s.mu.Lock()
				if s.Status == "connecting" {
					s.Status = "connection_failed"
				}
				s.mu.Unlock()
				a.logger.Warn("session reconnect failed", "session_id", id, "error", err)
			}
		}
	}
	return nil
}
func (a *app) close() {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, s := range a.sessions {
		s.Client.Disconnect()
	}
}
func (a *app) emit(sid, typ string, data any) {
	ev := event{ID: uuid.NewString(), Type: typ, SessionID: sid, Timestamp: time.Now().UTC(), Data: data}
	url, _, _ := a.webhookSettings()
	if url == "" {
		return
	}
	body, err := json.Marshal(ev)
	if err != nil {
		a.logger.Error("serialize webhook event", "type", typ, "error", err)
		return
	}
	if _, err = a.exec(`INSERT OR IGNORE INTO webhook_events(id,payload,attempts,available_at,created_at) VALUES(?,?,0,?,?)`, ev.ID, body, time.Now().Unix(), time.Now().Unix()); err != nil {
		a.logger.Error("persist webhook event", "type", typ, "session_id", sid, "error", err)
		return
	}
	select {
	case a.events <- ev:
	default:
	}
}
func (a *app) deliverEvents(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.events:
		case <-ticker.C:
		}
		for !a.deliverOne(ctx) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
}
func (a *app) deliverOne(parent context.Context) bool {
	var id string
	var body []byte
	err := a.queryRow(`SELECT id,payload FROM webhook_events WHERE available_at<=? ORDER BY created_at LIMIT 1`, time.Now().Unix()).Scan(&id, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return true
	}
	if err != nil {
		a.logger.Error("read webhook queue", "error", err)
		return true
	}
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	url, secret, token := a.webhookSettings()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err == nil {
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write(body)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Webhook-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		req.Header.Set("X-Event-ID", id)
		if token != "" {
			req.Header.Set("X-Webhook-Token", token)
		}
		var resp *http.Response
		resp, err = a.httpClient.Do(req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			if resp.StatusCode < 300 {
				_, err = a.execContext(ctx, `DELETE FROM webhook_events WHERE id=?`, id)
				return true
			}
			err = fmt.Errorf("webhook returned status %d", resp.StatusCode)
		}
	}
	var attempts int
	_ = a.queryRowContext(ctx, `SELECT attempts FROM webhook_events WHERE id=?`, id).Scan(&attempts)
	attempts++
	if attempts >= 12 {
		_, _ = a.execContext(ctx, `DELETE FROM webhook_events WHERE id=?`, id)
		a.logger.Warn("webhook event discarded after retry limit", "event_id", id)
		return true
	}
	delay := time.Second * time.Duration(1<<min(attempts-1, 8))
	_, updateErr := a.execContext(ctx, `UPDATE webhook_events SET attempts=?,available_at=? WHERE id=?`, attempts, time.Now().Add(delay).Unix(), id)
	if updateErr != nil {
		a.logger.Error("schedule webhook retry", "event_id", id, "error", updateErr)
	}
	if err != nil {
		a.logger.Warn("webhook delivery failed", "event_id", id, "attempt", attempts, "error", err)
	}
	return true
}
func decode(w http.ResponseWriter, r *http.Request, dst any, max int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("request must contain one JSON object")
	}
	return nil
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
