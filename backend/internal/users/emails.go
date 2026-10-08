package users

import (
	"context"
	"net/url"
	"strings"

	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/mail"
)

var roleNames = map[auth.Role]string{
	auth.RoleAdmin: "Administrator", auth.RoleCEO: "CEO", auth.RoleSalesManager: "Sales manager",
	auth.RoleSalesExecutive: "Sales executive", auth.RoleOperations: "Operations",
	auth.RoleTechnical: "Technical", auth.RoleFinance: "Finance",
}

func firstName(full string) string {
	if f := strings.Fields(full); len(f) > 0 {
		return f[0]
	}
	return full
}

// setupURL is the link a person opens to choose their password.
func (m *Module) setupURL(token string) string {
	return m.appURL + "/set-password?token=" + url.QueryEscape(token)
}

// delivery reports what happened to an email, so the admin knows whether to
// pass the link on themselves.
type delivery struct {
	EmailSent  bool   `json:"email_sent"`
	EmailError string `json:"email_error,omitempty"`
	// SetupLink is returned only when the email did not reach the mail server
	// (or email isn't configured), so the administrator can share it another way.
	SetupLink string `json:"setup_link,omitempty"`
}

func (m *Module) deliver(ctx context.Context, msg mail.Message, link string) delivery {
	err := m.mail.Send(ctx, msg)
	if err == nil && m.mail.Enabled() {
		return delivery{EmailSent: true}
	}
	d := delivery{SetupLink: link}
	if err != nil {
		d.EmailError = "The email couldn't be sent. Share the link below with them another way."
	} else {
		d.EmailError = "Email isn't set up on this server yet, so nothing was sent. Share the link below with them."
	}
	return d
}

func (m *Module) sendInvite(ctx context.Context, u User, token, invitedBy string) delivery {
	link := m.setupURL(token)
	html, text := mail.Render(mail.Content{
		Preheader: "Your DOS BPA account is ready. Set your password to get started.",
		Heading:   "Welcome to DOS BPA, " + firstName(u.FullName),
		Paragraphs: []string{
			invitedBy + " has created a DOS BPA account for you. It's where DOS keeps every client, quotation, order and invoice in one place.",
			"To get started, choose your own password. The link below works once and expires in 3 days.",
		},
		Details:    [][2]string{{"Sign-in email", u.Email}, {"Role", roleNames[u.Role]}, {"Position", u.Position}},
		ButtonText: "Set up my account",
		ButtonURL:  link,
		Footnote: "After that, sign in any time at " + m.appURL + "/login. If you weren't expecting this email, " +
			"you can ignore it and nothing will happen.",
	})
	return m.deliver(ctx, mail.Message{
		To: u.Email, ToName: u.FullName, Kind: "user_invite",
		Subject: "Your DOS BPA account is ready", HTML: html, Text: text,
	}, link)
}

func (m *Module) sendReset(ctx context.Context, u User, token, resetBy string) delivery {
	link := m.setupURL(token)
	html, text := mail.Render(mail.Content{
		Preheader: "Choose a new password for DOS BPA.",
		Heading:   "Choose a new password",
		Paragraphs: []string{
			"Hi " + firstName(u.FullName) + ", " + resetBy + " has reset your DOS BPA password. Your old password no longer works, and you've been signed out on all devices.",
			"Use the button below to choose a new one. The link works once and expires in 24 hours.",
		},
		ButtonText: "Choose a new password",
		ButtonURL:  link,
		Footnote:   "If you didn't expect this, tell your administrator straight away.",
	})
	return m.deliver(ctx, mail.Message{
		To: u.Email, ToName: u.FullName, Kind: "password_reset",
		Subject: "Reset your DOS BPA password", HTML: html, Text: text,
	}, link)
}
