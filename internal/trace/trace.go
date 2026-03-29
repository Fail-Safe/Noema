package trace

import (
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

type Type string

const (
	TypeFact        Type = "fact"
	TypeDecision    Type = "decision"
	TypePreference  Type = "preference"
	TypeContext     Type = "context"
	TypeSkill       Type = "skill"
	TypeIntent      Type = "intent"
	TypeObservation Type = "observation"
	TypeNote        Type = "note"
)

var ValidTypes = []Type{
	TypeFact, TypeDecision, TypePreference, TypeContext,
	TypeSkill, TypeIntent, TypeObservation, TypeNote,
}

func IsValidType(t string) bool {
	for _, v := range ValidTypes {
		if string(v) == t {
			return true
		}
	}
	return false
}

type Frontmatter struct {
	ID      string   `yaml:"id"`
	Title   string   `yaml:"title"`
	Type    string   `yaml:"type"`
	Author  string   `yaml:"author,omitempty"`
	Tags    []string `yaml:"tags,omitempty"`
	Created string   `yaml:"created"`
	Updated string   `yaml:"updated"`
}

type Trace struct {
	Frontmatter
	Body string
}

func New(title, traceType, author string, tags []string, body string) *Trace {
	now := time.Now().UTC().Format(time.RFC3339)
	id := NewID(title)
	return &Trace{
		Frontmatter: Frontmatter{
			ID:      id,
			Title:   title,
			Type:    traceType,
			Author:  author,
			Tags:    tags,
			Created: now,
			Updated: now,
		},
		Body: body,
	}
}

func Parse(data []byte) (*Trace, error) {
	s := string(data)
	if !strings.HasPrefix(s, "---\n") {
		return nil, fmt.Errorf("missing frontmatter delimiter")
	}
	rest := s[4:]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return nil, fmt.Errorf("unterminated frontmatter")
	}
	fmStr := rest[:end]
	body := strings.TrimPrefix(rest[end+5:], "\n")

	var fm Frontmatter
	if err := yaml.Unmarshal([]byte(fmStr), &fm); err != nil {
		return nil, fmt.Errorf("parsing frontmatter: %w", err)
	}
	return &Trace{Frontmatter: fm, Body: body}, nil
}

func ParseFile(path string) (*Trace, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

func (t *Trace) Write(path string) error {
	t.Frontmatter.Updated = time.Now().UTC().Format(time.RFC3339)
	fmData, err := yaml.Marshal(t.Frontmatter)
	if err != nil {
		return fmt.Errorf("encoding frontmatter: %w", err)
	}
	content := fmt.Sprintf("---\n%s---\n\n%s", string(fmData), t.Body)
	return os.WriteFile(path, []byte(content), 0o640)
}

// NewID generates a trace ID from a title: YYYYMMDD-slugified-title.
func NewID(title string) string {
	date := time.Now().UTC().Format("20060102")
	s := slug(title)
	if len(s) > 60 {
		s = strings.TrimRight(s[:60], "-")
	}
	return date + "-" + s
}

func slug(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	prevHyphen := true
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			prevHyphen = false
		} else if !prevHyphen {
			b.WriteByte('-')
			prevHyphen = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}
