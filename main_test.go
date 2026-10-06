package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAuthRequiresExactBearerToken(t *testing.T) {
	a := &app{cfg: config{token: strings.Repeat("x", 32)}}
	h := a.auth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, tc := range []struct {
		header string
		want   int
	}{{"", 401}, {"Bearer " + strings.Repeat("x", 32), 204}, {"Bearer " + strings.Repeat("x", 32) + "junk", 401}, {strings.Repeat("x", 32), 401}} {
		r := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
		r.Header.Set("Authorization", tc.header)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("header %q: got %d want %d", tc.header, w.Code, tc.want)
		}
	}
}

func TestDecodeRejectsUnknownAndTrailingJSON(t *testing.T) {
	for _, body := range []string{`{"text":"hello","extra":true}`, `{"text":"hello"} {}`} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		w := httptest.NewRecorder()
		var value struct {
			Text string `json:"text"`
		}
		if err := decode(w, r, &value, 100); err == nil {
			t.Fatalf("accepted invalid JSON %s", body)
		}
	}
}

func TestDestinationValidation(t *testing.T) {
	for _, valid := range []string{"15551234567@s.whatsapp.net", "123-456@g.us"} {
		if _, err := validDestination(valid); err != nil {
			t.Errorf("valid destination %q rejected: %v", valid, err)
		}
	}
	for _, invalid := range []string{"", "example.com", "123@broadcast", "@s.whatsapp.net"} {
		if _, err := validDestination(invalid); err == nil {
			t.Errorf("invalid destination %q accepted", invalid)
		}
	}
}

func TestWaitForPhonePairingReadyWaitsForWhatsAppHandshake(t *testing.T) {
	ready := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(ready)
	}()
	started := time.Now()
	if err := waitForPhonePairingReady(context.Background(), ready, time.Second); err != nil {
		t.Fatalf("wait for pairing readiness: %v", err)
	}
	if time.Since(started) < 15*time.Millisecond {
		t.Fatal("pairing proceeded before WhatsApp signaled readiness")
	}
}

func TestWaitForPhonePairingReadyReturnsOnTimeoutAndCancellation(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		if err := waitForPhonePairingReady(context.Background(), make(chan struct{}), 10*time.Millisecond); err == nil {
			t.Fatal("wait succeeded without a readiness event")
		}
	})
	t.Run("cancelled request", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := waitForPhonePairingReady(ctx, make(chan struct{}), time.Second); !errors.Is(err, context.Canceled) {
			t.Fatalf("wait error = %v, want context.Canceled", err)
		}
	})
	t.Run("invalid channel", func(t *testing.T) {
		if err := waitForPhonePairingReady(context.Background(), nil, time.Second); err == nil {
			t.Fatal("nil readiness channel must fail immediately")
		}
	})
}

func TestPostgresQueriesUseDollarBindingsAndConflictHandling(t *testing.T) {
	a := &app{cfg: config{dbDialect: "postgres"}}
	got := a.bindSQL(`INSERT OR IGNORE INTO api_messages(session_id,chat_jid,message_id) VALUES(?,?,?)`)
	want := `INSERT INTO api_messages(session_id,chat_jid,message_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`
	if got != want {
		t.Fatalf("bound query = %q, want %q", got, want)
	}
	got = a.bindSQL(`UPDATE api_products SET updated_at=? WHERE session_id=? AND id=?`)
	want = `UPDATE api_products SET updated_at=$1 WHERE session_id=$2 AND id=$3`
	if got != want {
		t.Fatalf("bound update = %q, want %q", got, want)
	}
}

func TestLoadConfigRejectsNonPostgresDatabase(t *testing.T) {
	t.Setenv("API_TOKEN", strings.Repeat("p", 32))
	t.Setenv("DATABASE_DSN", "file:whatsapp.db")
	if _, err := loadConfig(); err == nil {
		t.Fatal("accepted a local file database instead of Supabase Postgres")
	}
}

func TestLoadConfigValidatesLimitsAndWebhook(t *testing.T) {
	t.Setenv("API_TOKEN", strings.Repeat("s", 32))
	t.Setenv("LISTEN_ADDR", "127.0.0.1:8080")
	t.Setenv("DATABASE_DSN", "postgres://wa:secret@127.0.0.1:5432/whatsapp?sslmode=disable")
	t.Setenv("MAX_MEDIA_BYTES", "1024x")
	if _, err := loadConfig(); err == nil {
		t.Fatal("accepted malformed media size")
	}
	t.Setenv("MAX_MEDIA_BYTES", "1024")
	t.Setenv("WEBHOOK_URL", "http://example.org/hook")
	t.Setenv("WEBHOOK_SECRET", "secret")
	if _, err := loadConfig(); err == nil {
		t.Fatal("accepted non-HTTPS webhook")
	}
	t.Setenv("WEBHOOK_URL", "")
	t.Setenv("WEBHOOK_SECRET", "")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.maxMedia != 1024 {
		t.Fatalf("media limit = %d", cfg.maxMedia)
	}
}

func TestLoadConfigBindsRenderPortOnExternalInterface(t *testing.T) {
	t.Setenv("API_TOKEN", strings.Repeat("r", 32))
	t.Setenv("LISTEN_ADDR", "")
	t.Setenv("PORT", "10000")
	t.Setenv("DATABASE_DSN", "postgres://wa:secret@127.0.0.1:5432/whatsapp?sslmode=disable")
	t.Setenv("WEBHOOK_URL", "")
	t.Setenv("WEBHOOK_SECRET", "")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.addr != "0.0.0.0:10000" {
		t.Fatalf("Render address = %q, want 0.0.0.0:10000", cfg.addr)
	}
}

func TestLoadConfigUsesLoopbackForLocalDefault(t *testing.T) {
	t.Setenv("API_TOKEN", strings.Repeat("l", 32))
	t.Setenv("LISTEN_ADDR", "")
	t.Setenv("PORT", "")
	t.Setenv("DATABASE_DSN", "postgres://wa:secret@127.0.0.1:5432/whatsapp?sslmode=disable")
	t.Setenv("WEBHOOK_URL", "")
	t.Setenv("WEBHOOK_SECRET", "")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.addr != "127.0.0.1:8080" {
		t.Fatalf("local address = %q, want 127.0.0.1:8080", cfg.addr)
	}
}

func TestWebhookSignature(t *testing.T) {
	secret := []byte("test secret")
	payload := []byte(`{"id":"event-1"}`)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(payload)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	provided, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil || !hmac.Equal(provided, mac.Sum(nil)) {
		t.Fatal("signature should verify")
	}
	bad := append(bytes.Clone(provided), 0)
	if hmac.Equal(bad, mac.Sum(nil)) {
		t.Fatal("invalid signature verified")
	}
}

func TestRateLimiterReturns429AfterBurst(t *testing.T) {
	a := &app{rate: make(map[string]bucket)}
	h := a.rateLimit(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	for i := 0; i < 61; i++ {
		r := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
		r.RemoteAddr = "192.0.2.5:1234"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if i < 60 && w.Code != 204 {
			t.Fatalf("request %d unexpectedly got %d", i, w.Code)
		}
		if i == 60 && w.Code != 429 {
			t.Fatalf("request 61 got %d, want 429", w.Code)
		}
	}
}

func TestWebhookDialRejectsPrivateAddress(t *testing.T) {
	if isPublicIP(nil) || isPublicIP([]byte{127, 0, 0, 1}) || isPublicIP([]byte{10, 1, 2, 3}) {
		t.Fatal("private IP accepted")
	}
	if !isPublicIP([]byte{1, 1, 1, 1}) {
		t.Fatal("public IP rejected")
	}
}

func TestWebhookWorkerStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &app{events: make(chan event, 1)}
	done := make(chan struct{})
	go func() { a.deliverEvents(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}

func TestValidateProductFields(t *testing.T) {
	name := "Camiseta"
	price := "79.90"
	currency := "brl"
	in := productInput{Name: &name, Price: &price, Currency: &currency}
	if err := validateProductFields(&in); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	if *in.Currency != "BRL" {
		t.Fatalf("currency not normalized: %q", *in.Currency)
	}
	bad := []productInput{
		{Name: new(string)},
		{Name: ptrString("x"), Price: ptrString("R$ 10")},
		{Name: ptrString("x"), Currency: ptrString("REAL")},
		{Name: ptrString("x"), ImageURL: ptrString("http://insecure.example/a.jpg")},
	}
	for i, p := range bad {
		if err := validateProductFields(&p); err == nil {
			t.Fatalf("case %d accepted invalid product %+v", i, p)
		}
	}
}

func ptrString(s string) *string { return &s }
