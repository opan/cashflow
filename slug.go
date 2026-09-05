package main

import (
	"errors"
	"regexp"
	"strings"
)

var slugRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// normalizeSlug lowercases and converts spaces/underscores to single hyphens,
// stripping anything that isn't [a-z0-9-]. "Uang Kas 3A!" -> "uang-kas-3a".
func normalizeSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastHyphen := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastHyphen = false
		case r == ' ' || r == '-' || r == '_':
			if !lastHyphen && b.Len() > 0 {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func validateSlug(s string) error {
	switch {
	case len(s) < 3:
		return errors.New("minimal 3 karakter")
	case len(s) > 40:
		return errors.New("maksimal 40 karakter")
	case !slugRe.MatchString(s):
		return errors.New("hanya boleh huruf kecil, angka, dan tanda hubung")
	}
	return nil
}
