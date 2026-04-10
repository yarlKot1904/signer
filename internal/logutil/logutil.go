package logutil

import "strings"

func MaskEmail(email string) string {
	email = strings.TrimSpace(email)
	if email == "" {
		return ""
	}

	at := strings.Index(email, "@")
	if at <= 0 || at == len(email)-1 {
		return maskMiddle(email, 1, 1)
	}

	local := email[:at]
	domain := email[at+1:]
	return maskMiddle(local, 1, 0) + "@" + maskDomain(domain)
}

func MaskToken(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	return maskMiddle(token, 6, 4)
}

func maskDomain(domain string) string {
	if domain == "" {
		return ""
	}

	parts := strings.Split(domain, ".")
	for i, part := range parts {
		if part == "" {
			continue
		}
		if i == len(parts)-1 {
			parts[i] = part
			continue
		}
		parts[i] = maskMiddle(part, 1, 0)
	}
	return strings.Join(parts, ".")
}

func maskMiddle(value string, left, right int) string {
	runes := []rune(value)
	if len(runes) == 0 {
		return ""
	}
	if len(runes) <= left+right {
		if len(runes) == 1 {
			return "*"
		}
		return string(runes[:1]) + strings.Repeat("*", len(runes)-1)
	}
	return string(runes[:left]) + strings.Repeat("*", len(runes)-left-right) + string(runes[len(runes)-right:])
}
