package stickers

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// SchemaVersion is the only template schema version this build reads and
// writes.
const SchemaVersion = 1

// Template is the administrator-edited YAML document of one sticker set.
// Only the editable fields (id, description, tags, enabled, reset, forget)
// carry meaning on import; position, emoji, format and title are display
// aids and are ignored. file_id never appears: identity is the Telegram
// file_unique_id and the send token always comes from Telegram.
type Template struct {
	SchemaVersion int               `yaml:"schema_version"`
	Set           string            `yaml:"set"`
	Title         string            `yaml:"title,omitempty"`
	Stickers      []TemplateSticker `yaml:"stickers"`
	Missing       []TemplateMissing `yaml:"missing,omitempty"`
}

// TemplateSticker is one sticker currently in the Telegram set.
//
// Enabled and Reset are pointers so an omitted field is distinguishable from
// an explicit false: omitting enabled keeps the stored value, and only an
// explicit false disables.
type TemplateSticker struct {
	FileUniqueID string   `yaml:"file_unique_id"`
	Position     int      `yaml:"position,omitempty"`
	Emoji        string   `yaml:"emoji,omitempty"`
	Format       string   `yaml:"format,omitempty"`
	ID           string   `yaml:"id"`
	Description  string   `yaml:"description"`
	Tags         []string `yaml:"tags"`
	Enabled      *bool    `yaml:"enabled,omitempty"`
	Reset        *bool    `yaml:"reset,omitempty"`
}

// TemplateMissing is a retained entry that Telegram no longer reports. Its
// annotations are shown for reference; the only editable field is Forget.
type TemplateMissing struct {
	FileUniqueID string   `yaml:"file_unique_id"`
	ID           string   `yaml:"id"`
	Description  string   `yaml:"description"`
	Tags         []string `yaml:"tags"`
	Forget       *bool    `yaml:"forget,omitempty"`
}

func flag(b *bool) bool { return b != nil && *b }

// TemplateFilename names the exported document of a set.
func TemplateFilename(set string) string { return "stickers-" + set + ".yaml" }

// BuildTemplate renders the export template for a set. It is a pure function
// of the current Telegram set and the saved catalog: saved annotations are
// matched by (set, file_unique_id) irrespective of order and prefilled,
// newly seen stickers are blank, and retained entries Telegram no longer
// reports are listed in a separate "missing" section. Output is
// deterministic.
func BuildTemplate(stored State, remote RemoteSet) ([]byte, error) {
	if err := ValidateSetName(remote.Name); err != nil {
		return nil, err
	}
	if len(remote.Stickers) > MaxStickersPerSet {
		return nil, fmt.Errorf("sticker set has more than %d stickers", MaxStickersPerSet)
	}

	savedByID := map[string]Entry{}
	for _, e := range stored.EntriesOf(remote.Name) {
		savedByID[e.FileUniqueID] = e
	}

	doc := Template{SchemaVersion: SchemaVersion, Set: remote.Name, Title: SanitizeTitle(remote.Title)}
	inRemote := make(map[string]struct{}, len(remote.Stickers))
	for i, rs := range remote.Stickers {
		if rs.FileUniqueID == "" {
			return nil, errors.New("telegram returned a sticker without file_unique_id")
		}
		if _, dup := inRemote[rs.FileUniqueID]; dup {
			return nil, errors.New("telegram returned the same sticker twice; identity is ambiguous")
		}
		inRemote[rs.FileUniqueID] = struct{}{}

		entry := TemplateSticker{
			FileUniqueID: rs.FileUniqueID, Position: i + 1, Emoji: rs.Emoji, Format: string(rs.Format),
			Tags: []string{}, Enabled: boolPtr(true), Reset: boolPtr(false),
		}
		if saved, ok := savedByID[rs.FileUniqueID]; ok {
			entry.ID, entry.Description = saved.SemanticID, saved.Description
			entry.Tags = append([]string{}, saved.Tags...)
			entry.Enabled = boolPtr(saved.Enabled)
		}
		doc.Stickers = append(doc.Stickers, entry)
	}

	var missing []TemplateMissing
	for unique, saved := range savedByID {
		if _, ok := inRemote[unique]; ok {
			continue
		}
		missing = append(missing, TemplateMissing{
			FileUniqueID: unique, ID: saved.SemanticID, Description: saved.Description,
			Tags: append([]string{}, saved.Tags...), Forget: boolPtr(false),
		})
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].FileUniqueID < missing[j].FileUniqueID })
	doc.Missing = missing

	var body bytes.Buffer
	enc := yaml.NewEncoder(&body)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("render sticker template: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("render sticker template: %w", err)
	}

	return append([]byte(templateHeader(remote.Name)), body.Bytes()...), nil
}

func boolPtr(b bool) *bool { return &b }

func templateHeader(set string) string {
	lines := []string{
		fmt.Sprintf("Sticker catalog template for the set %q. Edit id, description and tags, then upload this file back.", set),
		"- Identity is file_unique_id. position only helps you match entries to the set in Telegram.",
		"- Blank id/description/tags keep what is already saved; new stickers stay inactive until annotated.",
		"- A sticker is selectable only with an id and a description. Tags are optional (any language).",
		"- id: lowercase letters, digits and underscores, starting with a letter, 3-48 characters, unique across all sets.",
		"- enabled: false hides a sticker without erasing its annotations. Leaving the field out keeps the saved value.",
		"- reset: true clears this sticker's id, description and tags and sets enabled back to true.",
		"  It overrides every other field of the entry, so you only need to flip that one flag.",
		"- Entries under \"missing\" are no longer in Telegram and are kept for restoration.",
		"  Set forget: true on one to delete it for good and free its id.",
	}
	var b strings.Builder
	for _, line := range lines {
		b.WriteString("# ")
		b.WriteString(line)
		b.WriteString("\n")
	}

	return b.String()
}

// ParseTemplate decodes an uploaded template strictly: bounded size, valid
// UTF-8, a single document, no YAML aliases, no unknown fields, no duplicate
// keys, the supported schema version and a well-formed set name. Semantic
// checks against the current Telegram set happen in PlanImport.
func ParseTemplate(raw []byte) (*Template, error) {
	switch {
	case len(raw) == 0:
		return nil, errors.New("the file is empty")
	case len(raw) > MaxUploadBytes:
		return nil, fmt.Errorf("the file is larger than %d KiB", MaxUploadBytes>>10)
	case !utf8.Valid(raw):
		return nil, errors.New("the file is not valid UTF-8")
	}

	// Aliases are never produced by the export and invite amplification.
	var probe yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(raw)).Decode(&probe); err != nil {
		return nil, fmt.Errorf("invalid YAML: %s", shortError(err))
	}
	if hasAlias(&probe) {
		return nil, errors.New("YAML anchors and aliases are not supported")
	}

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var doc Template
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("invalid template: %s", shortError(err))
	}
	if err := dec.Decode(new(yaml.Node)); !errors.Is(err, io.EOF) {
		return nil, errors.New("the file must contain exactly one YAML document")
	}

	if doc.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("unsupported schema_version %d (this bot reads version %d)", doc.SchemaVersion, SchemaVersion)
	}
	if err := ValidateSetName(doc.Set); err != nil {
		return nil, fmt.Errorf("set: %w", err)
	}
	if len(doc.Stickers) > MaxStickersPerSet {
		return nil, fmt.Errorf("more than %d stickers in the file", MaxStickersPerSet)
	}
	if len(doc.Missing) > MaxMissingPerSet {
		return nil, fmt.Errorf("more than %d entries under missing", MaxMissingPerSet)
	}

	return &doc, nil
}

func hasAlias(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	if n.Kind == yaml.AliasNode || n.Alias != nil || n.Anchor != "" {
		return true
	}
	for _, c := range n.Content {
		if hasAlias(c) {
			return true
		}
	}

	return false
}

// shortError keeps decoder messages to one bounded line.
func shortError(err error) string {
	msg := strings.Join(strings.Fields(err.Error()), " ")
	msg = strings.TrimPrefix(msg, "yaml: ")
	if runes := []rune(msg); len(runes) > 200 {
		msg = string(runes[:200]) + "…"
	}

	return msg
}
