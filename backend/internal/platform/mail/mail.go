// Package mail sends transactional email (invitations, password resets, and
// later quotations and invoices) and keeps a send log.
//
// With SMTP_HOST unset, messages are written to the server log instead of
// being sent, so development works without a mail server.
package mail

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Message is one email. Kind names the template (e.g. "user_invite") for the send log.
type Message struct {
	To      string
	ToName  string
	Cc      []string // also receive a copy, e.g. the member of staff sending a quotation
	ReplyTo string   // where replies go, when not the system mailbox
	Kind    string
	Subject string
	Text    string
	HTML    string
	// Attachments, e.g. a quotation PDF.
	Attachments []Attachment
}

type Attachment struct {
	Name        string
	ContentType string
	Data        []byte
}

// Config is the SMTP connection. Port 465 uses implicit TLS; other ports use STARTTLS when offered.
type Config struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	FromName string
}

// Service sends email and records every attempt in email_log.
type Service struct {
	cfg  Config
	pool *pgxpool.Pool
	log  *slog.Logger
}

func New(cfg Config, pool *pgxpool.Pool, log *slog.Logger) *Service {
	return &Service{cfg: cfg, pool: pool, log: log}
}

// Enabled reports whether real email is sent (SMTP configured).
func (s *Service) Enabled() bool { return s.cfg.Host != "" }

// Send delivers the message and logs the outcome. It never returns before the
// SMTP server accepts or rejects the message (bounded by a 20s timeout).
func (s *Service) Send(ctx context.Context, m Message) error {
	var err error
	if s.Enabled() {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		err = s.sendSMTP(ctx, m)
	} else {
		s.log.Info("email (SMTP not configured, not sent)", "to", m.To, "subject", m.Subject, "text", m.Text)
	}
	status, errText := "sent", ""
	if !s.Enabled() {
		status = "logged"
	}
	if err != nil {
		status, errText = "failed", err.Error()
		s.log.Error("email failed", "to", m.To, "kind", m.Kind, "err", err)
	}
	if s.pool != nil {
		_, lerr := s.pool.Exec(ctx, `INSERT INTO email_log (recipient, kind, subject, status, error)
			VALUES ($1,$2,$3,$4,NULLIF($5,''))`, m.To, m.Kind, m.Subject, status, errText)
		if lerr != nil {
			s.log.Error("email log insert failed", "err", lerr)
		}
	}
	return err
}

func (s *Service) sendSMTP(ctx context.Context, m Message) error {
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	d := net.Dialer{}
	if dl, ok := ctx.Deadline(); ok {
		d.Deadline = dl
	}
	tlsCfg := &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}

	var conn net.Conn
	var err error
	if s.cfg.Port == 465 {
		conn, err = tls.DialWithDialer(&d, "tcp", addr, tlsCfg)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp handshake: %w", err)
	}
	defer c.Close()

	if s.cfg.Port != 465 {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsCfg); err != nil {
				return fmt.Errorf("starttls: %w", err)
			}
		}
	}
	if s.cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := c.Mail(s.cfg.From); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}
	for _, r := range append([]string{m.To}, m.Cc...) {
		if err := c.Rcpt(r); err != nil {
			return fmt.Errorf("rcpt to %s: %w", r, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(s.build(m)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	return c.Quit()
}

// build renders the message: plain text + HTML alternatives, wrapped in
// multipart/mixed when there are attachments.
func (s *Service) build(m Message) []byte {
	boundary := randomHex(12)
	from := (&mail.Address{Name: s.cfg.FromName, Address: s.cfg.From}).String()
	to := (&mail.Address{Name: m.ToName, Address: m.To}).String()
	domain := s.cfg.From[strings.LastIndex(s.cfg.From, "@")+1:]

	var b strings.Builder
	h := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	h("From", from)
	h("To", to)
	if len(m.Cc) > 0 {
		h("Cc", strings.Join(m.Cc, ", "))
	}
	if m.ReplyTo != "" {
		h("Reply-To", m.ReplyTo)
	}
	h("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	h("Date", time.Now().Format(time.RFC1123Z))
	h("Message-ID", "<"+randomHex(16)+"@"+domain+">")
	h("MIME-Version", "1.0")
	mixed := ""
	if len(m.Attachments) > 0 {
		mixed = randomHex(12)
		h("Content-Type", `multipart/mixed; boundary="`+mixed+`"`)
		b.WriteString("\r\n--" + mixed + "\r\n")
	}
	h("Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
	b.WriteString("\r\n")
	part := func(ctype, body string) {
		b.WriteString("--" + boundary + "\r\n")
		b.WriteString("Content-Type: " + ctype + "; charset=utf-8\r\n")
		b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
		qw := quotedprintable.NewWriter(&b)
		_, _ = qw.Write([]byte(body))
		_ = qw.Close()
		b.WriteString("\r\n")
	}
	part("text/plain", m.Text)
	if m.HTML != "" {
		part("text/html", m.HTML)
	}
	b.WriteString("--" + boundary + "--\r\n")
	for _, a := range m.Attachments {
		ctype := a.ContentType
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		name := mime.QEncoding.Encode("utf-8", a.Name)
		b.WriteString("\r\n--" + mixed + "\r\n")
		b.WriteString("Content-Type: " + ctype + `; name="` + name + "\"\r\n")
		b.WriteString(`Content-Disposition: attachment; filename="` + name + "\"\r\n")
		b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
		enc := base64.StdEncoding.EncodeToString(a.Data)
		for len(enc) > 76 {
			b.WriteString(enc[:76] + "\r\n")
			enc = enc[76:]
		}
		b.WriteString(enc + "\r\n")
	}
	if mixed != "" {
		b.WriteString("--" + mixed + "--\r\n")
	}
	return []byte(b.String())
}

func randomHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// ErrNotConfigured is returned by callers that require real delivery.
var ErrNotConfigured = errors.New("email is not configured")
