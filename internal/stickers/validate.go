package stickers

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
)

var (
	semanticIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{2,47}$`)
	setNamePattern    = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)
)

// ValidationError lists the problems that made an input unusable. Issues is
// capped at MaxIssues; Total counts every problem. Stale marks a template
// that no longer matches the current Telegram set (re-export advised).
// Messages name set/sticker identities and rules only — never annotation
// text — so they are safe to log.
type ValidationError struct {
	Issues []string
	Total  int
	Stale  bool
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	if e.Stale {
		b.WriteString("sticker template is out of date")
	} else {
		b.WriteString("invalid sticker data")
	}
	fmt.Fprintf(&b, " (%d problem(s))", e.Total)
	for _, issue := range e.Issues {
		b.WriteString("; ")
		b.WriteString(issue)
	}
	if e.Total > len(e.Issues) {
		fmt.Fprintf(&b, "; and %d more", e.Total-len(e.Issues))
	}

	return b.String()
}

// issueList collects bounded problem lists.
type issueList struct {
	items []string
	total int
}

func (l *issueList) addf(format string, args ...any) {
	l.total++
	if len(l.items) < MaxIssues {
		l.items = append(l.items, fmt.Sprintf(format, args...))
	}
}

func (l *issueList) err(stale bool) error {
	if l.total == 0 {
		return nil
	}

	return &ValidationError{Issues: l.items, Total: l.total, Stale: stale}
}

// ValidateSemanticID checks the semantic ID shape.
func ValidateSemanticID(id string) error {
	if !semanticIDPattern.MatchString(id) {
		return errors.New("semantic id must match ^[a-z][a-z0-9_]{2,47}$")
	}

	return nil
}

// ValidateDescription checks a description: single line, no control or
// separator characters, bounded length.
func ValidateDescription(d string) error {
	if utf8.RuneCountInString(d) > MaxDescriptionRunes {
		return fmt.Errorf("description is longer than %d characters", MaxDescriptionRunes)
	}
	if !cleanText(d) {
		return errors.New("description must be a single line without control characters")
	}

	return nil
}

// ValidateTags checks a tag list: bounded count and length, no empty tags,
// no control characters, no duplicates (case-insensitive).
func ValidateTags(tags []string) error {
	if len(tags) > MaxTags {
		return fmt.Errorf("more than %d tags", MaxTags)
	}
	fold := cases.Fold()
	seen := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		if tag == "" {
			return errors.New("empty tag")
		}
		if utf8.RuneCountInString(tag) > MaxTagRunes {
			return fmt.Errorf("tag is longer than %d characters", MaxTagRunes)
		}
		if !cleanText(tag) {
			return errors.New("tag contains control characters")
		}
		key := fold.String(tag)
		if _, dup := seen[key]; dup {
			return errors.New("duplicate tag")
		}
		seen[key] = struct{}{}
	}

	return nil
}

// ValidateSetName checks a Telegram sticker set short name.
func ValidateSetName(name string) error {
	if !setNamePattern.MatchString(name) {
		return errors.New("sticker set names are 1-64 letters, digits or underscores")
	}

	return nil
}

// SanitizeTitle turns a Telegram-provided title into a bounded single line:
// control characters become spaces and the length is capped.
func SanitizeTitle(title string) string {
	title = strings.Map(func(r rune) rune {
		if !cleanRune(r) {
			return ' '
		}

		return r
	}, title)
	title = strings.Join(strings.Fields(title), " ")
	if runes := []rune(title); len(runes) > MaxTitleRunes {
		title = string(runes[:MaxTitleRunes])
	}

	return title
}

func cleanRune(r rune) bool {
	return !unicode.IsControl(r) && !unicode.Is(unicode.Zl, r) && !unicode.Is(unicode.Zp, r)
}

func cleanText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if !cleanRune(r) {
			return false
		}
	}

	return true
}

// ParseSetReference extracts a sticker set short name from a bare name, a
// t.me/addstickers/<name> URL (any scheme, optional query or trailing
// slash), or a tg://addstickers?set=<name> link.
func ParseSetReference(input string) (string, error) {
	ref := strings.TrimSpace(input)
	if ref == "" {
		return "", errors.New("send a sticker set name or a t.me/addstickers/<name> link")
	}

	name := ref
	lower := strings.ToLower(ref)
	switch {
	case strings.HasPrefix(lower, "tg://"):
		u, err := url.Parse(ref)
		if err != nil || !strings.EqualFold(u.Host, "addstickers") {
			return "", errors.New("only tg://addstickers?set=<name> links are supported")
		}
		name = u.Query().Get("set")
	case strings.Contains(ref, "/"):
		trimmed := ref
		for _, scheme := range []string{"https://", "http://"} {
			if strings.HasPrefix(lower, scheme) {
				trimmed = ref[len(scheme):]

				break
			}
		}
		trimmed = strings.SplitN(trimmed, "#", 2)[0]
		trimmed = strings.SplitN(trimmed, "?", 2)[0]
		trimmed = strings.TrimRight(trimmed, "/")
		host, path, _ := strings.Cut(trimmed, "/")
		host = strings.TrimPrefix(strings.ToLower(host), "www.")
		if host != "t.me" && host != "telegram.me" {
			return "", errors.New("only t.me/addstickers/<name> links are supported")
		}
		kind, rest, _ := strings.Cut(path, "/")
		switch {
		case strings.EqualFold(kind, "addemoji"):
			return "", errors.New("custom emoji sets are not supported; send a regular sticker set")
		case !strings.EqualFold(kind, "addstickers"):
			return "", errors.New("only t.me/addstickers/<name> links are supported")
		}
		name = rest
	}

	if err := ValidateSetName(name); err != nil {
		return "", err
	}

	return name, nil
}

// ValidateState checks a complete catalog against the global caps and row
// rules. It is used both for the prospective state of every update and for
// the startup restore, so persisted data is held to the same rules as
// imports. The error names identities and rules only, never annotation text.
func ValidateState(s State) error {
	var issues issueList

	if len(s.Sets) > MaxSets {
		issues.addf("more than %d sticker sets", MaxSets)
	}
	if len(s.Entries) > MaxTotalEntries {
		issues.addf("more than %d catalog entries", MaxTotalEntries)
	}

	sets := make(map[string]struct{}, len(s.Sets))
	for _, set := range s.Sets {
		if _, dup := sets[set.Name]; dup {
			issues.addf("set %q is listed twice", set.Name)
		}
		sets[set.Name] = struct{}{}
		if err := ValidateSetName(set.Name); err != nil {
			issues.addf("set %q: %v", set.Name, err)
		}
		if utf8.RuneCountInString(set.Title) > MaxTitleRunes || !cleanText(set.Title) {
			issues.addf("set %q: title is too long or has control characters", set.Name)
		}
	}

	type key struct{ set, unique string }
	type perSet struct{ available, missing int }
	seen := make(map[key]struct{}, len(s.Entries))
	counts := map[string]*perSet{}
	holders := make(map[string]key, len(s.Entries))

	for _, e := range s.Entries {
		where := fmt.Sprintf("%s/%s", e.SetName, e.FileUniqueID)
		if _, ok := sets[e.SetName]; !ok {
			issues.addf("sticker %s belongs to no known set", where)
		}
		k := key{e.SetName, e.FileUniqueID}
		if _, dup := seen[k]; dup {
			issues.addf("sticker %s is listed twice", where)
		}
		seen[k] = struct{}{}

		c := counts[e.SetName]
		if c == nil {
			c = &perSet{}
			counts[e.SetName] = c
		}
		if e.Available {
			c.available++
		} else {
			c.missing++
		}

		if e.FileUniqueID == "" || len(e.FileUniqueID) > MaxTelegramIDBytes {
			issues.addf("sticker %s: file_unique_id is empty or too long", where)
		}
		if e.FileID == "" || len(e.FileID) > MaxTelegramIDBytes {
			issues.addf("sticker %s: file_id is empty or too long", where)
		}
		if !e.Format.Valid() {
			issues.addf("sticker %s: unknown format", where)
		}
		if e.Position < 0 {
			issues.addf("sticker %s: negative position", where)
		}
		if utf8.RuneCountInString(e.Emoji) > MaxEmojiRunes || !cleanText(e.Emoji) {
			issues.addf("sticker %s: emoji is too long or has control characters", where)
		}
		if e.SemanticID != "" {
			if err := ValidateSemanticID(e.SemanticID); err != nil {
				issues.addf("sticker %s: %v", where, err)
			}
			if prev, dup := holders[e.SemanticID]; dup {
				issues.addf("semantic id %q is used by both %s/%s and %s", e.SemanticID, prev.set, prev.unique, where)
			} else {
				holders[e.SemanticID] = k
			}
		}
		if err := ValidateDescription(e.Description); err != nil {
			issues.addf("sticker %s: %v", where, err)
		}
		if err := ValidateTags(e.Tags); err != nil {
			issues.addf("sticker %s: %v", where, err)
		}
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic issue order
	for _, name := range names {
		c := counts[name]
		if c.available > MaxStickersPerSet {
			issues.addf("set %q has more than %d stickers", name, MaxStickersPerSet)
		}
		if c.missing > MaxMissingPerSet {
			issues.addf("set %q retains more than %d missing stickers; forget some", name, MaxMissingPerSet)
		}
	}

	return issues.err(false)
}
