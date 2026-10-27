// Package authkv parses direct header/cookie key-value pairs supplied on the
// command line as an alternative to a HAR capture.
//
// Accepted forms:
//
//	--header "X-SPOPacToken: v1.eyJ..."   (Name: Value)
//	--header "X-SPOPacToken=v1.eyJ..."    (Name=Value)
//	--cookie "FedAuth=abc; rtFa=def"      (cookie pairs)
//	--cookie "Cookie: FedAuth=abc"        (full Cookie header)
package authkv

import (
	"fmt"
	"net/http"
	"strings"
)

// HeaderFlags collects repeatable --header values.
type HeaderFlags []string

func (h *HeaderFlags) String() string { return strings.Join(*h, ", ") }

// Set implements flag.Value.
func (h *HeaderFlags) Set(v string) error {
	*h = append(*h, v)
	return nil
}

// CookieFlags collects repeatable --cookie values.
type CookieFlags []string

func (c *CookieFlags) String() string { return strings.Join(*c, "; ") }

// Set implements flag.Value.
func (c *CookieFlags) Set(v string) error {
	*c = append(*c, v)
	return nil
}

// Parse builds an http.Header from raw header and cookie key-value specs.
// Header specs may use "Name: Value" or "Name=Value"; cookie specs may be
// "k=v", "k=v; k2=v2" or a full "Cookie: k=v" header.
func Parse(headerSpecs []string, cookieSpecs []string) (http.Header, error) {
	out := http.Header{}

	for _, spec := range headerSpecs {
		name, value, err := splitKV(spec, true)
		if err != nil {
			return nil, fmt.Errorf("header %q: %w", spec, err)
		}
		out.Add(http.CanonicalHeaderKey(name), value)
	}

	var cookies []string
	for _, spec := range cookieSpecs {
		s := strings.TrimSpace(spec)
		if strings.HasPrefix(strings.ToLower(s), "cookie:") {
			s = strings.TrimSpace(s[len("cookie:"):])
		}
		// Validate each pair loosely: name=value, value may contain '='.
		for _, pair := range strings.Split(s, ";") {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			if _, _, err := splitKV(pair, false); err != nil {
				return nil, fmt.Errorf("cookie %q: %w", spec, err)
			}
		}
		cookies = append(cookies, s)
	}
	if len(cookies) > 0 {
		out.Add("Cookie", strings.Join(cookies, "; "))
	}

	return out, nil
}

// splitKV splits one key-value spec. When allowColon is set, "Name: Value" is
// accepted in addition to "Name=Value".
func splitKV(spec string, allowColon bool) (string, string, error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return "", "", fmt.Errorf("empty key=value spec")
	}
	if allowColon {
		if i := strings.Index(s, ":"); i >= 0 && !strings.Contains(s[:i], "=") {
			name := strings.TrimSpace(s[:i])
			value := strings.TrimSpace(s[i+1:])
			if name == "" {
				return "", "", fmt.Errorf("missing header name")
			}
			return name, value, nil
		}
	}
	i := strings.Index(s, "=")
	if i <= 0 {
		return "", "", fmt.Errorf("expected key=value (or \"Name: Value\")")
	}
	name := strings.TrimSpace(s[:i])
	value := strings.TrimSpace(s[i+1:])
	if name == "" {
		return "", "", fmt.Errorf("missing key")
	}
	return name, value, nil
}
