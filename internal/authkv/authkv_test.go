package authkv

import "testing"

func TestParseHeaderForms(t *testing.T) {
	h, err := Parse([]string{
		"X-SPOPacToken: v1.eyJabc",
		"User-Agent=MyAgent",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Get("X-SPOPacToken"); got != "v1.eyJabc" {
		t.Errorf("X-SPOPacToken = %q", got)
	}
	if got := h.Get("User-Agent"); got != "MyAgent" {
		t.Errorf("User-Agent = %q", got)
	}
}

func TestParseHeaderColonValueWithEquals(t *testing.T) {
	h, err := Parse([]string{"Authorization: Bearer a=b=c"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Get("Authorization"); got != "Bearer a=b=c" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestParseHeaderInvalid(t *testing.T) {
	if _, err := Parse([]string{"garbage"}, nil); err == nil {
		t.Error("expected error for spec without separator")
	}
	if _, err := Parse([]string{": novalue"}, nil); err == nil {
		t.Error("expected error for empty header name")
	}
}

func TestParseCookieForms(t *testing.T) {
	h, err := Parse(nil, []string{
		"FedAuth=abc123",
		"rtFa=x; other=y",
		"Cookie: last=z",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := h.Get("Cookie")
	want := "FedAuth=abc123; rtFa=x; other=y; last=z"
	if got != want {
		t.Errorf("Cookie = %q, want %q", got, want)
	}
}

func TestParseCookieInvalid(t *testing.T) {
	if _, err := Parse(nil, []string{"novalue"}); err == nil {
		t.Error("expected error for cookie without =")
	}
}

func TestParseEmpty(t *testing.T) {
	h, err := Parse(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 0 {
		t.Errorf("expected empty header, got %v", h)
	}
}
