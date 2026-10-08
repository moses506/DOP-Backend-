package mail

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
)

func TestBuildWithAttachment(t *testing.T) {
	s := New(Config{From: "workflow@dos.test", FromName: "DOS BPA"}, nil, nil)
	raw := s.build(Message{To: "client@x.test", ToName: "Client", Cc: []string{"me@dos.test"}, ReplyTo: "me@dos.test",
		Subject: "Quotation DOS-Q-2026-0001", Text: "Hello", HTML: "<p>Hello</p>",
		Attachments: []Attachment{{Name: "DOS-Q-2026-0001.pdf", ContentType: "application/pdf", Data: bytes.Repeat([]byte("%PDF"), 100)}}})
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if m.Header.Get("Cc") != "me@dos.test" || m.Header.Get("Reply-To") != "me@dos.test" {
		t.Fatalf("headers: %v", m.Header)
	}
	mt, params, _ := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if mt != "multipart/mixed" {
		t.Fatalf("type %s", mt)
	}
	r := multipart.NewReader(m.Body, params["boundary"])
	var kinds []string
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, strings.Split(p.Header.Get("Content-Type"), ";")[0])
		if p.FileName() == "DOS-Q-2026-0001.pdf" {
			b, _ := io.ReadAll(p)
			if len(b) == 0 {
				t.Fatal("empty attachment")
			}
		}
	}
	if strings.Join(kinds, ",") != "multipart/alternative,application/pdf" {
		t.Fatalf("parts %v", kinds)
	}
}
