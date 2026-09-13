package models

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// ipFioRe matches the "ИП <ФАМИЛИЯ> <ИМЯ> <ОТЧЕСТВО>" shape that
// individual entrepreneurs' names come back as from a УТМ (e.g.
// "ИП ИВАНОВ ПЁТР СЕРГЕЕВИЧ" or the unabbreviated
// "ИНДИВИДУАЛЬНЫЙ ПРЕДПРИНИМАТЕЛЬ ...").
var ipFioRe = regexp.MustCompile(`(?i)^(?:ип|индивидуальный предприниматель)\s+`)

// PrettyOrgName shortens a full-caps individual entrepreneur name for
// display — "ИП ИВАНОВ ПЁТР СЕРГЕЕВИЧ" becomes "ИП Иванов П.С." — leaving
// anything else (legal entities, unrecognized shapes) unchanged. This is
// display-only: the stored/raw name is never modified by it.
func PrettyOrgName(name string) string {
	name = strings.TrimSpace(name)
	loc := ipFioRe.FindStringIndex(name)
	if loc == nil {
		return name
	}

	words := strings.Fields(name[loc[1]:])
	if len(words) != 3 {
		return name
	}
	surname, first, patronymic := words[0], words[1], words[2]
	firstInitial, ok1 := firstRune(first)
	patrInitial, ok2 := firstRune(patronymic)
	if !ok1 || !ok2 {
		return name
	}

	return fmt.Sprintf("ИП %s %c.%c.", titleCaseWord(surname), unicode.ToUpper(firstInitial), unicode.ToUpper(patrInitial))
}

func firstRune(s string) (rune, bool) {
	for _, r := range s {
		return r, true
	}
	return 0, false
}

// titleCaseWord capitalizes the first letter of each hyphen-separated part
// of a word (so "ИВАНОВ-ПЕТРОВ" becomes "Иванов-Петров"), lowercasing the
// rest — plain strings.Title-style casing doesn't handle Cyrillic hyphenated
// surnames on its own.
func titleCaseWord(s string) string {
	parts := strings.Split(s, "-")
	for i, p := range parts {
		if p == "" {
			continue
		}
		r := []rune(strings.ToLower(p))
		r[0] = unicode.ToUpper(r[0])
		parts[i] = string(r)
	}
	return strings.Join(parts, "-")
}
