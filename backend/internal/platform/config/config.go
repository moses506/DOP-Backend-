// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env         string // development | production
	HTTPAddr    string
	DatabaseURL string
	JWTSecret   []byte
	AccessTTL   time.Duration // absolute session lifetime
	// SessionIdle ends a session after this long without the person using the app.
	SessionIdle time.Duration
	// TrustedProxies says whose X-Forwarded-For header to believe for the
	// client's address: "" (none — the app faces the internet directly),
	// "private" (a reverse proxy on a private network, e.g. nginx or Docker),
	// or a comma-separated list of proxy addresses/CIDRs.
	TrustedProxies string
	// DataKey encrypts secrets kept in the database, such as authenticator-app
	// keys. From DATA_ENCRYPTION_KEY, or JWT_SECRET if that isn't set.
	DataKey []byte
	// MFARoles must use two-step verification at sign-in.
	MFARoles    []string
	CORSOrigins []string
	Location    *time.Location // business timezone, used for year-based numbering

	// AppURL is the public address of the web app, used in links inside emails.
	AppURL string

	// Outgoing email. With SMTPHost empty, emails are written to the log instead.
	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	SMTPPassword string
	SMTPFrom     string
	SMTPFromName string

	// Company is printed on quotations and other documents.
	Company Company
	// QuoteApprovalDiscount: quotes with a discount above this percentage need a
	// sales manager's approval before they can be sent.
	QuoteApprovalDiscount float64
	// QuoteValidDays is how long a new quotation is valid for by default.
	QuoteValidDays int
}

// Company details for document letterheads (COMPANY_* settings). Leave a
// setting empty to leave that line off the documents.
type Company struct {
	Name    string `json:"name"`
	Tagline string `json:"tagline"`
	Address string `json:"address"` // may span lines, separated by "|" in the setting
	Phone   string `json:"phone"`
	Email   string `json:"email"`
	Website string `json:"website"`
	TPIN    string `json:"tpin"`
	Bank    string `json:"bank"` // bank details for payment, lines separated by "|"
	// Bank accounts printed on quotations: the account in the quotation's
	// currency is shown first. Lines separated by "|".
	BankZMW string `json:"bank_zmw"`
	BankUSD string `json:"bank_usd"`
}

func (c Config) IsProduction() bool { return c.Env == "production" }

func Load() (Config, error) {
	cfg := Config{
		Env:         get("APP_ENV", "development"),
		HTTPAddr:    get("HTTP_ADDR", ":8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		JWTSecret:   []byte(os.Getenv("JWT_SECRET")),
		CORSOrigins: split(get("CORS_ORIGINS", "http://localhost:5173")),
	}

	if cfg.DatabaseURL == "" {
		return cfg, errors.New("DATABASE_URL is required")
	}
	if len(cfg.JWTSecret) < 32 {
		return cfg, errors.New("JWT_SECRET must be at least 32 characters")
	}

	ttl, err := time.ParseDuration(get("ACCESS_TOKEN_TTL", "12h"))
	if err != nil {
		return cfg, fmt.Errorf("ACCESS_TOKEN_TTL: %w", err)
	}
	cfg.AccessTTL = ttl
	if cfg.SessionIdle, err = time.ParseDuration(get("SESSION_IDLE_TIMEOUT", "60m")); err != nil {
		return cfg, fmt.Errorf("SESSION_IDLE_TIMEOUT: %w", err)
	}
	cfg.TrustedProxies = get("TRUSTED_PROXIES", "")
	cfg.DataKey = []byte(os.Getenv("DATA_ENCRYPTION_KEY"))
	if len(cfg.DataKey) == 0 {
		cfg.DataKey = cfg.JWTSecret
	} else if len(cfg.DataKey) < 32 {
		return cfg, errors.New("DATA_ENCRYPTION_KEY must be at least 32 characters")
	}
	cfg.MFARoles = split(get("MFA_REQUIRED_ROLES", "admin,ceo,sales_manager"))

	loc, err := time.LoadLocation(get("APP_TIMEZONE", "Africa/Lusaka"))
	if err != nil {
		return cfg, fmt.Errorf("APP_TIMEZONE: %w", err)
	}
	cfg.Location = loc

	cfg.AppURL = strings.TrimRight(get("APP_URL", "http://localhost:5173"), "/")
	cfg.SMTPHost = get("SMTP_HOST", "")
	cfg.SMTPUsername = os.Getenv("SMTP_USERNAME")
	cfg.SMTPPassword = os.Getenv("SMTP_PASSWORD")
	cfg.SMTPFrom = get("SMTP_FROM", "no-reply@doslimited.com")
	cfg.SMTPFromName = get("SMTP_FROM_NAME", "DOS BPA")
	port, err := strconv.Atoi(get("SMTP_PORT", "587"))
	if err != nil {
		return cfg, fmt.Errorf("SMTP_PORT: %w", err)
	}
	cfg.SMTPPort = port

	lines := func(v string) string { return strings.ReplaceAll(v, "|", "\n") }
	cfg.Company = Company{
		Name:    get("COMPANY_NAME", "Digital Office Solutions Limited"),
		Tagline: get("COMPANY_TAGLINE", "Business Solutions Beyond Borders"),
		Address: lines(get("COMPANY_ADDRESS", "Lusaka, Zambia")),
		Phone:   os.Getenv("COMPANY_PHONE"),
		Email:   os.Getenv("COMPANY_EMAIL"),
		Website: get("COMPANY_WEBSITE", "www.doslimited.com"),
		TPIN:    os.Getenv("COMPANY_TPIN"),
		Bank:    lines(os.Getenv("COMPANY_BANK_DETAILS")),
		BankZMW: lines(os.Getenv("COMPANY_BANK_ZMW")),
		BankUSD: lines(os.Getenv("COMPANY_BANK_USD")),
	}
	if cfg.QuoteApprovalDiscount, err = strconv.ParseFloat(get("QUOTE_APPROVAL_DISCOUNT", "10"), 64); err != nil ||
		cfg.QuoteApprovalDiscount < 0 || cfg.QuoteApprovalDiscount > 100 {
		return cfg, errors.New("QUOTE_APPROVAL_DISCOUNT must be a percentage between 0 and 100")
	}
	if cfg.QuoteValidDays, err = strconv.Atoi(get("QUOTE_VALID_DAYS", "30")); err != nil || cfg.QuoteValidDays < 1 || cfg.QuoteValidDays > 365 {
		return cfg, errors.New("QUOTE_VALID_DAYS must be a number of days between 1 and 365")
	}

	if cfg.IsProduction() {
		if err := cfg.checkProduction(); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

// checkProduction refuses to start a production server with settings that
// would leave it exposed. Each message says what to fix.
func (c Config) checkProduction() error {
	var problems []string
	secret := string(c.JWTSecret)
	if len(c.JWTSecret) < 48 || strings.Contains(strings.ToLower(secret), "change") || strings.Contains(strings.ToLower(secret), "example") {
		problems = append(problems, "JWT_SECRET must be a random value of at least 48 characters (e.g. `openssl rand -base64 48`), not the example")
	}
	if !strings.HasPrefix(c.AppURL, "https://") {
		problems = append(problems, "APP_URL must start with https:// — sign-in cookies are only sent over HTTPS in production")
	}
	for _, o := range c.CORSOrigins {
		if o == "*" || !strings.HasPrefix(o, "https://") {
			problems = append(problems, "CORS_ORIGINS must list your https:// web address(es) only, not "+o)
		}
	}
	if strings.Contains(c.DatabaseURL, "sslmode=disable") && !strings.Contains(c.DatabaseURL, "@localhost") && !strings.Contains(c.DatabaseURL, "@127.0.0.1") && !strings.Contains(c.DatabaseURL, "@db:") {
		problems = append(problems, "DATABASE_URL uses sslmode=disable to a remote database — use sslmode=require (or verify-full)")
	}
	if c.AccessTTL > 24*time.Hour {
		problems = append(problems, "ACCESS_TOKEN_TTL must be 24h or less")
	}
	if c.SessionIdle <= 0 || c.SessionIdle > 8*time.Hour {
		problems = append(problems, "SESSION_IDLE_TIMEOUT must be between 1m and 8h")
	}
	if len(problems) > 0 {
		return errors.New("refusing to start in production:\n  - " + strings.Join(problems, "\n  - "))
	}
	return nil
}

func get(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
